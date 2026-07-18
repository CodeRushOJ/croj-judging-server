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
	GetProblemByID(int64) (*model.Problem, error)
	GetTestBundleByProblemVersionID(int64) (*model.TestBundle, error)
}

type ResultExecutor interface {
	Execute(context.Context, *model.Task, *model.Problem, *model.TestBundle) (callback.Result, error)
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
	problem, err := service.store.GetProblemByID(event.ProblemID)
	if err != nil {
		return callback.Result{}, fmt.Errorf("get problem %d: %w", event.ProblemID, err)
	}
	if problem == nil {
		return callback.Result{}, callback.Permanent(fmt.Errorf("problem %d does not exist", event.ProblemID))
	}
	if submission.ProblemVersionID == nil || *submission.ProblemVersionID <= 0 {
		result := systemErrorResult("submission has no immutable problem version")
		return withResultIdentity(result, event), nil
	}
	testBundle, err := service.store.GetTestBundleByProblemVersionID(*submission.ProblemVersionID)
	if err != nil {
		return callback.Result{}, fmt.Errorf("get test bundle for problem version %d: %w", *submission.ProblemVersionID, err)
	}
	if testBundle == nil || testBundle.ProblemVersionID != *submission.ProblemVersionID {
		result := systemErrorResult("immutable test bundle is unavailable")
		return withResultIdentity(result, event), nil
	}
	result, err := service.executor.Execute(ctx, submission, problem, testBundle)
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
