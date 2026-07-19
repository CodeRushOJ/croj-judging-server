package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type SandboxBatchExecutor interface {
	ExecuteBatch(context.Context, string, *sandboxpb.ExecuteBatchV1Request) ([]*sandboxpb.ExecuteBatchV1Event, error)
}

const maxSandboxBatchCasesV1 = 256
const maxSandboxBatchRequestBytesV1 = 64 << 20

type BatchBundlePipeline struct {
	selector         SandboxSelector
	executor         SandboxBatchExecutor
	maxInfraAttempts int
}

func NewBatchBundlePipeline(selector SandboxSelector, executor SandboxBatchExecutor, maxInfraAttempts int) *BatchBundlePipeline {
	if maxInfraAttempts <= 0 {
		maxInfraAttempts = 3
	}
	return &BatchBundlePipeline{selector: selector, executor: executor, maxInfraAttempts: maxInfraAttempts}
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
	manifest := artifact.Manifest()
	if err := manifest.Validate(); err != nil {
		return systemErrorResult("bundle manifest became invalid"), nil
	}
	if len(manifest.Cases) > maxSandboxBatchCasesV1 {
		return systemErrorResult("bundle exceeds sandbox batch case limit"), nil
	}
	request := &sandboxpb.ExecuteBatchV1Request{
		Language:      submission.Language,
		SourceCode:    submission.Code,
		Timeout:       timeoutSeconds(executionConfig.TimeLimitMillis),
		MemoryLimit:   boundedInt32(executionConfig.MemoryLimitMB),
		StopOnFailure: true,
		Cases:         make([]*sandboxpb.ExecuteBatchV1Case, 0, len(manifest.Cases)),
	}
	expectedOutputs := make([]string, 0, len(manifest.Cases))
	for _, testCase := range manifest.Cases {
		input, expected, err := artifact.ReadCase(testCase)
		if err != nil {
			return systemErrorResult("bundle case could not be read"), nil
		}
		expectedOutputs = append(expectedOutputs, expected)
		expectedForSandbox := ""
		if manifest.Checker == bundle.CheckerExact {
			expectedForSandbox = expected
		}
		request.Cases = append(request.Cases, &sandboxpb.ExecuteBatchV1Case{
			CaseId:         testCase.ID,
			Stdin:          input,
			ExpectedOutput: expectedForSandbox,
			CompareOutput:  manifest.Checker == bundle.CheckerExact,
		})
	}
	if proto.Size(request) > maxSandboxBatchRequestBytesV1 {
		return systemErrorResult("bundle exceeds sandbox batch byte limit"), nil
	}
	events, invalidResponse, err := pipeline.executeBatch(ctx, request)
	if err != nil {
		return callback.Result{}, err
	}
	if invalidResponse {
		return systemErrorResult("sandbox batch response was invalid"), nil
	}
	return aggregateBatchResult(manifest, expectedOutputs, events), nil
}

func (pipeline *BatchBundlePipeline) executeBatch(
	ctx context.Context,
	request *sandboxpb.ExecuteBatchV1Request,
) ([]*sandboxpb.ExecuteBatchV1Event, bool, error) {
	var lastRetryable error
	receivedInvalid := false
	for attempt := 0; attempt < pipeline.maxInfraAttempts; attempt++ {
		address, err := pipeline.selector.SelectSandbox()
		if err != nil {
			return nil, false, fmt.Errorf("select sandbox: %w", err)
		}
		events, err := pipeline.executor.ExecuteBatch(ctx, address, request)
		if err != nil {
			code := status.Code(err)
			if code == codes.Unavailable || code == codes.ResourceExhausted {
				lastRetryable = err
				continue
			}
			return nil, false, fmt.Errorf("execute sandbox batch: %w", err)
		}
		if err := validateBatchEvents(request, events); err != nil {
			receivedInvalid = true
			continue
		}
		return events, false, nil
	}
	if lastRetryable != nil && !receivedInvalid {
		return nil, false, fmt.Errorf("execute sandbox batch after %d endpoint attempts: %w", pipeline.maxInfraAttempts, lastRetryable)
	}
	return nil, true, nil
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
			if caseIndex > 0 && events[eventIndex-1].Result.Status != "Accepted" {
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
	expectedOutputs []string,
	events []*sandboxpb.ExecuteBatchV1Event,
) callback.Result {
	if events[0].Kind == sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR {
		return callback.Result{
			Status:       callback.StatusCompileError,
			CompileError: "compilation failed; diagnostics redacted",
			Stderr:       "compilation failed",
		}
	}
	result := callback.Result{Status: callback.StatusAccepted}
	summaries := make([]string, 0, len(events)-1)
	for index, event := range events[:len(events)-1] {
		caseStatus := mapBundleStatus(event.Result.Status)
		if event.Result.Status == "Accepted" && !outputsMatch(manifest.Checker, event.Result.Stdout, expectedOutputs[index]) {
			caseStatus = callback.StatusWrongAnswer
		}
		result.TimeUsedMillis = max(result.TimeUsedMillis, boundedMetric(event.Result.TimeUsed, 86_400_000))
		result.MemoryUsedKB = max(result.MemoryUsedKB, boundedMetric(event.Result.MemoryUsed, 2_147_483_647))
		result.ExitCode = int(event.Result.ExitCode)
		result.Status = caseStatus
		summaries = append(summaries, fmt.Sprintf("case=%s sandboxStatus=%s status=%s", event.CaseId, event.Result.Status, caseStatus))
		result.Stderr = callback.TruncateUTF16(strings.Join(summaries, ";"), 65_536)
		if caseStatus == callback.StatusCompileError {
			result.CompileError = "compilation failed; diagnostics redacted"
		}
		if caseStatus != callback.StatusAccepted {
			return result
		}
	}
	result.ExitCode = 0
	return result
}
