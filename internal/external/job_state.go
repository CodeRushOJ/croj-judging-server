package external

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type JobStatus string

const (
	JobStatusQueued    JobStatus = "QUEUED"
	JobStatusRunning   JobStatus = "RUNNING"
	JobStatusSucceeded JobStatus = "SUCCEEDED"
	JobStatusFailed    JobStatus = "FAILED"
	JobStatusCancelled JobStatus = "CANCELLED"
)

var (
	ErrJobNotClaimable = errors.New("judge job is not claimable")
	ErrStaleJobClaim   = errors.New("judge job claim is stale")
	ErrInvalidJobState = errors.New("judge job state is invalid")
)

type JobClaim struct {
	WorkerID  string
	AttemptNo uint32
}

type DurableCaseResult struct {
	CaseID      string `json:"caseId"`
	Verdict     string `json:"verdict"`
	TimeMillis  int64  `json:"timeMillis"`
	MemoryBytes int64  `json:"memoryBytes"`
	Score       *int   `json:"score,omitempty"`
	MaxScore    *int   `json:"maxScore,omitempty"`
}

type DurableJobResult struct {
	Verdict       string              `json:"verdict"`
	CompileStatus string              `json:"compileStatus"`
	TimeMillis    int64               `json:"timeMillis"`
	MemoryBytes   int64               `json:"memoryBytes"`
	Score         *int                `json:"score,omitempty"`
	TotalScore    *int                `json:"totalScore,omitempty"`
	Cases         []DurableCaseResult `json:"cases"`
}

type DurableJob struct {
	ExternalID        string
	Status            JobStatus
	AttemptNo         uint32
	WorkerID          string
	LeaseUntil        time.Time
	NextAttemptAt     time.Time
	CancelRequestedAt *time.Time
	Result            *DurableJobResult
	FailureCode       string
	StartedAt         *time.Time
	CompletedAt       *time.Time
}

func (job *DurableJob) Claim(workerID string, now time.Time, leaseDuration time.Duration) (JobClaim, error) {
	if job == nil || strings.TrimSpace(workerID) == "" || len(workerID) > 128 || leaseDuration <= 0 {
		return JobClaim{}, fmt.Errorf("%w: invalid claim arguments", ErrInvalidJobState)
	}
	claimable := job.Status == JobStatusQueued && !job.NextAttemptAt.After(now)
	claimable = claimable || job.Status == JobStatusRunning && !job.LeaseUntil.After(now)
	if !claimable || job.Status == JobStatusQueued && job.CancelRequestedAt != nil || job.AttemptNo == ^uint32(0) {
		return JobClaim{}, ErrJobNotClaimable
	}
	job.AttemptNo++
	job.Status = JobStatusRunning
	job.WorkerID = workerID
	job.LeaseUntil = now.Add(leaseDuration)
	job.NextAttemptAt = time.Time{}
	job.FailureCode = ""
	if job.StartedAt == nil {
		job.StartedAt = copyTime(now)
	}
	return JobClaim{WorkerID: workerID, AttemptNo: job.AttemptNo}, nil
}

func (job *DurableJob) Heartbeat(claim JobClaim, now time.Time, leaseDuration time.Duration) error {
	if leaseDuration <= 0 || !job.ownsActiveClaim(claim, now) {
		return ErrStaleJobClaim
	}
	job.LeaseUntil = now.Add(leaseDuration)
	return nil
}

func (job *DurableJob) RequestCancel(now time.Time) (bool, error) {
	if job == nil {
		return false, ErrInvalidJobState
	}
	switch job.Status {
	case JobStatusQueued:
		job.Status = JobStatusCancelled
		job.CancelRequestedAt = copyTime(now)
		job.CompletedAt = copyTime(now)
		job.clearLease()
		return true, nil
	case JobStatusRunning:
		if job.CancelRequestedAt != nil {
			return false, nil
		}
		job.CancelRequestedAt = copyTime(now)
		return true, nil
	case JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
		return false, nil
	default:
		return false, ErrInvalidJobState
	}
}

func (job *DurableJob) Complete(claim JobClaim, result DurableJobResult, now time.Time) error {
	if !job.ownsActiveClaim(claim, now) {
		return ErrStaleJobClaim
	}
	if job.CancelRequestedAt != nil {
		job.Status = JobStatusCancelled
		job.Result = nil
	} else {
		if err := validateDurableJobResult(result); err != nil {
			return fmt.Errorf("%w: terminal result is incomplete", ErrInvalidJobState)
		}
		copied := result
		copied.Score, copied.TotalScore = copyScore(result.Score), copyScore(result.TotalScore)
		copied.Cases = make([]DurableCaseResult, len(result.Cases))
		for index, item := range result.Cases {
			copied.Cases[index] = item
			copied.Cases[index].Score = copyScore(item.Score)
			copied.Cases[index].MaxScore = copyScore(item.MaxScore)
		}
		job.Status = JobStatusSucceeded
		job.Result = &copied
	}
	job.FailureCode = ""
	job.CompletedAt = copyTime(now)
	job.clearLease()
	return nil
}

func validateDurableJobResult(result DurableJobResult) error {
	if strings.TrimSpace(result.Verdict) == "" || len(result.Verdict) > 64 ||
		strings.TrimSpace(result.CompileStatus) == "" || len(result.CompileStatus) > 64 ||
		result.TimeMillis < 0 || result.MemoryBytes < 0 || len(result.Cases) > 256 {
		return ErrInvalidJobState
	}
	if !validScorePair(result.Score, result.TotalScore) {
		return ErrInvalidJobState
	}
	caseIDs := make(map[string]struct{}, len(result.Cases))
	var caseScoreSum, caseMaximumSum int64
	for _, item := range result.Cases {
		if strings.TrimSpace(item.CaseID) == "" || len(item.CaseID) > 128 ||
			strings.TrimSpace(item.Verdict) == "" || len(item.Verdict) > 64 ||
			item.TimeMillis < 0 || item.MemoryBytes < 0 {
			return ErrInvalidJobState
		}
		if !validScorePair(item.Score, item.MaxScore) {
			return ErrInvalidJobState
		}
		if result.Score == nil {
			if item.Score != nil {
				return ErrInvalidJobState
			}
		} else {
			if item.Score == nil ||
				(item.Verdict == "ACCEPTED" && *item.Score != *item.MaxScore) ||
				(item.Verdict != "ACCEPTED" && *item.Score != 0) {
				return ErrInvalidJobState
			}
			caseScoreSum += int64(*item.Score)
			caseMaximumSum += int64(*item.MaxScore)
		}
		if _, exists := caseIDs[item.CaseID]; exists {
			return ErrInvalidJobState
		}
		caseIDs[item.CaseID] = struct{}{}
	}
	if result.Score == nil {
		return nil
	}
	if result.CompileStatus == "FAILED" {
		if result.Verdict != "COMPILE_ERROR" || *result.Score != 0 || len(result.Cases) != 0 {
			return ErrInvalidJobState
		}
		return nil
	}
	if result.CompileStatus != "SUCCEEDED" ||
		caseScoreSum != int64(*result.Score) ||
		caseMaximumSum != int64(*result.TotalScore) {
		return ErrInvalidJobState
	}
	if (*result.Score == *result.TotalScore && result.Verdict != "ACCEPTED") ||
		(*result.Score < *result.TotalScore && result.Verdict != "WRONG_ANSWER") {
		return ErrInvalidJobState
	}
	return nil
}

// ValidateDurableJobResult exposes the persistence invariant to the canonical
// worker boundary so malformed scores cannot cross either layer.
func ValidateDurableJobResult(result DurableJobResult) error {
	return validateDurableJobResult(result)
}

func validScorePair(score, maximum *int) bool {
	if (score == nil) != (maximum == nil) {
		return false
	}
	return score == nil || *maximum > 0 && *maximum <= 1_000_000_000 &&
		*score >= 0 && *score <= *maximum
}

func copyScore(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func (job *DurableJob) FailInfrastructure(
	claim JobClaim,
	failureCode string,
	maximumAttempts uint32,
	now time.Time,
	retryDelay time.Duration,
) (bool, error) {
	if !job.ownsActiveClaim(claim, now) {
		return false, ErrStaleJobClaim
	}
	if strings.TrimSpace(failureCode) == "" || len(failureCode) > 64 || maximumAttempts == 0 || retryDelay < 0 {
		return false, fmt.Errorf("%w: invalid infrastructure failure", ErrInvalidJobState)
	}
	if job.CancelRequestedAt != nil {
		job.Status = JobStatusCancelled
		job.CompletedAt = copyTime(now)
		job.clearLease()
		return true, nil
	}
	if job.AttemptNo < maximumAttempts {
		job.Status = JobStatusQueued
		job.NextAttemptAt = now.Add(retryDelay)
		job.clearLease()
		return false, nil
	}
	job.Status = JobStatusFailed
	job.FailureCode = failureCode
	job.CompletedAt = copyTime(now)
	job.clearLease()
	return true, nil
}

func (job *DurableJob) ownsActiveClaim(claim JobClaim, now time.Time) bool {
	return job != nil && job.Status == JobStatusRunning && claim.AttemptNo > 0 &&
		job.AttemptNo == claim.AttemptNo && job.WorkerID == claim.WorkerID &&
		claim.WorkerID != "" && job.LeaseUntil.After(now)
}

func (job *DurableJob) clearLease() {
	job.WorkerID = ""
	job.LeaseUntil = time.Time{}
}

func copyTime(value time.Time) *time.Time {
	copied := value
	return &copied
}
