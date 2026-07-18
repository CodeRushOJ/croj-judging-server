package service

import (
	"context"
	"errors"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type fakeSubmissionStore struct {
	submission *model.Task
	problem    *model.Problem
	updated    *model.Task
}

func (s *fakeSubmissionStore) GetSubmissionByID(int64) (*model.Task, error) {
	return s.submission, nil
}

func (s *fakeSubmissionStore) GetProblemByID(int64) (*model.Problem, error) {
	return s.problem, nil
}

func (s *fakeSubmissionStore) UpdateSubmissionResultInTx(submission *model.Task) error {
	s.updated = submission
	return nil
}

type fakePipeline struct {
	err    error
	called bool
}

func (p *fakePipeline) Run(_ context.Context, submission *model.Task, _ *model.Problem) error {
	p.called = true
	if p.err == nil {
		submission.Status = model.StatusAccepted
	}
	return p.err
}

func TestJudgeServicePersistsRealPipelineResult(t *testing.T) {
	store := &fakeSubmissionStore{
		submission: &model.Task{ID: 41, ProblemID: 7, Status: model.StatusPending},
		problem:    &model.Problem{ID: 7, TimeLimit: 1000, MemoryLimit: 256},
	}
	pipeline := &fakePipeline{}
	service := NewJudgeService(store, pipeline)

	if err := service.ProcessTask(context.Background(), "41"); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if !pipeline.called {
		t.Fatal("execution pipeline was not called")
	}
	if store.updated == nil || store.updated.Status != model.StatusAccepted {
		t.Fatalf("updated submission = %+v", store.updated)
	}
}

func TestJudgeServiceDoesNotPersistWhenSandboxFails(t *testing.T) {
	store := &fakeSubmissionStore{
		submission: &model.Task{ID: 42, ProblemID: 8, Status: model.StatusPending},
		problem:    &model.Problem{ID: 8},
	}
	wantErr := errors.New("sandbox unavailable")
	service := NewJudgeService(store, &fakePipeline{err: wantErr})

	err := service.ProcessTask(context.Background(), "42")
	if !errors.Is(err, wantErr) {
		t.Fatalf("ProcessTask error = %v, want %v", err, wantErr)
	}
	if store.updated != nil {
		t.Fatalf("unexpected persisted submission: %+v", store.updated)
	}
}
