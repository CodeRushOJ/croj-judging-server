package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	judgesandbox "github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
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

type countingArtifact struct {
	*memoryArtifact
	reads int
}

func (artifact *countingArtifact) ReadCase(testCase bundle.Case) (string, string, error) {
	artifact.reads++
	return artifact.memoryArtifact.ReadCase(testCase)
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

func TestBatchBundlePipelineReturnsSystemErrorWhenImmutableLimitsDisagree(t *testing.T) {
	for _, test := range []struct {
		name   string
		config ExecutionConfig
	}{
		{name: "time limit", config: ExecutionConfig{TimeLimitMillis: 2000, MemoryLimitMB: 64}},
		{name: "memory limit", config: ExecutionConfig{TimeLimitMillis: 1000, MemoryLimitMB: 128}},
		{name: "zero time limit", config: ExecutionConfig{TimeLimitMillis: 0, MemoryLimitMB: 64}},
		{name: "negative time limit", config: ExecutionConfig{TimeLimitMillis: -1, MemoryLimitMB: 64}},
		{name: "zero memory limit", config: ExecutionConfig{TimeLimitMillis: 1000, MemoryLimitMB: 0}},
		{name: "negative memory limit", config: ExecutionConfig{TimeLimitMillis: 1000, MemoryLimitMB: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := &countingArtifact{memoryArtifact: exactArtifact(1)}
			executor := &batchExecutorStub{}
			pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)

			result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), test.config, artifact)

			if err != nil || result.Status != callback.StatusSystemError {
				t.Fatalf("result=%+v error=%v, want terminal SYSTEM_ERROR", result, err)
			}
			if artifact.reads != 0 || len(executor.requests) != 0 {
				t.Fatalf("case reads=%d sandbox calls=%d, want immutable-boundary rejection", artifact.reads, len(executor.requests))
			}
		})
	}
}

func TestBatchBundlePipelineRejectsImmutableCheckerMismatchBeforeReadingCases(t *testing.T) {
	artifact := &countingArtifact{memoryArtifact: exactArtifact(1)}
	executor := &batchExecutorStub{}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	config := validExecutionConfig()
	config.Checker = bundle.CheckerToken
	config.CheckerPinned = true

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)

	if err != nil || result.Status != callback.StatusSystemError || artifact.reads != 0 || len(executor.requests) != 0 {
		t.Fatalf("result=%+v error=%v reads=%d calls=%d", result, err, artifact.reads, len(executor.requests))
	}
}

func TestBatchBundlePipelineAllowsLegacyUnpinnedTokenChecker(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.manifest.Checker = bundle.CheckerToken
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "one\n"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	config := validExecutionConfig()
	config.CheckerPinned = false

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)

	if err != nil || result.Status != callback.StatusAccepted || len(executor.requests) != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, len(executor.requests))
	}
}

func TestBatchBundlePipelineRejectsOversizedBatchBeforeSandbox(t *testing.T) {
	artifact := &memoryArtifact{
		manifest: bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64}},
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
	if !errors.Is(err, ErrCanonicalInfrastructure) || result.Status != "" || len(executor.requests) != 0 {
		t.Fatalf("result=%+v sandbox calls=%d", result, len(executor.requests))
	}
}

func TestIncrementalBatchCaseWireSizeMatchesProtobuf(t *testing.T) {
	request := &sandboxpb.ExecuteBatchV1Request{Language: "cpp", SourceCode: "int main(){}", Timeout: 2, MemoryLimit: 64}
	incremental := proto.Size(request)
	for index := 0; index < 256; index++ {
		requestCase := &sandboxpb.ExecuteBatchV1Case{CaseId: fmt.Sprintf("case-%03d", index), Stdin: strings.Repeat("x", index*17), ExpectedOutput: "answer", CompareOutput: true}
		incremental += batchCaseWireBytes(requestCase)
		request.Cases = append(request.Cases, requestCase)
		if incremental != proto.Size(request) {
			t.Fatalf("case %d incremental=%d protobuf=%d", index, incremental, proto.Size(request))
		}
	}
	field := request.ProtoReflect().Descriptor().Fields().ByName("cases")
	if field == nil || protowire.Number(field.Number()) != executeBatchCasesFieldNumber {
		t.Fatalf("cases field number=%v want=%d", field, executeBatchCasesFieldNumber)
	}
}

func TestBatchBundlePipelineStopsReadingCasesWhenWireLimitIsCrossed(t *testing.T) {
	artifact := &countingArtifact{memoryArtifact: exactArtifact(3)}
	executor := &batchExecutorStub{}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	pipeline.maxRequestBytes = proto.Size(&sandboxpb.ExecuteBatchV1Request{
		Language:      validBundleSubmission().Language,
		SourceCode:    validBundleSubmission().Code,
		Timeout:       timeoutSeconds(validExecutionConfig().TimeLimitMillis),
		MemoryLimit:   boundedInt32(validExecutionConfig().MemoryLimitMB),
		StopOnFailure: true,
	})

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if !errors.Is(err, ErrCanonicalInfrastructure) || result.Status != "" || artifact.reads != 1 || len(executor.requests) != 0 {
		t.Fatalf("result=%+v reads=%d sandbox calls=%d", result, artifact.reads, len(executor.requests))
	}
}

func TestBatchBundlePipelineRetainsOnlyHashesForLargeTokenOutputs(t *testing.T) {
	const caseCount = 64
	artifact := &countingArtifact{memoryArtifact: &memoryArtifact{
		manifest: bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerToken, Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64}},
		contents: make(map[string]string, caseCount*2),
	}}
	largeToken := strings.Repeat("x", 256<<10)
	for index := range caseCount {
		id := fmt.Sprintf("case-%03d", index)
		input, output := id+".in", id+".out"
		artifact.manifest.Cases = append(artifact.manifest.Cases, bundle.Case{ID: id, Input: input, Output: output, Weight: 1})
		artifact.contents[input] = "x"
		artifact.contents[output] = fmt.Sprintf("%03d-%s", index, largeToken)
	}
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{{
		Kind:   sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR,
		Result: &sandboxpb.ExecuteResponse{Status: "Compile Error"},
	}}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	pipeline.maxExpectedCheckBytes = caseCount * sha256.Size * 2

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusCompileError || artifact.reads != caseCount || len(executor.requests) != 1 {
		t.Fatalf("result=%+v reads=%d sandbox calls=%d", result, artifact.reads, len(executor.requests))
	}
	if size := proto.Size(executor.requests[0]); size >= 1<<20 {
		t.Fatalf("token request retained raw expected outputs: protobuf size=%d", size)
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

func TestBatchBundlePipelineDoesNotRetryMalformedCompletedStream(t *testing.T) {
	executor := &sequenceBatchExecutor{eventSets: [][]*sandboxpb.ExecuteBatchV1Event{nil, nil}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if !errors.Is(err, ErrCanonicalInfrastructure) || result.Status != "" || len(executor.requests) != 1 {
		t.Fatalf("result=%+v sandbox calls=%d, want deterministic failure without retry", result, len(executor.requests))
	}
}

func TestBatchBundlePipelineDoesNotRetryClientRejectedStream(t *testing.T) {
	executor := &sequenceBatchExecutor{errors: []error{judgesandbox.ErrInvalidBatchStream, nil}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 2)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if !errors.Is(err, ErrCanonicalInfrastructure) || result.Status != "" || len(executor.requests) != 1 {
		t.Fatalf("result=%+v sandbox calls=%d, want deterministic failure without retry", result, len(executor.requests))
	}
}

func TestBatchBundlePipelineRoutesSandboxSystemErrorToInfrastructureFailure(t *testing.T) {
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Sandbox Error"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteCanonical(context.Background(), CanonicalExecutionRequest{Language: "cpp", SourceCode: "int main(){}", StopOnFailure: true}, exactArtifact(1))
	if !errors.Is(err, ErrCanonicalInfrastructure) || result.Status != "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
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

func TestBatchBundlePipelineCanonicalRequestUsesBundleLimitsAndStopPolicy(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.manifest.Limits = bundle.Limits{TimeLimitMillis: 1500, MemoryLimitMiB: 512}
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "one"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"dns:///sandbox-workers.coderushoj.svc.cluster.local:50051"}}, executor, 1)
	result, err := pipeline.ExecuteCanonical(context.Background(), CanonicalExecutionRequest{
		Language: "cpp", SourceCode: "int main(){}", StopOnFailure: false,
	}, artifact)
	if err != nil || result.Status != callback.StatusAccepted || len(result.Cases) != 1 || result.Cases[0].CaseID != "case-1" || result.Cases[0].Status != callback.StatusAccepted {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	request := executor.requests[0]
	if request.Language != "cpp" || request.SourceCode != "int main(){}" || request.Timeout != 2 || request.MemoryLimit != 512 || request.StopOnFailure {
		t.Fatalf("canonical sandbox request = %+v", request)
	}
}

func TestBatchBundlePipelineKeepsOrderedResultsWhenStopOnFailureIsDisabled(t *testing.T) {
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "wrong", TimeUsed: 8, MemoryUsed: 100}},
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "two", TimeUsed: 11, MemoryUsed: 120}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteCanonical(context.Background(), CanonicalExecutionRequest{
		Language: "cpp", SourceCode: "int main(){}", StopOnFailure: false,
	}, exactArtifact(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || len(result.Cases) != 2 ||
		result.Cases[0].CaseID != "case-1" || result.Cases[0].Status != callback.StatusWrongAnswer ||
		result.Cases[1].CaseID != "case-2" || result.Cases[1].Status != callback.StatusAccepted ||
		result.TimeUsedMillis != 11 || result.MemoryUsedKB != 120 {
		t.Fatalf("canonical result = %+v", result)
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

func TestBatchBundlePipelineNeverRetriesSameEndpointInOneAttempt(t *testing.T) {
	executor := &sequenceBatchExecutor{errors: []error{
		status.Error(codes.Unavailable, "gone"),
		status.Error(codes.Unavailable, "gone"),
		status.Error(codes.Unavailable, "gone"),
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 3)
	_, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), exactArtifact(1))
	if status.Code(err) != codes.Unavailable || len(executor.requests) != 1 {
		t.Fatalf("error=%v sandbox calls=%d, want one attempt and preserved Unavailable", err, len(executor.requests))
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
	if executor.requests[0].Cases[0].ExpectedOutput != "" || executor.requests[0].Cases[0].CompareOutput || executor.requests[0].Cases[0].TokenExpectedSha256 != "615f69ed4e249a34955fc08be20fc324c06462f6ae8b817d22280505adca9209" || !executor.requests[0].StopOnFailure {
		t.Fatalf("token expected output crossed sandbox boundary: %+v", executor.requests[0].Cases[0])
	}
}

func TestBatchBundlePipelineRechecksTokenVerdictFromRetainedHash(t *testing.T) {
	artifact := exactArtifact(1)
	artifact.manifest.Checker = bundle.CheckerToken
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "wrong"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer {
		t.Fatalf("result=%+v, want local hash-only Wrong Answer", result)
	}
}

func TestBatchBundlePipelineStopsTokenBatchAtFirstMismatch(t *testing.T) {
	artifact := exactArtifact(2)
	artifact.manifest.Checker = bundle.CheckerToken
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Wrong Answer", Stdout: "wrong"}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), validExecutionConfig(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || executor.requests[0].Cases[0].TokenExpectedSha256 == "" || executor.requests[0].Cases[0].ExpectedOutput != "" {
		t.Fatalf("result=%+v request=%+v, want hash-only first-case comparison", result, executor.requests[0])
	}
}

func TestBatchBundlePipelineScoresEveryOICaseDeterministically(t *testing.T) {
	total := 100
	artifact := exactArtifact(2)
	artifact.manifest.SchemaVersion = 2
	artifact.manifest.JudgeMode = bundle.JudgeModeOI
	artifact.manifest.TotalScore = &total
	artifact.manifest.Cases[0].Weight = 30
	artifact.manifest.Cases[1].Weight = 70
	executor := &batchExecutorStub{events: []*sandboxpb.ExecuteBatchV1Event{
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Wrong Answer", TimeUsed: 8, MemoryUsed: 100}},
		{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "two\n", TimeUsed: 11, MemoryUsed: 120}},
		{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	config := validExecutionConfig()
	config.JudgeMode, config.TotalScore = bundle.JudgeModeOI, total

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || result.Score == nil || *result.Score != 70 ||
		result.TotalScore == nil || *result.TotalScore != 100 || executor.requests[0].StopOnFailure {
		t.Fatalf("result=%+v request=%+v", result, executor.requests[0])
	}
}

func TestBatchBundlePipelineRunsSpecialJudgeThroughASecondSandboxBatch(t *testing.T) {
	artifact, config := specialJudgeArtifact(t, bundle.JudgeModeACM, []int{1, 1})
	executor := &sequenceBatchExecutor{eventSets: [][]*sandboxpb.ExecuteBatchV1Event{
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "contestant-one", TimeUsed: 10, MemoryUsed: 100}},
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "contestant-two", TimeUsed: 20, MemoryUsed: 100}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: `{"schemaVersion":1,"accepted":true}`, TimeUsed: 3, MemoryUsed: 200}},
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: `{"schemaVersion":1,"accepted":false,"message":"redacted reason"}`, TimeUsed: 4, MemoryUsed: 80}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 1)

	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || len(executor.requests) != 2 ||
		executor.requests[0].StopOnFailure || executor.requests[1].Language != "go" ||
		executor.requests[1].SourceCode != artifact.checkerSource ||
		result.TimeUsedMillis != 24 || result.MemoryUsedKB != 200 {
		t.Fatalf("result=%+v requests=%+v", result, executor.requests)
	}
	var checkerInput struct {
		SchemaVersion  int    `json:"schemaVersion"`
		CaseID         string `json:"caseId"`
		Input          string `json:"input"`
		ExpectedOutput string `json:"expectedOutput"`
		ActualOutput   string `json:"actualOutput"`
	}
	if err := json.Unmarshal([]byte(executor.requests[1].Cases[0].Stdin), &checkerInput); err != nil {
		t.Fatal(err)
	}
	if checkerInput.SchemaVersion != 1 || checkerInput.CaseID != "case-1" ||
		checkerInput.Input != "input-1" || checkerInput.ExpectedOutput != "one\n" ||
		checkerInput.ActualOutput != "contestant-one" {
		t.Fatalf("checker ABI input=%+v", checkerInput)
	}
	serialized := result.Stdout + result.Stderr + result.CompileError
	for _, hidden := range []string{"input-1", "one\n", "contestant-one", "redacted reason", artifact.checkerSource} {
		if strings.Contains(serialized, hidden) {
			t.Fatalf("callback leaked hidden checker data %q: %+v", hidden, result)
		}
	}
}

func TestBatchBundlePipelineScoresSpecialJudgeOICases(t *testing.T) {
	artifact, _ := specialJudgeArtifact(t, bundle.JudgeModeOI, []int{30, 70})
	executor := &sequenceBatchExecutor{eventSets: [][]*sandboxpb.ExecuteBatchV1Event{
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "contestant-one"}},
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "contestant-two"}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: `{"schemaVersion":1,"accepted":true}`}},
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-2", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: `{"schemaVersion":1,"accepted":false}`}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 1)

	result, err := pipeline.ExecuteCanonical(
		context.Background(),
		CanonicalExecutionRequest{Language: "go", SourceCode: "package main\nfunc main() {}\n"},
		artifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != callback.StatusWrongAnswer || result.Score == nil || *result.Score != 30 ||
		result.TotalScore == nil || *result.TotalScore != 100 ||
		result.Cases[0].Score == nil || *result.Cases[0].Score != 30 ||
		result.Cases[1].Score == nil || *result.Cases[1].Score != 0 {
		t.Fatalf("OI special-judge result = %+v", result)
	}
}

func TestBatchBundlePipelineRejectsMalformedSpecialJudgeOutputAsInfrastructure(t *testing.T) {
	artifact, config := specialJudgeArtifact(t, bundle.JudgeModeACM, []int{1})
	executor := &sequenceBatchExecutor{eventSets: [][]*sandboxpb.ExecuteBatchV1Event{
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: "contestant"}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
		{
			{Kind: sandboxpb.ExecuteBatchV1Event_CASE_RESULT, CaseId: "case-1", Result: &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: `{"schemaVersion":1,"accepted":true,"unknown":"secret"}`}},
			{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED},
		},
	}}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a", "sandbox-b"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)
	if !errors.Is(err, ErrCanonicalInfrastructure) ||
		!errors.Is(err, ErrTenantCheckerFailure) ||
		result.Status != "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestParseSpecialJudgeOutputRejectsInvalidUTF8(t *testing.T) {
	if _, err := parseSpecialJudgeOutput(
		"{\"schemaVersion\":1,\"accepted\":true,\"message\":\"" + string([]byte{0xff}) + "\"}",
	); err == nil {
		t.Fatal("invalid UTF-8 checker output was accepted")
	}
}

func TestSpecialJudgeABIPreEncodeLimitRejectsOversizedComponents(t *testing.T) {
	if !specialJudgePayloadWithinPreEncodeLimit("case-1", "input", "expected", "actual") {
		t.Fatal("small checker ABI payload was rejected")
	}
	if specialJudgePayloadWithinPreEncodeLimit(
		"case-1",
		strings.Repeat("x", maxSpecialJudgeProtocolBytesV1),
		"expected",
		"actual",
	) {
		t.Fatal("oversized checker ABI component was accepted before JSON encoding")
	}
}

func TestBatchBundlePipelineRejectsInternalSpecialJudgeSnapshotMismatchBeforeSandbox(t *testing.T) {
	artifact, config := specialJudgeArtifact(t, bundle.JudgeModeACM, []int{1})
	config.SpecialJudgeSource += "// mismatch"
	executor := &batchExecutorStub{}
	pipeline := NewBatchBundlePipeline(&sequenceSelector{endpoints: []string{"sandbox-a"}}, executor, 1)
	result, err := pipeline.ExecuteArtifact(context.Background(), validBundleSubmission(), config, artifact)
	if err != nil || result.Status != callback.StatusSystemError || len(executor.requests) != 0 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, len(executor.requests))
	}
}

func specialJudgeArtifact(t *testing.T, mode bundle.JudgeMode, weights []int) (*memoryArtifact, ExecutionConfig) {
	t.Helper()
	source := "package main\nfunc main() {}\n"
	digest := sha256.Sum256([]byte(source))
	manifest := bundle.Manifest{
		SchemaVersion: 2,
		JudgeMode:     mode,
		Checker:       bundle.CheckerSpecial,
		Limits:        bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		SpecialJudge: &bundle.SpecialJudge{
			Language: "go", Source: "checker/main.go", SourceSHA256: hex.EncodeToString(digest[:]),
			TimeLimitMillis: 2000, MemoryLimitMiB: 128,
		},
	}
	contents := make(map[string]string, len(weights)*2)
	total := 0
	for index, weight := range weights {
		id := fmt.Sprintf("case-%d", index+1)
		input, output := id+".in", id+".out"
		manifest.Cases = append(manifest.Cases, bundle.Case{ID: id, Input: input, Output: output, Weight: weight})
		contents[input] = fmt.Sprintf("input-%d", index+1)
		contents[output] = []string{"one\n", "two\n"}[index%2]
		total += weight
	}
	config := validExecutionConfig()
	config.SpecialJudge = true
	config.Checker = bundle.CheckerSpecial
	config.SpecialJudgeLanguage = "go"
	config.SpecialJudgeSource = source
	if mode == bundle.JudgeModeOI {
		manifest.TotalScore = &total
		config.JudgeMode, config.TotalScore = bundle.JudgeModeOI, total
	}
	return &memoryArtifact{manifest: manifest, contents: contents, checkerSource: source}, config
}
