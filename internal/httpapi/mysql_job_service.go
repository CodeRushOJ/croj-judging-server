package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type durableJobRepository interface {
	Submit(context.Context, string, string, external.JudgeJobRequest) (external.SubmitJobResult, error)
	List(context.Context, string, external.JobListOptions) (external.JobListResult, error)
	Get(context.Context, string, string) (external.ExternalJobRecord, error)
	Cancel(context.Context, string, string) (external.ExternalJobRecord, error)
}

type MySQLJobService struct{ repository durableJobRepository }

func NewMySQLJobService(repository durableJobRepository) (*MySQLJobService, error) {
	if repository == nil {
		return nil, fmt.Errorf("durable job repository is required")
	}
	return &MySQLJobService{repository: repository}, nil
}

func (service *MySQLJobService) Submit(
	ctx context.Context,
	tenantID string,
	idempotencyKey string,
	command SubmitJobCommand,
) (JobView, bool, error) {
	result, err := service.repository.Submit(ctx, tenantID, idempotencyKey, external.JudgeJobRequest{
		BundleID: command.BundleID, Language: command.Language, SourceCode: []byte(command.SourceCode),
		StopOnFailure: command.StopOnFailure, CallbackID: command.CallbackID,
		ClientReference: command.ClientReference,
	})
	if err != nil {
		return JobView{}, false, mapRepositoryJobError(err)
	}
	view, err := publicJobView(result.Job)
	return view, result.Replayed, err
}

func (service *MySQLJobService) List(ctx context.Context, tenantID string, query JobListQuery) (JobListPage, error) {
	result, err := service.repository.List(ctx, tenantID, external.JobListOptions{
		Cursor: query.Cursor, Limit: query.Limit, Status: external.JobStatus(query.Status),
	})
	if err != nil {
		return JobListPage{}, mapRepositoryJobError(err)
	}
	page := JobListPage{Items: make([]JobView, 0, len(result.Jobs)), NextCursor: result.NextCursor}
	for _, job := range result.Jobs {
		view, err := publicJobView(job)
		if err != nil {
			return JobListPage{}, err
		}
		page.Items = append(page.Items, view)
	}
	return page, nil
}

func (service *MySQLJobService) Get(ctx context.Context, tenantID, jobID string) (JobView, error) {
	job, err := service.repository.Get(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, mapRepositoryJobError(err)
	}
	return publicJobView(job)
}

func (service *MySQLJobService) Cancel(ctx context.Context, tenantID, jobID string) (JobView, error) {
	job, err := service.repository.Cancel(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, mapRepositoryJobError(err)
	}
	return publicJobView(job)
}

func publicJobView(job external.ExternalJobRecord) (JobView, error) {
	status, err := publicJobStatus(job.Status)
	if err != nil {
		return JobView{}, err
	}
	view := JobView{
		JobID: job.ExternalID, Status: status, CreatedAt: job.CreatedAt,
		ClientReference: job.ClientReference,
	}
	if job.Result != nil {
		view.Result = &JobResultView{
			Verdict: job.Result.Verdict, CompileStatus: job.Result.CompileStatus,
			TimeMillis: job.Result.TimeMillis, MemoryBytes: job.Result.MemoryBytes,
			Cases: make([]CaseResultView, 0, len(job.Result.Cases)),
		}
		for _, item := range job.Result.Cases {
			view.Result.Cases = append(view.Result.Cases, CaseResultView{
				CaseID: item.CaseID, Verdict: item.Verdict,
				TimeMillis: item.TimeMillis, MemoryBytes: item.MemoryBytes,
			})
		}
	}
	return view, nil
}

func publicJobStatus(status external.JobStatus) (JobStatus, error) {
	switch status {
	case external.JobStatusQueued:
		return JobQueued, nil
	case external.JobStatusRunning:
		return JobRunning, nil
	case external.JobStatusSucceeded:
		return JobSucceeded, nil
	case external.JobStatusFailed:
		return JobFailed, nil
	case external.JobStatusCancelled:
		return JobCancelled, nil
	default:
		return "", ErrJobUnavailable
	}
}

func mapRepositoryJobError(err error) error {
	switch {
	case errors.Is(err, external.ErrExternalJobNotFound):
		return ErrJobNotFound
	case errors.Is(err, external.ErrExternalJobConflict):
		return ErrIdempotencyConflict
	case errors.Is(err, external.ErrQueuedQuotaExceeded):
		return ErrJobQuotaExceeded
	case errors.Is(err, external.ErrExternalJobInvalid), errors.Is(err, external.ErrInvalidJobCursor):
		return ErrJobInvalid
	case errors.Is(err, external.ErrExternalJobUnavailable):
		return ErrJobUnavailable
	default:
		return ErrJobUnavailable
	}
}
