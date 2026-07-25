package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

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

var (
	ErrCanonicalInfrastructure = errors.New("canonical execution infrastructure failed")
	ErrTenantCheckerFailure    = fmt.Errorf("%w: tenant special judge failed", ErrCanonicalInfrastructure)
)

type SandboxBatchExecutor interface {
	ExecuteBatch(context.Context, string, *sandboxpb.ExecuteBatchV1Request) ([]*sandboxpb.ExecuteBatchV1Event, error)
}

type SandboxExcludingSelector interface {
	SelectSandboxExcluding(map[string]struct{}) (string, error)
}

type SpecialJudgeArtifact interface {
	ReadSpecialJudge() (string, error)
}

const maxSandboxBatchCasesV1 = 256
const maxSandboxBatchRequestBytesV1 = 64 << 20
const maxSpecialJudgeProtocolBytesV1 = 4 << 20
const maxSpecialJudgeResultBytesV1 = 64 << 10
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
	Score          *int
	MaxScore       *int
}

type CanonicalResult struct {
	Status         callback.Status
	ExitCode       int
	TimeUsedMillis int
	MemoryUsedKB   int
	Stderr         string
	CompileError   string
	Score          *int
	TotalScore     *int
	Cases          []CanonicalCaseResult
}

func (result CanonicalResult) CallbackResult() callback.Result {
	return callback.Result{
		Status: result.Status, ExitCode: result.ExitCode,
		TimeUsedMillis: result.TimeUsedMillis, MemoryUsedKB: result.MemoryUsedKB,
		Score: copyInt(result.Score), TotalScore: copyInt(result.TotalScore),
		Stderr: result.Stderr, CompileError: result.CompileError,
	}
}

func copyInt(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
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
	if submission == nil || artifact == nil {
		return callback.Result{}, fmt.Errorf("submission and test artifact are required")
	}
	if executionConfig.TimeLimitMillis <= 0 || executionConfig.MemoryLimitMB <= 0 {
		return systemErrorResult("immutable execution limits must be positive"), nil
	}
	manifestLimits := artifact.Manifest().Limits
	if manifestLimits.TimeLimitMillis != executionConfig.TimeLimitMillis ||
		manifestLimits.MemoryLimitMiB != executionConfig.MemoryLimitMB {
		return systemErrorResult("immutable execution limits disagree with test bundle manifest"), nil
	}
	manifest := artifact.Manifest()
	if executionConfig.JudgeMode != manifest.JudgeMode {
		return systemErrorResult("immutable judge mode disagrees with test bundle manifest"), nil
	}
	if executionConfig.CheckerPinned && executionConfig.Checker != manifest.Checker {
		return systemErrorResult("immutable checker disagrees with test bundle manifest"), nil
	}
	if manifest.JudgeMode == bundle.JudgeModeOI &&
		(manifest.TotalScore == nil || executionConfig.TotalScore != *manifest.TotalScore) {
		return systemErrorResult("immutable total score disagrees with test bundle manifest"), nil
	}
	if manifest.Checker == bundle.CheckerSpecial {
		specialArtifact, ok := artifact.(SpecialJudgeArtifact)
		if !ok || manifest.SpecialJudge == nil || !executionConfig.SpecialJudge ||
			executionConfig.SpecialJudgeLanguage != manifest.SpecialJudge.Language {
			return systemErrorResult("immutable special judge metadata disagrees with test bundle manifest"), nil
		}
		source, err := specialArtifact.ReadSpecialJudge()
		if err != nil || source != executionConfig.SpecialJudgeSource {
			return systemErrorResult("immutable special judge source disagrees with test bundle manifest"), nil
		}
	} else if executionConfig.SpecialJudge {
		return systemErrorResult("immutable special judge config requires a special checker bundle"), nil
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
	stopOnFailure := input.StopOnFailure &&
		manifest.JudgeMode == bundle.JudgeModeACM &&
		manifest.Checker != bundle.CheckerSpecial
	request := &sandboxpb.ExecuteBatchV1Request{
		Language:      input.Language,
		SourceCode:    input.SourceCode,
		Timeout:       timeoutSeconds(manifest.Limits.TimeLimitMillis),
		MemoryLimit:   boundedInt32(manifest.Limits.MemoryLimitMiB),
		StopOnFailure: stopOnFailure,
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
		switch manifest.Checker {
		case bundle.CheckerExact:
			expectedForSandbox = expected
		case bundle.CheckerToken:
			expectedTokenSHA256 = tokenOutputSHA256(expected)
			expectedCheck = expectedTokenSHA256
		case bundle.CheckerSpecial:
			// The expected output remains only in judging-server memory and is
			// sent to the separately sandboxed checker through its bounded ABI.
		default:
			return CanonicalResult{}, fmt.Errorf("%w: unsupported checker", ErrCanonicalInfrastructure)
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
	result, actualOutputs := aggregateBatchResult(manifest, expectedChecks, events)
	if result.Status == callback.StatusSystemError {
		return CanonicalResult{}, fmt.Errorf("%w: sandbox returned a system error", ErrCanonicalInfrastructure)
	}
	if manifest.Checker == bundle.CheckerSpecial && result.Status != callback.StatusCompileError {
		result, err = pipeline.applySpecialJudge(ctx, manifest, artifact, request, expectedChecks, actualOutputs, result)
		if err != nil {
			return CanonicalResult{}, err
		}
	}
	return applyManifestScoring(manifest, result), nil
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
) (CanonicalResult, []string) {
	if events[0].Kind == sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR {
		return CanonicalResult{
			Status:       callback.StatusCompileError,
			CompileError: "compilation failed; diagnostics redacted",
			Stderr:       "compilation failed",
		}, nil
	}
	result := CanonicalResult{Status: callback.StatusAccepted, Cases: make([]CanonicalCaseResult, 0, len(events)-1)}
	actualOutputs := make([]string, 0, len(events)-1)
	summaries := make([]string, 0, len(events)-1)
	for index, event := range events[:len(events)-1] {
		caseStatus := mapBundleStatus(event.Result.Status)
		if event.Result.Status == "Accepted" && manifest.Checker != bundle.CheckerSpecial &&
			!outputMatchesExpectedCheck(manifest.Checker, event.Result.Stdout, expectedChecks[index]) {
			caseStatus = callback.StatusWrongAnswer
		}
		actualOutputs = append(actualOutputs, event.Result.Stdout)
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
	return result, actualOutputs
}

func outputMatchesExpectedCheck(checker bundle.Checker, actual, expectedCheck string) bool {
	if checker == bundle.CheckerToken {
		return tokenOutputSHA256(actual) == expectedCheck
	}
	return outputsMatch(checker, actual, expectedCheck)
}

type specialJudgeInputV1 struct {
	SchemaVersion  int    `json:"schemaVersion"`
	CaseID         string `json:"caseId"`
	Input          string `json:"input"`
	ExpectedOutput string `json:"expectedOutput"`
	ActualOutput   string `json:"actualOutput"`
}

type specialJudgeOutputV1 struct {
	SchemaVersion int    `json:"schemaVersion"`
	Accepted      *bool  `json:"accepted"`
	Message       string `json:"message,omitempty"`
}

func (pipeline *BatchBundlePipeline) applySpecialJudge(
	ctx context.Context,
	manifest bundle.Manifest,
	artifact CaseArtifact,
	contestantRequest *sandboxpb.ExecuteBatchV1Request,
	expectedOutputs []string,
	actualOutputs []string,
	result CanonicalResult,
) (CanonicalResult, error) {
	specialArtifact, ok := artifact.(SpecialJudgeArtifact)
	if !ok || manifest.SpecialJudge == nil || len(result.Cases) != len(manifest.Cases) ||
		len(contestantRequest.Cases) != len(manifest.Cases) || len(expectedOutputs) != len(manifest.Cases) ||
		len(actualOutputs) != len(manifest.Cases) {
		return CanonicalResult{}, fmt.Errorf("%w: special judge artifact is incomplete", ErrCanonicalInfrastructure)
	}
	source, err := specialArtifact.ReadSpecialJudge()
	if err != nil || source == "" {
		return CanonicalResult{}, fmt.Errorf("%w: special judge source is unavailable", ErrCanonicalInfrastructure)
	}
	checkerRequest := &sandboxpb.ExecuteBatchV1Request{
		Language:      manifest.SpecialJudge.Language,
		SourceCode:    source,
		Timeout:       timeoutSeconds(manifest.SpecialJudge.TimeLimitMillis),
		MemoryLimit:   boundedInt32(manifest.SpecialJudge.MemoryLimitMiB),
		StopOnFailure: false,
		Cases:         make([]*sandboxpb.ExecuteBatchV1Case, 0, len(manifest.Cases)),
	}
	requestBytes := proto.Size(checkerRequest)
	caseIndexes := make([]int, 0, len(manifest.Cases))
	for index, item := range result.Cases {
		if item.Status != callback.StatusAccepted {
			continue
		}
		if !specialJudgePayloadWithinPreEncodeLimit(
			item.CaseID,
			contestantRequest.Cases[index].Stdin,
			expectedOutputs[index],
			actualOutputs[index],
		) {
			return CanonicalResult{}, fmt.Errorf("%w: special judge ABI input exceeds its limit", ErrTenantCheckerFailure)
		}
		payload, err := json.Marshal(specialJudgeInputV1{
			SchemaVersion:  1,
			CaseID:         item.CaseID,
			Input:          contestantRequest.Cases[index].Stdin,
			ExpectedOutput: expectedOutputs[index],
			ActualOutput:   actualOutputs[index],
		})
		actualOutputs[index] = ""
		if err != nil || len(payload) > maxSpecialJudgeProtocolBytesV1 {
			return CanonicalResult{}, fmt.Errorf("%w: special judge ABI input exceeds its limit", ErrTenantCheckerFailure)
		}
		requestCase := &sandboxpb.ExecuteBatchV1Case{CaseId: item.CaseID, Stdin: string(payload)}
		requestBytes += batchCaseWireBytes(requestCase)
		if requestBytes > pipeline.maxBatchRequestBytes() {
			return CanonicalResult{}, fmt.Errorf("%w: special judge batch exceeds sandbox byte limit", ErrTenantCheckerFailure)
		}
		checkerRequest.Cases = append(checkerRequest.Cases, requestCase)
		caseIndexes = append(caseIndexes, index)
	}
	if len(checkerRequest.Cases) == 0 {
		return recomputeCanonicalVerdict(result), nil
	}
	events, invalidResponse, err := pipeline.executeBatch(ctx, checkerRequest)
	if err != nil {
		return CanonicalResult{}, err
	}
	if invalidResponse || len(events) != len(checkerRequest.Cases)+1 {
		return CanonicalResult{}, fmt.Errorf("%w: special judge sandbox response was invalid", ErrCanonicalInfrastructure)
	}
	if events[0].Kind == sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR {
		return CanonicalResult{}, fmt.Errorf("%w: special judge compilation failed", ErrTenantCheckerFailure)
	}
	for eventIndex, event := range events[:len(events)-1] {
		if event.Result == nil {
			return CanonicalResult{}, fmt.Errorf("%w: special judge sandbox response was invalid", ErrCanonicalInfrastructure)
		}
		if event.Result.Status != "Accepted" {
			return CanonicalResult{}, fmt.Errorf("%w: special judge did not execute successfully", ErrTenantCheckerFailure)
		}
		accepted, err := parseSpecialJudgeOutput(event.Result.Stdout)
		if err != nil {
			return CanonicalResult{}, fmt.Errorf("%w: special judge output was invalid", ErrTenantCheckerFailure)
		}
		index := caseIndexes[eventIndex]
		result.Cases[index].TimeUsedMillis += boundedMetric(event.Result.TimeUsed, 86_400_000)
		result.Cases[index].MemoryUsedKB = max(
			result.Cases[index].MemoryUsedKB,
			boundedMetric(event.Result.MemoryUsed, 2_147_483_647),
		)
		if accepted {
			result.Cases[index].Status = callback.StatusAccepted
		} else {
			result.Cases[index].Status = callback.StatusWrongAnswer
		}
	}
	return recomputeCanonicalVerdict(result), nil
}

func specialJudgePayloadWithinPreEncodeLimit(caseID, input, expected, actual string) bool {
	// Reserve more than the fixed JSON keys, punctuation, schema version and
	// string quotes require. The remaining budget is charged using the exact
	// escaping classes used by encoding/json, preventing a large temporary
	// allocation before the final serialized-size check.
	remaining := maxSpecialJudgeProtocolBytesV1 - 256
	for _, value := range []string{caseID, input, expected, actual} {
		var ok bool
		remaining, ok = consumeJSONStringBudget(value, remaining)
		if !ok {
			return false
		}
	}
	return true
}

func consumeJSONStringBudget(value string, remaining int) (int, bool) {
	if remaining < 0 || !utf8.ValidString(value) {
		return 0, false
	}
	for index := 0; index < len(value); {
		character := value[index]
		cost := 1
		switch {
		case character == '"' || character == '\\' ||
			character == '\b' || character == '\f' ||
			character == '\n' || character == '\r' || character == '\t':
			cost = 2
			index++
		case character < 0x20 || character == '<' || character == '>' || character == '&':
			cost = 6
			index++
		case character < utf8.RuneSelf:
			index++
		default:
			runeValue, width := utf8.DecodeRuneInString(value[index:])
			if runeValue == '\u2028' || runeValue == '\u2029' {
				cost = 6
			} else {
				cost = width
			}
			index += width
		}
		if cost > remaining {
			return 0, false
		}
		remaining -= cost
	}
	return remaining, true
}

func (pipeline *BatchBundlePipeline) maxBatchRequestBytes() int {
	if pipeline.maxRequestBytes > 0 {
		return pipeline.maxRequestBytes
	}
	return maxSandboxBatchRequestBytesV1
}

func parseSpecialJudgeOutput(data string) (bool, error) {
	if len(data) == 0 || len(data) > maxSpecialJudgeResultBytesV1 || !utf8.ValidString(data) {
		return false, fmt.Errorf("checker output encoding or size is invalid")
	}
	var output specialJudgeOutputV1
	decoder := json.NewDecoder(bytes.NewReader([]byte(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return false, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false, fmt.Errorf("checker output must contain exactly one JSON document")
	}
	if output.SchemaVersion != 1 || output.Accepted == nil || callback.UTF16Len(output.Message) > 1024 {
		return false, fmt.Errorf("checker output contract is invalid")
	}
	return *output.Accepted, nil
}

func recomputeCanonicalVerdict(result CanonicalResult) CanonicalResult {
	result.Status = callback.StatusAccepted
	result.ExitCode = 0
	result.TimeUsedMillis = 0
	result.MemoryUsedKB = 0
	summaries := make([]string, 0, len(result.Cases))
	for _, item := range result.Cases {
		if result.Status == callback.StatusAccepted && item.Status != callback.StatusAccepted {
			result.Status = item.Status
		}
		result.TimeUsedMillis = max(result.TimeUsedMillis, item.TimeUsedMillis)
		result.MemoryUsedKB = max(result.MemoryUsedKB, item.MemoryUsedKB)
		summaries = append(summaries, fmt.Sprintf("case=%s status=%s", item.CaseID, item.Status))
	}
	result.Stderr = callback.TruncateUTF16(strings.Join(summaries, ";"), 65_536)
	return result
}

func applyManifestScoring(manifest bundle.Manifest, result CanonicalResult) CanonicalResult {
	if manifest.JudgeMode != bundle.JudgeModeOI || manifest.TotalScore == nil {
		return result
	}
	score, total := 0, *manifest.TotalScore
	for index := range result.Cases {
		caseScore := 0
		caseMaximum := manifest.Cases[index].Weight
		if result.Cases[index].Status == callback.StatusAccepted {
			caseScore = caseMaximum
			score += caseScore
		}
		result.Cases[index].Score = copyInt(&caseScore)
		result.Cases[index].MaxScore = copyInt(&caseMaximum)
	}
	result.Score, result.TotalScore = copyInt(&score), copyInt(&total)
	if result.Status != callback.StatusCompileError {
		if score == total {
			result.Status = callback.StatusAccepted
			result.ExitCode = 0
		} else {
			result.Status = callback.StatusWrongAnswer
		}
	}
	return result
}
