package external

import (
	"errors"
	"testing"
	"time"
)

func TestDurableJobClaimAndHeartbeatUseAttemptCAS(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	job := DurableJob{ExternalID: "ceirceirceirceirceirceirce", Status: JobStatusQueued, NextAttemptAt: now}
	claim, err := job.Claim("worker-a", now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claim.AttemptNo != 1 || job.Status != JobStatusRunning || job.WorkerID != "worker-a" || !job.LeaseUntil.Equal(now.Add(30*time.Second)) {
		t.Fatalf("unexpected claim=%+v job=%+v", claim, job)
	}
	if err := job.Heartbeat(JobClaim{WorkerID: "worker-b", AttemptNo: 1}, now.Add(time.Second), 30*time.Second); !errors.Is(err, ErrStaleJobClaim) {
		t.Fatalf("wrong worker heartbeat error=%v", err)
	}
	if err := job.Heartbeat(claim, now.Add(10*time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !job.LeaseUntil.Equal(now.Add(40 * time.Second)) {
		t.Fatalf("lease=%s", job.LeaseUntil)
	}
}

func TestExpiredLeaseCanBeReclaimedExactlyOnce(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	job := DurableJob{
		ExternalID: "ceirceirceirceirceirceirce", Status: JobStatusRunning,
		AttemptNo: 1, WorkerID: "dead-worker", LeaseUntil: now.Add(-time.Second),
	}
	claim, err := job.Claim("worker-b", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.AttemptNo != 2 || job.AttemptNo != 2 {
		t.Fatalf("claim=%+v job=%+v", claim, job)
	}
	if _, err := job.Claim("worker-c", now, time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("active lease was reclaimed: %v", err)
	}
}

func TestExpiredCancelledLeaseIsReclaimedToReachTerminalState(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	job := DurableJob{
		Status: JobStatusRunning, AttemptNo: 1, WorkerID: "dead-worker",
		LeaseUntil: now.Add(-time.Second), CancelRequestedAt: timePointer(now.Add(-time.Minute)),
	}
	claim, err := job.Claim("recovery-worker", now, time.Minute)
	if err != nil {
		t.Fatalf("cancelled expired lease became permanently stuck: %v", err)
	}
	if err := job.Complete(claim, DurableJobResult{Verdict: "IGNORED", CompileStatus: "IGNORED"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if job.Status != JobStatusCancelled || job.CompletedAt == nil || job.Result != nil {
		t.Fatalf("recovered cancellation=%+v", job)
	}
}

func TestCancellationIsIdempotentAcrossQueuedRunningAndTerminalJobs(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	queued := DurableJob{Status: JobStatusQueued}
	if changed, err := queued.RequestCancel(now); err != nil || !changed || queued.Status != JobStatusCancelled || queued.CompletedAt == nil {
		t.Fatalf("queued cancellation changed=%v err=%v job=%+v", changed, err, queued)
	}
	if changed, err := queued.RequestCancel(now.Add(time.Second)); err != nil || changed {
		t.Fatalf("terminal cancellation changed=%v err=%v", changed, err)
	}

	running := DurableJob{Status: JobStatusRunning, AttemptNo: 3, WorkerID: "worker-a", LeaseUntil: now.Add(time.Minute)}
	if changed, err := running.RequestCancel(now); err != nil || !changed || running.CancelRequestedAt == nil || running.Status != JobStatusRunning {
		t.Fatalf("running cancellation changed=%v err=%v job=%+v", changed, err, running)
	}
	if changed, err := running.RequestCancel(now.Add(time.Second)); err != nil || changed {
		t.Fatalf("duplicate cancellation changed=%v err=%v", changed, err)
	}
}

func TestCompletionRejectsStaleWorkersAndHonorsCancellation(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	job := DurableJob{Status: JobStatusRunning, AttemptNo: 2, WorkerID: "worker-b", LeaseUntil: now.Add(time.Minute)}
	result := DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}
	if err := job.Complete(JobClaim{WorkerID: "worker-a", AttemptNo: 1}, result, now); !errors.Is(err, ErrStaleJobClaim) {
		t.Fatalf("stale completion error=%v", err)
	}
	job.CancelRequestedAt = timePointer(now.Add(-time.Second))
	if err := job.Complete(JobClaim{WorkerID: "worker-b", AttemptNo: 2}, result, now); err != nil {
		t.Fatal(err)
	}
	if job.Status != JobStatusCancelled || job.Result != nil || job.WorkerID != "" || !job.LeaseUntil.IsZero() {
		t.Fatalf("cancelled completion leaked result or lease: %+v", job)
	}
}

func TestCompletionRejectsMalformedRedactedResults(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	for name, result := range map[string]DurableJobResult{
		"negative aggregate": {Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: -1},
		"negative case":      {Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{{CaseID: "1", Verdict: "ACCEPTED", MemoryBytes: -1}}},
		"empty case id":      {Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{{Verdict: "ACCEPTED"}}},
		"duplicate case":     {Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{{CaseID: "1", Verdict: "ACCEPTED"}, {CaseID: "1", Verdict: "WRONG_ANSWER"}}},
	} {
		t.Run(name, func(t *testing.T) {
			job := DurableJob{Status: JobStatusRunning, AttemptNo: 1, WorkerID: "worker-a", LeaseUntil: now.Add(time.Minute)}
			err := job.Complete(JobClaim{WorkerID: "worker-a", AttemptNo: 1}, result, now)
			if !errors.Is(err, ErrInvalidJobState) || job.Status != JobStatusRunning || job.Result != nil {
				t.Fatalf("err=%v job=%+v", err, job)
			}
		})
	}
}

func TestInfrastructureFailureRetriesThenTerminates(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	job := DurableJob{Status: JobStatusRunning, AttemptNo: 1, WorkerID: "worker-a", LeaseUntil: now.Add(time.Minute)}
	claim := JobClaim{WorkerID: "worker-a", AttemptNo: 1}
	if terminal, err := job.FailInfrastructure(claim, "SANDBOX_UNAVAILABLE", 3, now, 5*time.Second); err != nil || terminal {
		t.Fatalf("first failure terminal=%v err=%v", terminal, err)
	}
	if job.Status != JobStatusQueued || !job.NextAttemptAt.Equal(now.Add(5*time.Second)) || job.FailureCode != "" {
		t.Fatalf("retry state=%+v", job)
	}
	second, err := job.Claim("worker-b", now.Add(5*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if terminal, err := job.FailInfrastructure(second, "SANDBOX_UNAVAILABLE", 2, now.Add(6*time.Second), 5*time.Second); err != nil || !terminal {
		t.Fatalf("terminal failure terminal=%v err=%v", terminal, err)
	}
	if job.Status != JobStatusFailed || job.FailureCode != "SANDBOX_UNAVAILABLE" || job.CompletedAt == nil {
		t.Fatalf("terminal state=%+v", job)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
