package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sequenceSelector struct {
	endpoints []string
	calls     int
}

func (selector *sequenceSelector) SelectSandbox() (string, error) {
	if len(selector.endpoints) == 0 {
		return "", errors.New("no endpoint")
	}
	endpoint := selector.endpoints[selector.calls%len(selector.endpoints)]
	selector.calls++
	return endpoint, nil
}

func (selector *sequenceSelector) SelectSandboxExcluding(excluded map[string]struct{}) (string, error) {
	for range len(selector.endpoints) {
		endpoint, err := selector.SelectSandbox()
		if err != nil {
			return "", err
		}
		if _, skip := excluded[endpoint]; !skip {
			return endpoint, nil
		}
	}
	return "", errors.New("no untried endpoint")
}

type sequenceExecutor struct {
	responses []*sandboxpb.ExecuteResponse
	errors    []error
	requests  []*sandboxpb.ExecuteRequest
	addresses []string
}

func (executor *sequenceExecutor) Execute(_ context.Context, address string, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	executor.addresses = append(executor.addresses, address)
	executor.requests = append(executor.requests, request)
	index := len(executor.requests) - 1
	if index < len(executor.errors) && executor.errors[index] != nil {
		return nil, executor.errors[index]
	}
	return executor.responses[index], nil
}

type memoryArtifact struct {
	manifest      bundle.Manifest
	contents      map[string]string
	checkerSource string
}

func (artifact *memoryArtifact) Manifest() bundle.Manifest { return artifact.manifest }
func (artifact *memoryArtifact) ReadCase(testCase bundle.Case) (string, string, error) {
	return artifact.contents[testCase.Input], artifact.contents[testCase.Output], nil
}
func (artifact *memoryArtifact) ReadSpecialJudge() (string, error) {
	return artifact.checkerSource, nil
}
func (artifact *memoryArtifact) Close() error { return nil }

func TestBundlePipelineRunsCasesInOrderAndAggregatesMaximumMetrics(t *testing.T) {
	artifact := exactArtifact(2)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{
		{Status: "Accepted", TimeUsed: 20, MemoryUsed: 500, Stdout: "one\n"},
		{Status: "Accepted", TimeUsed: 15, MemoryUsed: 700, Stdout: "two\n"},
	}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusAccepted || result.TimeUsedMillis != 20 || result.MemoryUsedKB != 700 {
		t.Fatalf("result = %+v", result)
	}
	if len(executor.requests) != 2 || executor.requests[0].Stdin != "input-1" || executor.requests[1].ExpectedOutput != "two\n" {
		t.Fatalf("requests = %+v", executor.requests)
	}
}

func TestBundlePipelineTokenCheckerComparesInJudging(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.manifest.Checker = bundle.CheckerToken
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{{Status: "Accepted", Stdout: " one\t\n"}}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusAccepted {
		t.Fatalf("result = %+v", result)
	}
	if executor.requests[0].ExpectedOutput != "" {
		t.Fatal("token checker sent hidden expected output to sandbox comparison")
	}
}

func TestOutputCheckersMatchDocumentedNormalization(t *testing.T) {
	if !outputsMatch(bundle.CheckerExact, "  one  \r\n two\t\n\n", "one\ntwo") {
		t.Fatal("exact checker did not match sandbox normalization")
	}
	if outputsMatch(bundle.CheckerExact, "unexpected", "") {
		t.Fatal("empty expected output bypassed exact comparison")
	}
	if !outputsMatch(bundle.CheckerToken, "one\t two\n", " one two ") {
		t.Fatal("token checker rejected equivalent tokens")
	}
	if outputsMatch(bundle.CheckerToken, "one two", "one three") {
		t.Fatal("token checker accepted different tokens")
	}
}

func TestBundlePipelineDoesNotPublishHiddenContents(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.contents[artifact.manifest.Cases[0].Input] = "hidden-input-secret"
	artifact.contents[artifact.manifest.Cases[0].Output] = "hidden-output-secret"
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{{Status: "Accepted", Stdout: "contestant-output"}}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	serialized := result.Stdout + result.Stderr + result.CompileError
	for _, secret := range []string{"hidden-input-secret", "hidden-output-secret", "contestant-output"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("callback leaked %q: %+v", secret, result)
		}
	}
}

func TestBundlePipelineRedactsUntrustedCompileDiagnostics(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.contents[artifact.manifest.Cases[0].Input] = "hidden-input-secret"
	artifact.contents[artifact.manifest.Cases[0].Output] = "hidden-output-secret"
	submission := validBundleSubmission()
	submission.Code = "contestant-source-secret"
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{{
		Status:       "Compile Error",
		CompileError: submission.Code + " hidden-input-secret hidden-output-secret",
		Stderr:       "hidden-output-secret",
		Error:        "hidden-input-secret",
	}}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), submission, validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusCompileError || result.CompileError != "compilation failed; diagnostics redacted" {
		t.Fatalf("result = %+v", result)
	}
	serialized := result.Stdout + result.Stderr + result.CompileError
	for _, secret := range []string{submission.Code, "hidden-input-secret", "hidden-output-secret"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("compile callback leaked %q: %+v", secret, result)
		}
	}
}

func TestBundlePipelineStopsAtFirstContestantVerdict(t *testing.T) {
	artifact := exactArtifact(2)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{
		{Status: "Wrong Answer", TimeUsed: 8, MemoryUsed: 100},
		{Status: "Accepted"},
	}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || len(executor.requests) != 1 {
		t.Fatalf("result=%+v calls=%d", result, len(executor.requests))
	}
}

func TestBundlePipelineMapsOutputLimitToRuntimeErrorWithoutRetry(t *testing.T) {
	artifact := exactArtifact(1)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{
		{Status: "Output Limit Exceeded", TimeUsed: 8, MemoryUsed: 100},
		{Status: "Accepted"},
	}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusRuntimeError || len(executor.requests) != 1 || !strings.Contains(result.Stderr, "Output Limit Exceeded") {
		t.Fatalf("result=%+v calls=%d", result, len(executor.requests))
	}
}

func TestBundlePipelineFailsOverInfrastructureFailuresOnly(t *testing.T) {
	for name, first := range map[string]struct {
		response *sandboxpb.ExecuteResponse
		err      error
	}{
		"sandbox status":   {response: &sandboxpb.ExecuteResponse{Status: "Sandbox Error"}},
		"gRPC capacity":    {err: status.Error(codes.ResourceExhausted, "busy")},
		"gRPC unavailable": {err: status.Error(codes.Unavailable, "gone")},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := exactArtifact(1)
			executor := &sequenceExecutor{
				responses: []*sandboxpb.ExecuteResponse{first.response, {Status: "Accepted", Stdout: "one"}},
				errors:    []error{first.err, nil},
			}
			pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
			result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != callback.StatusAccepted || len(executor.requests) != 2 || executor.addresses[0] == executor.addresses[1] {
				t.Fatalf("result=%+v addresses=%v", result, executor.addresses)
			}
		})
	}
}

func TestBundlePipelineReturnsSystemErrorAfterUnknownStatusLimit(t *testing.T) {
	artifact := exactArtifact(1)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{{Status: "Mystery"}, {Status: "Mystery"}}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"a", "b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusSystemError || len(executor.requests) != 2 {
		t.Fatalf("result=%+v calls=%d", result, len(executor.requests))
	}
}

func TestBundlePipelineKeepsExhaustedTransportFailuresRetryable(t *testing.T) {
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			artifact := exactArtifact(1)
			executor := &sequenceExecutor{
				responses: []*sandboxpb.ExecuteResponse{nil, nil},
				errors: []error{
					status.Error(code, "sandbox temporarily unavailable"),
					status.Error(code, "sandbox temporarily unavailable"),
				},
			}
			pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"a", "b"}}, executor, 2)
			result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
			if status.Code(err) != code {
				t.Fatalf("error = %v, want retryable gRPC code %s; result=%+v", err, code, result)
			}
			if len(executor.requests) != 2 {
				t.Fatalf("calls = %d, want bounded endpoint failover before retry", len(executor.requests))
			}
		})
	}
}

func exactArtifact(count int) *memoryArtifact {
	manifest := bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64}}
	contents := make(map[string]string)
	for index := 1; index <= count; index++ {
		id := string(rune('0' + index))
		input, output := "case-"+id+".in", "case-"+id+".out"
		manifest.Cases = append(manifest.Cases, bundle.Case{ID: "case-" + id, Input: input, Output: output, Weight: 1})
		contents[input] = "input-" + id
		contents[output] = map[int]string{1: "one\n", 2: "two\n"}[index]
	}
	return &memoryArtifact{manifest: manifest, contents: contents}
}

func validBundleSubmission() *model.Task {
	return &model.Task{ID: 99, Language: "go", Code: "package main"}
}
func validExecutionConfig() ExecutionConfig {
	return ExecutionConfig{
		TimeLimitMillis: 1000,
		MemoryLimitMB:   64,
		JudgeMode:       bundle.JudgeModeACM,
		Checker:         bundle.CheckerExact,
	}
}
