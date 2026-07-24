package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"gorm.io/datatypes"
)

type fakeSubmissionStore struct {
	submission *model.Task
	version    *model.ProblemVersion
	versionErr error
	bundle     *model.TestBundle
	bundleErr  error
}

func (store *fakeSubmissionStore) GetSubmissionByID(int64) (*model.Task, error) {
	return store.submission, nil
}

func (store *fakeSubmissionStore) GetProblemVersionByID(int64) (*model.ProblemVersion, error) {
	return store.version, store.versionErr
}

func (store *fakeSubmissionStore) GetTestBundleByProblemVersionID(int64) (*model.TestBundle, error) {
	return store.bundle, store.bundleErr
}

type fakeResultExecutor struct {
	result callback.Result
	err    error
	calls  int
	config ExecutionConfig
}

func (executor *fakeResultExecutor) Execute(_ context.Context, _ *model.Task, config ExecutionConfig, _ *model.TestBundle) (callback.Result, error) {
	executor.calls++
	executor.config = config
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
		submission: validBundleTask(),
		version:    validProblemVersion(),
		bundle:     &model.TestBundle{ProblemVersionID: 7},
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
	if executor.config.TimeLimitMillis != 2500 || executor.config.MemoryLimitMB != 128 {
		t.Fatalf("executor did not use immutable limits: %+v", executor.config)
	}
}

func TestJudgeServicePublishesSystemErrorForMissingImmutableBundle(t *testing.T) {
	for name, mutate := range map[string]func(*fakeSubmissionStore){
		"null problem version": func(store *fakeSubmissionStore) { store.submission.ProblemVersionID = nil },
		"missing version row":  func(store *fakeSubmissionStore) { store.version = nil },
		"missing bundle":       func(store *fakeSubmissionStore) { store.bundle = nil },
	} {
		t.Run(name, func(t *testing.T) {
			store := validStore()
			mutate(store)
			executor := &fakeResultExecutor{result: callback.Result{Status: callback.StatusAccepted}}
			publisher := &fakeResultPublisher{}
			judgeService := NewJudgeService(store, executor, publisher, NewTaskRegistry(16, time.Hour))
			if err := judgeService.ProcessEvent(context.Background(), validSubmissionEvent()); err != nil {
				t.Fatal(err)
			}
			if publisher.result.Status != callback.StatusSystemError || executor.calls != 0 {
				t.Fatalf("result=%+v executor calls=%d", publisher.result, executor.calls)
			}
		})
	}
}

func TestJudgeServiceRejectsMismatchedDraftAndUnsupportedImmutableVersions(t *testing.T) {
	tests := map[string]func(*model.ProblemVersion){
		"version mismatch": func(version *model.ProblemVersion) { version.ID = 8 },
		"problem mismatch": func(version *model.ProblemVersion) { version.ProblemID = 999 },
		"draft":            func(version *model.ProblemVersion) { version.State = "DRAFT" },
		"special judge": func(version *model.ProblemVersion) {
			version.JudgeConfigJSON = datatypes.JSON([]byte(`{"specialJudge":true,"specialJudgeCode":"x","specialJudgeLanguage":"go","judgeMode":0}`))
		},
		"OI": func(version *model.ProblemVersion) {
			version.JudgeConfigJSON = datatypes.JSON([]byte(`{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":1}`))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := validStore()
			mutate(store.version)
			executor := &fakeResultExecutor{}
			publisher := &fakeResultPublisher{}
			judgeService := NewJudgeService(store, executor, publisher, NewTaskRegistry(16, time.Hour))
			if err := judgeService.ProcessEvent(context.Background(), validSubmissionEvent()); err != nil {
				t.Fatal(err)
			}
			if publisher.result.Status != callback.StatusSystemError || executor.calls != 0 {
				t.Fatalf("result=%+v calls=%d", publisher.result, executor.calls)
			}
		})
	}
}

func TestJudgeServiceRetriesTransientBundleMetadataFailure(t *testing.T) {
	for name, mutate := range map[string]func(*fakeSubmissionStore){
		"version": func(store *fakeSubmissionStore) { store.versionErr = errors.New("database unavailable") },
		"bundle":  func(store *fakeSubmissionStore) { store.bundleErr = errors.New("database unavailable") },
	} {
		t.Run(name, func(t *testing.T) {
			store := validStore()
			mutate(store)
			publisher := &fakeResultPublisher{}
			judgeService := NewJudgeService(store, &fakeResultExecutor{}, publisher, NewTaskRegistry(16, time.Hour))
			err := judgeService.ProcessEvent(context.Background(), validSubmissionEvent())
			if err == nil || callback.IsPermanent(err) || publisher.calls != 0 {
				t.Fatalf("error=%v permanent=%v publisher calls=%d", err, callback.IsPermanent(err), publisher.calls)
			}
		})
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
		submission: validBundleTask(),
		version:    validProblemVersion(),
		bundle:     &model.TestBundle{ProblemVersionID: 7},
	}
}

func validProblemVersion() *model.ProblemVersion {
	return &model.ProblemVersion{
		ID:              7,
		ProblemID:       42,
		State:           "PUBLISHED",
		LimitsJSON:      datatypes.JSON([]byte(`{"timeLimit":2500,"memoryLimit":128,"totalScore":100}`)),
		JudgeConfigJSON: datatypes.JSON([]byte(`{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`)),
	}
}

func validBundleTask() *model.Task {
	problemVersionID := int64(7)
	return &model.Task{ID: 99, ProblemID: 42, ProblemVersionID: &problemVersionID, UserID: 7, Language: "java17", Status: model.StatusPending}
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
