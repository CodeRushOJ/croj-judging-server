package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type SubmissionStore interface {
	GetSubmissionByID(int64) (*model.Task, error)
	GetProblemVersionByID(int64) (*model.ProblemVersion, error)
	GetTestBundleByProblemVersionID(int64) (*model.TestBundle, error)
}

type ResultExecutor interface {
	Execute(context.Context, *model.Task, ExecutionConfig, *model.TestBundle) (callback.Result, error)
}

type ResultPublisher interface {
	Publish(context.Context, callback.Result) (callback.Disposition, error)
}

type JudgeService struct {
	store     SubmissionStore
	executor  ResultExecutor
	publisher ResultPublisher
	registry  *TaskRegistry
}

func NewJudgeService(
	store SubmissionStore,
	executor ResultExecutor,
	publisher ResultPublisher,
	registry *TaskRegistry,
) *JudgeService {
	if registry == nil {
		registry = NewTaskRegistry(10_000, 6*time.Hour)
	}
	return &JudgeService{store: store, executor: executor, publisher: publisher, registry: registry}
}

func (service *JudgeService) ProcessEvent(ctx context.Context, event model.SubmissionRequested) error {
	return service.registry.Process(
		ctx,
		event.DeduplicationKey(),
		func(ctx context.Context) (callback.Result, error) {
			return service.execute(ctx, event)
		},
		func(ctx context.Context, result callback.Result) error {
			_, err := service.publisher.Publish(ctx, result)
			return err
		},
	)
}

func (service *JudgeService) execute(ctx context.Context, event model.SubmissionRequested) (callback.Result, error) {
	submission, err := service.store.GetSubmissionByID(event.SubmissionID)
	if err != nil {
		return callback.Result{}, fmt.Errorf("get submission %d: %w", event.SubmissionID, err)
	}
	if submission == nil {
		return callback.Result{}, callback.Permanent(fmt.Errorf("submission %d does not exist", event.SubmissionID))
	}
	if submission.Status != model.StatusPending {
		return callback.Result{}, callback.Permanent(fmt.Errorf("submission %d is already terminal", event.SubmissionID))
	}
	if submission.ProblemID != event.ProblemID || submission.UserID != event.UserID || submission.Language != event.Language {
		return callback.Result{}, callback.Permanent(fmt.Errorf("SubmissionRequested metadata does not match submission %d", event.SubmissionID))
	}
	if submission.ProblemVersionID == nil || *submission.ProblemVersionID <= 0 {
		result := systemErrorResult("submission has no immutable problem version")
		return withResultIdentity(result, event), nil
	}
	problemVersionID := *submission.ProblemVersionID
	problemVersion, err := service.store.GetProblemVersionByID(problemVersionID)
	if err != nil {
		return callback.Result{}, fmt.Errorf("get problem version %d: %w", problemVersionID, err)
	}
	if problemVersion == nil || problemVersion.ID != problemVersionID || problemVersion.ProblemID != event.ProblemID || problemVersion.State != "PUBLISHED" {
		result := systemErrorResult("immutable problem version is unavailable")
		return withResultIdentity(result, event), nil
	}
	executionConfig, err := ParseExecutionConfig(problemVersion)
	if err != nil {
		result := systemErrorResult("immutable problem version is invalid or unsupported")
		return withResultIdentity(result, event), nil
	}
	testBundle, err := service.store.GetTestBundleByProblemVersionID(problemVersionID)
	if err != nil {
		return callback.Result{}, fmt.Errorf("get test bundle for problem version %d: %w", problemVersionID, err)
	}
	if testBundle == nil || testBundle.ProblemVersionID != problemVersionID {
		result := systemErrorResult("immutable test bundle is unavailable")
		return withResultIdentity(result, event), nil
	}
	result, err := service.executor.Execute(ctx, submission, executionConfig, testBundle)
	if err != nil {
		return callback.Result{}, fmt.Errorf("execute submission %d attempt %d: %w", event.SubmissionID, event.AttemptNo, err)
	}
	return withResultIdentity(result, event), nil
}

func withResultIdentity(result callback.Result, event model.SubmissionRequested) callback.Result {
	result.ResultID = event.EventID
	result.SubmissionID = event.SubmissionID
	result.AttemptNo = event.AttemptNo
	return result
}
