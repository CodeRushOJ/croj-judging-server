package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	judgesandbox "github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var ErrCanonicalInfrastructure = errors.New("canonical execution infrastructure failed")

type SandboxBatchExecutor interface {
	ExecuteBatch(context.Context, string, *sandboxpb.ExecuteBatchV1Request) ([]*sandboxpb.ExecuteBatchV1Event, error)
}

type SandboxExcludingSelector interface {
	SelectSandboxExcluding(map[string]struct{}) (string, error)
}

const maxSandboxBatchCasesV1 = 256
const maxSandboxBatchRequestBytesV1 = 64 << 20
const executeBatchCasesFieldNumber = protowire.Number(6)

type BatchBundlePipeline struct {
	selector              SandboxSelector
	executor              SandboxBatchExecutor
	maxInfraAttempts      int
	maxRequestBytes       int
	maxExpectedCheckBytes int
}

// CanonicalExecutionRequest contains only caller-owned execution data. Test
// limits remain authoritative in the verified immutable bundle manifest.
type CanonicalExecutionRequest struct {
	Language      string
	SourceCode    string
	StopOnFailure bool
}

type CanonicalCaseResult struct {
	CaseID         string
	Status         callback.Status
	TimeUsedMillis int
	MemoryUsedKB   int
}

type CanonicalResult struct {
	Status         callback.Status
	ExitCode       int
	TimeUsedMillis int
	MemoryUsedKB   int
	Stderr         string
	CompileError   string
	Cases          []CanonicalCaseResult
}

func (result CanonicalResult) CallbackResult() callback.Result {
	return callback.Result{
		Status: result.Status, ExitCode: result.ExitCode,
		TimeUsedMillis: result.TimeUsedMillis, MemoryUsedKB: result.MemoryUsedKB,
		Stderr: result.Stderr, CompileError: result.CompileError,
	}
}

func NewBatchBundlePipeline(selector SandboxSelector, executor SandboxBatchExecutor, maxInfraAttempts int) *BatchBundlePipeline {
	if maxInfraAttempts <= 0 {
		maxInfraAttempts = 3
	}
	return &BatchBundlePipeline{
		selector:              selector,
		executor:              executor,
		maxInfraAttempts:      maxInfraAttempts,
		maxRequestBytes:       maxSandboxBatchRequestBytesV1,
		maxExpectedCheckBytes: maxSandboxBatchRequestBytesV1,
	}
}

func (pipeline *BatchBundlePipeline) ExecuteArtifact(
	ctx context.Context,
	submission *model.Task,
	executionConfig ExecutionConfig,
	artifact CaseArtifact,
) (callback.Result, error) {
	if submission == nil || artifact == nil || executionConfig.TimeLimitMillis <= 0 || executionConfig.MemoryLimitMB <= 0 {
		return callback.Result{}, fmt.Errorf("submission, immutable execution config, and test artifact are required")
	}
	result, err := pipeline.ExecuteCanonical(ctx, CanonicalExecutionRequest{
		Language: submission.Language, SourceCode: submission.Code, StopOnFailure: true,
	}, artifact)
	return result.CallbackResult(), err
}

func (pipeline *BatchBundlePipeline) ExecuteCanonical(
	ctx context.Context,
	input CanonicalExecutionRequest,
	artifact CaseArtifact,
) (CanonicalResult, error) {
	if pipeline == nil || artifact == nil || strings.TrimSpace(input.Language) == "" || input.SourceCode == "" {
		return CanonicalResult{}, fmt.Errorf("canonical execution request and test artifact are required")
	}
	manifest := artifact.Manifest()
	if err := manifest.Validate(); err != nil {
		return CanonicalResult{}, fmt.Errorf("%w: bundle manifest became invalid", ErrCanonicalInfrastructure)
	}
	if len(manifest.Cases) > maxSandboxBatchCasesV1 {
		return CanonicalResult{}, fmt.Errorf("%w: bundle exceeds sandbox batch case limit", ErrCanonicalInfrastructure)
	}
	request := &sandboxpb.ExecuteBatchV1Request{
		Language:      input.Language,
		SourceCode:    input.SourceCode,
		Timeout:       timeoutSeconds(manifest.Limits.TimeLimitMillis),
		MemoryLimit:   boundedInt32(manifest.Limits.MemoryLimitMiB),
		StopOnFailure: input.StopOnFailure,
		Cases:         make([]*sandboxpb.ExecuteBatchV1Case, 0, len(manifest.Cases)),
	}
	maxRequestBytes := pipeline.maxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = maxSandboxBatchRequestBytesV1
	}
	if proto.Size(request) > maxRequestBytes {
		return CanonicalResult{}, fmt.Errorf("%w: submission exceeds sandbox batch byte limit", ErrCanonicalInfrastructure)
	}
	requestBytes := proto.Size(request)
	expectedChecks := make([]string, 0, len(manifest.Cases))
	maxExpectedCheckBytes := pipeline.maxExpectedCheckBytes
	if maxExpectedCheckBytes <= 0 {
		maxExpectedCheckBytes = maxSandboxBatchRequestBytesV1
	}
	retainedExpectedCheckBytes := 0
	for _, testCase := range manifest.Cases {
		input, expected, err := artifact.ReadCase(testCase)
		if err != nil {
			return CanonicalResult{}, fmt.Errorf("%w: bundle case could not be read", ErrCanonicalInfrastructure)
		}
		expectedForSandbox := ""
		expectedTokenSHA256 := ""
		expectedCheck := expected
		if manifest.Checker == bundle.CheckerExact {
			expectedForSandbox = expected
		} else {
			expectedTokenSHA256 = tokenOutputSHA256(expected)
			expectedCheck = expectedTokenSHA256
		}
		expectedChecks = append(expectedChecks, expectedCheck)
		retainedExpectedCheckBytes += len(expectedCheck)
		if retainedExpectedCheckBytes > maxExpectedCheckBytes {
			return CanonicalResult{}, fmt.Errorf("%w: bundle exceeds local expected-check retention limit", ErrCanonicalInfrastructure)
		}
		requestCase := &sandboxpb.ExecuteBatchV1Case{
			CaseId:              testCase.ID,
			Stdin:               input,
			ExpectedOutput:      expectedForSandbox,
			CompareOutput:       manifest.Checker == bundle.CheckerExact,
			TokenExpectedSha256: expectedTokenSHA256,
		}
		requestBytes += batchCaseWireBytes(requestCase)
		if requestBytes > maxRequestBytes {
			return CanonicalResult{}, fmt.Errorf("%w: bundle exceeds sandbox batch byte limit", ErrCanonicalInfrastructure)
		}
		request.Cases = append(request.Cases, requestCase)
	}
	events, invalidResponse, err := pipeline.executeBatch(ctx, request)
	if err != nil {
		return CanonicalResult{}, err
	}
	if invalidResponse {
		return CanonicalResult{}, fmt.Errorf("%w: sandbox batch response was invalid", ErrCanonicalInfrastructure)
	}
	result := aggregateBatchResult(manifest, expectedChecks, events)
	if result.Status == callback.StatusSystemError {
		return CanonicalResult{}, fmt.Errorf("%w: sandbox returned a system error", ErrCanonicalInfrastructure)
	}
	return result, nil
}

func batchCaseWireBytes(requestCase *sandboxpb.ExecuteBatchV1Case) int {
	caseBytes := proto.Size(requestCase)
	return protowire.SizeTag(executeBatchCasesFieldNumber) + protowire.SizeVarint(uint64(caseBytes)) + caseBytes
}

func (pipeline *BatchBundlePipeline) executeBatch(
	ctx context.Context,
	request *sandboxpb.ExecuteBatchV1Request,
) ([]*sandboxpb.ExecuteBatchV1Event, bool, error) {
	var lastRetryable error
	attempted := make(map[string]struct{}, pipeline.maxInfraAttempts)
	for attempt := 0; attempt < pipeline.maxInfraAttempts; attempt++ {
		address, err := pipeline.selectUntriedSandbox(attempted)
		if err != nil {
			if lastRetryable != nil {
				return nil, false, fmt.Errorf("execute sandbox batch after %d distinct endpoint attempts: %w", len(attempted), lastRetryable)
			}
			return nil, false, fmt.Errorf("select sandbox: %w", err)
		}
		attempted[address] = struct{}{}
		events, err := pipeline.executor.ExecuteBatch(ctx, address, request)
		if err != nil {
			if errors.Is(err, judgesandbox.ErrInvalidBatchStream) {
				return nil, true, nil
			}
			code := status.Code(err)
			if code == codes.Unavailable || code == codes.ResourceExhausted {
				lastRetryable = err
				continue
			}
			return nil, false, fmt.Errorf("execute sandbox batch: %w", err)
		}
		if err := validateBatchEvents(request, events); err != nil {
			return nil, true, nil
		}
		return events, false, nil
	}
	if lastRetryable != nil {
		return nil, false, fmt.Errorf("execute sandbox batch after %d endpoint attempts: %w", pipeline.maxInfraAttempts, lastRetryable)
	}
	return nil, true, nil
}

func (pipeline *BatchBundlePipeline) selectUntriedSandbox(attempted map[string]struct{}) (string, error) {
	if selector, ok := pipeline.selector.(SandboxExcludingSelector); ok {
		return selector.SelectSandboxExcluding(attempted)
	}
	address, err := pipeline.selector.SelectSandbox()
	if err != nil {
		return "", err
	}
	if _, duplicate := attempted[address]; duplicate {
		return "", fmt.Errorf("selector returned an already attempted sandbox endpoint")
	}
	return address, nil
}

func tokenOutputSHA256(output string) string {
	hasher := sha256.New()
	var length [8]byte
	for _, token := range strings.Fields(output) {
		binary.BigEndian.PutUint64(length[:], uint64(len(token)))
		_, _ = hasher.Write(length[:])
		_, _ = hasher.Write([]byte(token))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func validateBatchEvents(request *sandboxpb.ExecuteBatchV1Request, events []*sandboxpb.ExecuteBatchV1Event) error {
	if request == nil || len(events) == 0 {
		return fmt.Errorf("batch response is empty")
	}
	if len(events) == 1 && events[0] != nil && events[0].Kind == sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR {
		if events[0].CaseId != "" || events[0].Result == nil || events[0].Result.Status != "Compile Error" {
			return fmt.Errorf("compile event is malformed")
		}
		return nil
	}
	caseIndex := 0
	for eventIndex, event := range events {
		if event == nil {
			return fmt.Errorf("event %d is nil", eventIndex)
		}
		switch event.Kind {
		case sandboxpb.ExecuteBatchV1Event_CASE_RESULT:
			if request.StopOnFailure && caseIndex > 0 && events[eventIndex-1].Result.Status != "Accepted" {
				return fmt.Errorf("batch continued after contestant verdict")
			}
			if caseIndex >= len(request.Cases) || event.Result == nil || event.CaseId != request.Cases[caseIndex].CaseId || !isKnownContestantStatus(event.Result.Status) {
				return fmt.Errorf("case event %d is malformed", eventIndex)
			}
			caseIndex++
		case sandboxpb.ExecuteBatchV1Event_COMPLETED:
			if eventIndex != len(events)-1 || event.CaseId != "" || event.Result != nil || caseIndex == 0 {
				return fmt.Errorf("completion event is malformed")
			}
			lastStatus := events[eventIndex-1].Result.Status
			if caseIndex != len(request.Cases) && (!request.StopOnFailure || lastStatus == "Accepted") {
				return fmt.Errorf("batch completed before all required cases")
			}
			return nil
		default:
			return fmt.Errorf("unexpected batch event kind %s", event.Kind)
		}
	}
	return fmt.Errorf("batch completion event is missing")
}

func aggregateBatchResult(
	manifest bundle.Manifest,
	expectedChecks []string,
	events []*sandboxpb.ExecuteBatchV1Event,
) CanonicalResult {
	if events[0].Kind == sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR {
		return CanonicalResult{
			Status:       callback.StatusCompileError,
			CompileError: "compilation failed; diagnostics redacted",
			Stderr:       "compilation failed",
		}
	}
	result := CanonicalResult{Status: callback.StatusAccepted, Cases: make([]CanonicalCaseResult, 0, len(events)-1)}
	summaries := make([]string, 0, len(events)-1)
	for index, event := range events[:len(events)-1] {
		caseStatus := mapBundleStatus(event.Result.Status)
		if event.Result.Status == "Accepted" && !outputMatchesExpectedCheck(manifest.Checker, event.Result.Stdout, expectedChecks[index]) {
			caseStatus = callback.StatusWrongAnswer
		}
		result.TimeUsedMillis = max(result.TimeUsedMillis, boundedMetric(event.Result.TimeUsed, 86_400_000))
		result.MemoryUsedKB = max(result.MemoryUsedKB, boundedMetric(event.Result.MemoryUsed, 2_147_483_647))
		if result.Status == callback.StatusAccepted && caseStatus != callback.StatusAccepted {
			result.Status = caseStatus
			result.ExitCode = int(event.Result.ExitCode)
		}
		result.Cases = append(result.Cases, CanonicalCaseResult{
			CaseID: event.CaseId, Status: caseStatus,
			TimeUsedMillis: boundedMetric(event.Result.TimeUsed, 86_400_000),
			MemoryUsedKB:   boundedMetric(event.Result.MemoryUsed, 2_147_483_647),
		})
		summaries = append(summaries, fmt.Sprintf("case=%s sandboxStatus=%s status=%s", event.CaseId, event.Result.Status, caseStatus))
		result.Stderr = callback.TruncateUTF16(strings.Join(summaries, ";"), 65_536)
		if caseStatus == callback.StatusCompileError {
			result.CompileError = "compilation failed; diagnostics redacted"
		}
	}
	if result.Status == callback.StatusAccepted {
		result.ExitCode = 0
	}
	return result
}

func outputMatchesExpectedCheck(checker bundle.Checker, actual, expectedCheck string) bool {
	if checker == bundle.CheckerToken {
		return tokenOutputSHA256(actual) == expectedCheck
	}
	return outputsMatch(checker, actual, expectedCheck)
}
