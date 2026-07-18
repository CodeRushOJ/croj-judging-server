package service

import (
	"context"
	"errors"
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
	manifest bundle.Manifest
	contents map[string]string
}

func (artifact *memoryArtifact) Manifest() bundle.Manifest { return artifact.manifest }
func (artifact *memoryArtifact) ReadCase(testCase bundle.Case) (string, string, error) {
	return artifact.contents[testCase.Input], artifact.contents[testCase.Output], nil
}
func (artifact *memoryArtifact) Close() error { return nil }

func TestBundlePipelineRunsCasesInOrderAndAggregatesMaximumMetrics(t *testing.T) {
	artifact := exactArtifact(2)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{
		{Status: "Accepted", TimeUsed: 20, MemoryUsed: 500, Stdout: "one\n"},
		{Status: "Accepted", TimeUsed: 15, MemoryUsed: 700, Stdout: "two\n"},
	}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validBundleProblem(), artifact)
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
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validBundleProblem(), artifact)
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

func TestBundlePipelineStopsAtFirstContestantVerdict(t *testing.T) {
	artifact := exactArtifact(2)
	executor := &sequenceExecutor{responses: []*sandboxpb.ExecuteResponse{
		{Status: "Wrong Answer", TimeUsed: 8, MemoryUsed: 100},
		{Status: "Accepted"},
	}}
	pipeline := NewBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validBundleProblem(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || len(executor.requests) != 1 {
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
			result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validBundleProblem(), artifact)
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
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validBundleProblem(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusSystemError || len(executor.requests) != 2 {
		t.Fatalf("result=%+v calls=%d", result, len(executor.requests))
	}
}

func exactArtifact(count int) *memoryArtifact {
	manifest := bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact}
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
func validBundleProblem() *model.Problem { return &model.Problem{TimeLimit: 1000, MemoryLimit: 64} }
