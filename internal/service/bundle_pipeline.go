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
)

type CaseArtifact interface {
	Manifest() bundle.Manifest
	ReadCase(bundle.Case) (string, string, error)
	Close() error
}

type BundlePipeline struct {
	selector         SandboxSelector
	executor         SandboxExecutor
	maxInfraAttempts int
}

func NewBundlePipeline(selector SandboxSelector, executor SandboxExecutor, maxInfraAttempts int) *BundlePipeline {
	if maxInfraAttempts <= 0 {
		maxInfraAttempts = 3
	}
	return &BundlePipeline{selector: selector, executor: executor, maxInfraAttempts: maxInfraAttempts}
}

func (pipeline *BundlePipeline) ExecuteArtifact(
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
	result := callback.Result{Status: callback.StatusAccepted}
	summaries := make([]string, 0, len(manifest.Cases))
	for _, testCase := range manifest.Cases {
		input, expected, err := artifact.ReadCase(testCase)
		if err != nil {
			return systemErrorResult("bundle case could not be read"), nil
		}
		response, infrastructureExhausted, err := pipeline.executeCase(ctx, submission, executionConfig, manifest.Checker, input, expected)
		if err != nil {
			return callback.Result{}, err
		}
		if infrastructureExhausted {
			summaries = append(summaries, fmt.Sprintf("case=%s status=SYSTEM_ERROR", testCase.ID))
			result.Status = callback.StatusSystemError
			result.Stderr = callback.TruncateUTF16(strings.Join(summaries, ";"), 65_536)
			return result, nil
		}
		caseStatus := mapBundleStatus(response.Status)
		if response.Status == "Accepted" && !outputsMatch(manifest.Checker, response.Stdout, expected) {
			caseStatus = callback.StatusWrongAnswer
		}
		result.TimeUsedMillis = max(result.TimeUsedMillis, boundedMetric(response.TimeUsed, 86_400_000))
		result.MemoryUsedKB = max(result.MemoryUsedKB, boundedMetric(response.MemoryUsed, 2_147_483_647))
		result.ExitCode = int(response.ExitCode)
		result.Status = caseStatus
		summaries = append(summaries, fmt.Sprintf("case=%s sandboxStatus=%s status=%s", testCase.ID, response.Status, caseStatus))
		result.Stderr = callback.TruncateUTF16(strings.Join(summaries, ";"), 65_536)
		if caseStatus == callback.StatusCompileError {
			// A sandbox has received source, hidden stdin and expected output. Its
			// diagnostic fields therefore cross an untrusted confidentiality
			// boundary and must never be forwarded to the callback.
			result.CompileError = "compilation failed; diagnostics redacted"
		}
		if caseStatus != callback.StatusAccepted {
			return result, nil
		}
	}
	result.ExitCode = 0
	return result, nil
}

func (pipeline *BundlePipeline) executeCase(
	ctx context.Context,
	submission *model.Task,
	executionConfig ExecutionConfig,
	checker bundle.Checker,
	input string,
	expected string,
) (*sandboxpb.ExecuteResponse, bool, error) {
	var lastRetryableError error
	receivedInvalidResponse := false
	for attempt := 0; attempt < pipeline.maxInfraAttempts; attempt++ {
		address, err := pipeline.selector.SelectSandbox()
		if err != nil {
			return nil, false, fmt.Errorf("select sandbox: %w", err)
		}
		expectedForSandbox := ""
		if checker == bundle.CheckerExact {
			expectedForSandbox = expected
		}
		response, err := pipeline.executor.Execute(ctx, address, &sandboxpb.ExecuteRequest{
			Language:       submission.Language,
			SourceCode:     submission.Code,
			Stdin:          input,
			Timeout:        timeoutSeconds(executionConfig.TimeLimitMillis),
			MemoryLimit:    boundedInt32(executionConfig.MemoryLimitMB),
			ExpectedOutput: expectedForSandbox,
		})
		if err != nil {
			code := status.Code(err)
			if code == codes.Unavailable || code == codes.ResourceExhausted {
				lastRetryableError = err
				continue
			}
			return nil, false, fmt.Errorf("execute hidden case: %w", err)
		}
		if response == nil {
			receivedInvalidResponse = true
			continue
		}
		if isKnownContestantStatus(response.Status) {
			return response, false, nil
		}
		// Sandbox Error and every unknown status are infrastructure failures.
		receivedInvalidResponse = true
	}
	if lastRetryableError != nil && !receivedInvalidResponse {
		return nil, false, fmt.Errorf("execute hidden case after %d endpoint attempts: %w", pipeline.maxInfraAttempts, lastRetryableError)
	}
	return nil, true, nil
}

func isKnownContestantStatus(value string) bool {
	switch value {
	case "Accepted", "Compile Error", "Wrong Answer", "Time Limit Exceeded", "Memory Limit Exceeded", "Runtime Error", "Output Limit Exceeded":
		return true
	default:
		return false
	}
}

func mapBundleStatus(value string) callback.Status {
	if value == "Output Limit Exceeded" {
		return callback.StatusRuntimeError
	}
	return mapCallbackStatus(value)
}

func outputsMatch(checker bundle.Checker, actual, expected string) bool {
	if checker == bundle.CheckerToken {
		actualTokens, expectedTokens := strings.Fields(actual), strings.Fields(expected)
		if len(actualTokens) != len(expectedTokens) {
			return false
		}
		for index := range actualTokens {
			if actualTokens[index] != expectedTokens[index] {
				return false
			}
		}
		return true
	}
	return normalizeExactOutput(actual) == normalizeExactOutput(expected)
}

func normalizeExactOutput(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func systemErrorResult(summary string) callback.Result {
	return callback.Result{Status: callback.StatusSystemError, Stderr: callback.TruncateUTF16(summary, 65_536)}
}
