package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type fakeSubmissionStore struct {
	submission *model.Task
	problem    *model.Problem
}

func (store *fakeSubmissionStore) GetSubmissionByID(int64) (*model.Task, error) {
	return store.submission, nil
}

func (store *fakeSubmissionStore) GetProblemByID(int64) (*model.Problem, error) {
	return store.problem, nil
}

type fakeResultExecutor struct {
	result callback.Result
	err    error
	calls  int
}

func (executor *fakeResultExecutor) Execute(context.Context, *model.Task, *model.Problem) (callback.Result, error) {
	executor.calls++
	return executor.result, executor.err
}

type fakeResultPublisher struct {
	result callback.Result
	err    error
	calls  int
}

func (publisher *fakeResultPublisher) Publish(_ context.Context, result callback.Result) (callback.Disposition, error) {
	publisher.calls++
	publisher.result = result
	return callback.DispositionApplied, publisher.err
}

func TestJudgeServicePublishesStableEventResultWithoutDatabaseWrite(t *testing.T) {
	store := &fakeSubmissionStore{
		submission: &model.Task{ID: 99, ProblemID: 42, UserID: 7, Language: "java17", Status: model.StatusPending},
		problem:    &model.Problem{ID: 42},
	}
	executor := &fakeResultExecutor{result: callback.Result{Status: callback.StatusAccepted}}
	publisher := &fakeResultPublisher{}
	service := NewJudgeService(store, executor, publisher, NewTaskRegistry(16, time.Hour))

	if err := service.ProcessEvent(context.Background(), validSubmissionEvent()); err != nil {
		t.Fatalf("ProcessEvent: %v", err)
	}
	if publisher.result.ResultID != "50f75fdf-fdea-473f-a156-bf1ed60acf58" || publisher.result.SubmissionID != 99 || publisher.result.AttemptNo != 1 {
		t.Fatalf("published result identity = %+v", publisher.result)
	}
	if executor.calls != 1 || publisher.calls != 1 {
		t.Fatalf("executor calls=%d publisher calls=%d", executor.calls, publisher.calls)
	}
}

func TestJudgeServiceKeepsGRPCFailureRetryable(t *testing.T) {
	wantErr := errors.New("rpc error: code = ResourceExhausted")
	executor := &fakeResultExecutor{err: wantErr}
	publisher := &fakeResultPublisher{}
	service := NewJudgeService(validStore(), executor, publisher, NewTaskRegistry(16, time.Hour))

	err := service.ProcessEvent(context.Background(), validSubmissionEvent())
	if !errors.Is(err, wantErr) || callback.IsPermanent(err) {
		t.Fatalf("ProcessEvent error = %v, permanent=%v", err, callback.IsPermanent(err))
	}
	if publisher.calls != 0 {
		t.Fatal("result was published after failed execution")
	}
}

func TestJudgeServiceDoesNotRepeatPermanentCallbackFailure(t *testing.T) {
	executor := &fakeResultExecutor{result: callback.Result{Status: callback.StatusAccepted}}
	publisher := &fakeResultPublisher{err: callback.Permanent(errors.New("HTTP 409"))}
	service := NewJudgeService(validStore(), executor, publisher, NewTaskRegistry(16, time.Hour))
	event := validSubmissionEvent()

	if err := service.ProcessEvent(context.Background(), event); !callback.IsPermanent(err) {
		t.Fatalf("first error = %v", err)
	}
	if err := service.ProcessEvent(context.Background(), event); err != nil {
		t.Fatalf("completed duplicate: %v", err)
	}
	if executor.calls != 1 || publisher.calls != 1 {
		t.Fatalf("executor calls=%d publisher calls=%d", executor.calls, publisher.calls)
	}
}

func validStore() *fakeSubmissionStore {
	return &fakeSubmissionStore{
		submission: &model.Task{ID: 99, ProblemID: 42, UserID: 7, Language: "java17", Status: model.StatusPending},
		problem:    &model.Problem{ID: 42},
	}
}

func validSubmissionEvent() model.SubmissionRequested {
	return model.SubmissionRequested{
		SchemaVersion: 1,
		EventID:       "50f75fdf-fdea-473f-a156-bf1ed60acf58",
		SubmissionID:  99,
		AttemptNo:     1,
		ProblemID:     42,
		UserID:        7,
		Language:      "java17",
	}
}
