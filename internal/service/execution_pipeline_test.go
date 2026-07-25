package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
)

type fakeSelector struct {
	endpoint string
	err      error
}

func (s fakeSelector) SelectSandbox() (string, error) {
	return s.endpoint, s.err
}

type fakeExecutor struct {
	address  string
	request  *sandboxpb.ExecuteRequest
	response *sandboxpb.ExecuteResponse
	err      error
}

func (e *fakeExecutor) Execute(_ context.Context, address string, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	e.address = address
	e.request = request
	return e.response, e.err
}

func TestExecutionPipelineSelectsEndpointAndAppliesSandboxResult(t *testing.T) {
	executor := &fakeExecutor{response: &sandboxpb.ExecuteResponse{
		Status:     "Wrong Answer",
		TimeUsed:   37,
		MemoryUsed: 2048,
		Stderr:     "diagnostic",
	}}
	pipeline := NewExecutionPipeline(fakeSelector{endpoint: "10.0.0.7:50051"}, executor)
	submission := &model.Task{Language: "go", Code: "package main"}
	problem := &model.Problem{TimeLimit: 1500, MemoryLimit: 256}

	if err := pipeline.Run(context.Background(), submission, problem); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if executor.address != "10.0.0.7:50051" {
		t.Fatalf("address = %q", executor.address)
	}
	if executor.request.Language != "go" || executor.request.SourceCode != "package main" {
		t.Fatalf("unexpected request: %+v", executor.request)
	}
	if executor.request.Timeout != 2 || executor.request.MemoryLimit != 256 {
		t.Fatalf("limits = %ds/%dMiB, want 2s/256MiB", executor.request.Timeout, executor.request.MemoryLimit)
	}
	if submission.Status != model.StatusWrongAnswer {
		t.Fatalf("status = %d, want WrongAnswer", submission.Status)
	}
	if submission.RunTime == nil || *submission.RunTime != 37 {
		t.Fatalf("runtime = %v, want 37", submission.RunTime)
	}
	if submission.Memory == nil || *submission.Memory != 2048 {
		t.Fatalf("memory = %v, want 2048", submission.Memory)
	}
	if submission.JudgeInfo == nil || !strings.Contains(*submission.JudgeInfo, "Wrong Answer") {
		t.Fatalf("judge info = %v", submission.JudgeInfo)
	}
	if !json.Valid([]byte(*submission.JudgeInfo)) {
		t.Fatalf("judge info is not valid JSON: %q", *submission.JudgeInfo)
	}
}

func TestExecutionPipelineReturnsNoEndpointWithoutCallingSandbox(t *testing.T) {
	wantErr := errors.New("no ready sandbox endpoints")
	executor := &fakeExecutor{response: &sandboxpb.ExecuteResponse{Status: "Accepted"}}
	pipeline := NewExecutionPipeline(fakeSelector{err: wantErr}, executor)

	err := pipeline.Run(context.Background(), &model.Task{}, &model.Problem{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want %v", err, wantErr)
	}
	if executor.request != nil {
		t.Fatal("sandbox was called without an endpoint")
	}
}

func TestExecutionPipelineReturnsGRPCFailureWithoutMutatingSubmission(t *testing.T) {
	wantErr := errors.New("grpc unavailable")
	executor := &fakeExecutor{err: wantErr}
	pipeline := NewExecutionPipeline(fakeSelector{endpoint: "10.0.0.8:50051"}, executor)
	submission := &model.Task{Status: model.StatusPending}

	err := pipeline.Run(context.Background(), submission, &model.Problem{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want %v", err, wantErr)
	}
	if submission.Status != model.StatusPending {
		t.Fatalf("status = %d, want Pending", submission.Status)
	}
}

func TestMapSandboxStatus(t *testing.T) {
	tests := map[string]model.SubmissionStatus{
		"Accepted":              model.StatusAccepted,
		"Compile Error":         model.StatusCompileError,
		"Wrong Answer":          model.StatusWrongAnswer,
		"Time Limit Exceeded":   model.StatusTimeLimitExceeded,
		"Memory Limit Exceeded": model.StatusMemoryLimitExceeded,
		"Runtime Error":         model.StatusRuntimeError,
		"Output Limit Exceeded": model.StatusOutputLimitExceeded,
		"Sandbox Error":         model.StatusSystemError,
		"Unknown":               model.StatusSystemError,
	}
	for sandboxStatus, want := range tests {
		t.Run(sandboxStatus, func(t *testing.T) {
			if got := MapSandboxStatus(sandboxStatus); got != want {
				t.Fatalf("MapSandboxStatus(%q) = %d, want %d", sandboxStatus, got, want)
			}
		})
	}
}

func TestExecutionPipelineBuildsCallbackResult(t *testing.T) {
	executor := &fakeExecutor{response: &sandboxpb.ExecuteResponse{
		Status:       "Compile Error",
		ExitCode:     2,
		Stderr:       "compiler stderr",
		CompileError: "syntax error",
		TimeUsed:     -1,
		MemoryUsed:   -1,
	}}
	pipeline := NewExecutionPipeline(fakeSelector{endpoint: "10.0.0.9:50051"}, executor)

	result, err := pipeline.Execute(context.Background(), &model.Task{Language: "cpp", Code: "bad"}, &model.Problem{TimeLimit: 1000, MemoryLimit: 64})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != callback.StatusCompileError || result.CompileError != "syntax error" {
		t.Fatalf("result = %+v", result)
	}
	if result.TimeUsedMillis != 0 || result.MemoryUsedKB != 0 {
		t.Fatalf("negative metrics were not normalized: %+v", result)
	}
}

func TestExecutionPipelineTruncatesCallbackTextByUTF16CodeUnits(t *testing.T) {
	executor := &fakeExecutor{response: &sandboxpb.ExecuteResponse{
		Status: "Accepted",
		Stdout: strings.Repeat("😀", 40_000),
	}}
	pipeline := NewExecutionPipeline(fakeSelector{endpoint: "10.0.0.9:50051"}, executor)

	result, err := pipeline.Execute(context.Background(), &model.Task{}, &model.Problem{})
	if err != nil {
		t.Fatal(err)
	}
	if got := callback.UTF16Len(result.Stdout); got != 65_536 {
		t.Fatalf("stdout UTF-16 length = %d", got)
	}
}
