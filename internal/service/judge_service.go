package service

import (
	"context"
	"fmt"
	"strconv"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type SubmissionStore interface {
	GetSubmissionByID(int64) (*model.Task, error)
	GetProblemByID(int64) (*model.Problem, error)
	UpdateSubmissionResultInTx(*model.Task) error
}

type SubmissionExecutor interface {
	Run(context.Context, *model.Task, *model.Problem) error
}

type JudgeService struct {
	store    SubmissionStore
	executor SubmissionExecutor
}

func NewJudgeService(store SubmissionStore, executor SubmissionExecutor) *JudgeService {
	return &JudgeService{store: store, executor: executor}
}

func (s *JudgeService) ProcessTask(ctx context.Context, taskIDValue string) error {
	taskID, err := strconv.ParseInt(taskIDValue, 10, 64)
	if err != nil || taskID <= 0 {
		return fmt.Errorf("invalid task ID %q", taskIDValue)
	}

	submission, err := s.store.GetSubmissionByID(taskID)
	if err != nil {
		return fmt.Errorf("get submission %d: %w", taskID, err)
	}
	if submission == nil || submission.Status != model.StatusPending {
		return nil
	}
	problem, err := s.store.GetProblemByID(submission.ProblemID)
	if err != nil {
		return fmt.Errorf("get problem %d for submission %d: %w", submission.ProblemID, taskID, err)
	}
	if problem == nil {
		return fmt.Errorf("problem %d for submission %d does not exist", submission.ProblemID, taskID)
	}
	if err := s.executor.Run(ctx, submission, problem); err != nil {
		return fmt.Errorf("execute submission %d: %w", taskID, err)
	}
	if err := s.store.UpdateSubmissionResultInTx(submission); err != nil {
		return fmt.Errorf("persist submission %d result: %w", taskID, err)
	}
	return nil
}
