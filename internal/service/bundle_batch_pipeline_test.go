package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type batchExecutorStub struct {
	requests  []*sandboxpb.ExecuteBatchV1Request
	addresses []string
	events    []*sandboxpb.ExecuteBatchV1Event
	err       error
}

type sequenceBatchExecutor struct {
	eventSets [][]*sandboxpb.ExecuteBatchV1Event
	errors    []error
	requests  []*sandboxpb.ExecuteBatchV1Request
	addresses []string
}

func (executor *sequenceBatchExecutor) ExecuteBatch(_ context.Context, address string, request *sandboxpb.ExecuteBatchV1Request) ([]*sandboxpb.ExecuteBatchV1Event, error) {
	index := len(executor.requests)
	executor.requests = append(executor.requests, request)
	executor.addresses = append(executor.addresses, address)
	if index < len(executor.errors) && executor.errors[index] != nil {
		return nil, executor.errors[index]
	}
	return executor.eventSets[index], nil
}

func TestBatchBundlePipelineRejectsOversizedBatchBeforeSandbox(t *testing.T) {
	artifact := &memoryArtifact{
		manifest: bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact},
		contents: map[string]string{},
	}
	for index := 0; index < 257; index++ {
		id := fmt.Sprintf("case-%03d", index)
		input, output := id+".in", id+".out"
		artifact.manifest.Cases = append(artifact.manifest.Cases, bundle.Case{ID: id, Input: input, Output: output, Weight: 1})
		artifact.contents[input], artifact.contents[output] = "input", "output"
	}
	executor := &batchExecutorStub{}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusSystemError || len(executor.requests) != 0 {
		t.Fatalf("result=%+v sandbox calls=%d", result, len(executor.requests))
	}
}

func (executor *batchExecutorStub) ExecuteBatch(
	_ context.Context,
	address string,
	request *sandboxpb.ExecuteBatchV1Request,
) ([]*sandboxpb.ExecuteBatchV1Event, error) {
	executor.addresses = append(executor.addresses, address)
	executor.requests = append(executor.requests, request)
	return executor.events, executor.err
}

func TestBatchBundlePipelineSendsAllCasesInOneCompileOnceRequest(t *testing.T) {
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "one", TimeUsed: 8, MemoryUsed: 100}},
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "two", TimeUsed: 11, MemoryUsed: 120}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 2)

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusAccepted || result.TimeUsedMillis != 11 || result.MemoryUsedKB != 120 {
		t.Fatalf("result = %+v", result)
	}
	if len(executor.requests) != 1 || len(executor.requests[0].Cases) != 2 {
		t.Fatalf("batch requests = %+v", executor.requests)
	}
	if executor.requests[0].Cases[0].CaseId != "case-1" || executor.requests[0].Cases[1].CaseId != "case-2" {
		t.Fatalf("case order = %+v", executor.requests[0].Cases)
	}
	if !executor.requests[0].Cases[0].CompareOutput || executor.requests[0].Cases[0].ExpectedOutput != "one\n" {
		t.Fatalf("exact case contract = %+v", executor.requests[0].Cases[0])
	}
}

func TestBatchBundlePipelineRetriesWholeBatchOnCapacityFailure(t *testing.T) {
	executor := &sequenceBatchExecutor{
		errors: []error{status.Error(codes.ResourceExhausted, "busy"), nil},
		eventSets: [][]*sandboxpb.ExecuteBatchV1Event{nil, {
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "one"}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		}},
	}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusAccepted || len(executor.requests) != 2 || executor.addresses[0] == executor.addresses[1] {
		t.Fatalf("result=%+v addresses=%v", result, executor.addresses)
	}
}

func TestBatchBundlePipelinePreservesRetryableCodeAfterEndpointExhaustion(t *testing.T) {
	executor := &sequenceBatchExecutor{errors: []error{
		status.Error(codes.Unavailable, "gone"),
		status.Error(codes.Unavailable, "gone"),
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	_, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v, want Unavailable", err)
	}
}

func TestBatchBundlePipelineRedactsCompileDiagnostics(t *testing.T) {
	secret := "source-and-hidden-diagnostic"
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{{
		Kind: sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR,
		Result: &sandboxpb.ExecuteResponse{
			Status:       "Compile Error",
			CompileError: secret,
			Error:        secret,
		},
	}}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusCompileError || result.CompileError != "compilation failed; diagnostics redacted" || strings.Contains(fmt.Sprintf("%+v", result), secret) {
		t.Fatalf("result leaked compile diagnostics: %+v", result)
	}
}

func TestBatchBundlePipelineKeepsTokenExpectedOutputOutOfSandbox(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.manifest.Checker = bundle.CheckerToken
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: " one\t"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusAccepted {
		t.Fatalf("result = %+v", result)
	}
	if executor.requests[0].Cases[0].ExpectedOutput != "" || executor.requests[0].Cases[0].CompareOutput || !executor.requests[0].StopOnFailure {
		t.Fatalf("token expected output crossed sandbox boundary: %+v", executor.requests[0].Cases[0])
	}
}
