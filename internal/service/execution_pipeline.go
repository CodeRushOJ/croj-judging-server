package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
)

type SandboxSelector interface {
	SelectSandbox() (string, error)
}

type SandboxExecutor interface {
	Execute(context.Context, string, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error)
}

type ExecutionPipeline struct {
	selector SandboxSelector
	executor SandboxExecutor
}

func NewExecutionPipeline(selector SandboxSelector, executor SandboxExecutor) *ExecutionPipeline {
	return &ExecutionPipeline{selector: selector, executor: executor}
}

func (p *ExecutionPipeline) Run(ctx context.Context, submission *model.Task, problem *model.Problem) error {
	if submission == nil || problem == nil {
		return fmt.Errorf("submission and problem are required")
	}
	address, err := p.selector.SelectSandbox()
	if err != nil {
		return fmt.Errorf("select sandbox: %w", err)
	}
	response, err := p.executor.Execute(ctx, address, &sandboxpb.ExecuteRequest{
		Language:    submission.Language,
		SourceCode:  submission.Code,
		Timeout:     timeoutSeconds(problem.TimeLimit),
		MemoryLimit: boundedInt32(problem.MemoryLimit),
	})
	if err != nil {
		return fmt.Errorf("execute submission %d: %w", submission.ID, err)
	}
	if response == nil {
		return fmt.Errorf("execute submission %d: sandbox returned no response", submission.ID)
	}
	applySandboxResult(submission, response)
	return nil
}

func MapSandboxStatus(status string) model.SubmissionStatus {
	switch status {
	case "Accepted":
		return model.StatusAccepted
	case "Compile Error":
		return model.StatusCompileError
	case "Wrong Answer":
		return model.StatusWrongAnswer
	case "Time Limit Exceeded":
		return model.StatusTimeLimitExceeded
	case "Memory Limit Exceeded":
		return model.StatusMemoryLimitExceeded
	case "Runtime Error":
		return model.StatusRuntimeError
	default:
		return model.StatusSystemError
	}
}

func applySandboxResult(submission *model.Task, response *sandboxpb.ExecuteResponse) {
	submission.Status = MapSandboxStatus(response.Status)
	runtime := boundedInt(response.TimeUsed)
	memory := boundedInt(response.MemoryUsed)
	submission.RunTime = &runtime
	submission.Memory = &memory
	judgeInfoBytes, _ := json.Marshal(struct {
		Status       string `json:"status"`
		ExitCode     int32  `json:"exitCode"`
		Diagnostic   string `json:"diagnostic,omitempty"`
		CompileError string `json:"compileError,omitempty"`
	}{
		Status:       response.Status,
		ExitCode:     response.ExitCode,
		Diagnostic:   firstDiagnostic(response.Error, response.Stderr),
		CompileError: truncate(strings.TrimSpace(response.CompileError), 4096),
	})
	judgeInfo := string(judgeInfoBytes)
	submission.JudgeInfo = &judgeInfo
	if submission.Status == model.StatusSystemError && response.Error != "" {
		errorMessage := truncate(response.Error, 4096)
		submission.ErrorMessage = &errorMessage
	} else {
		submission.ErrorMessage = nil
	}
}

func timeoutSeconds(milliseconds int) int32 {
	if milliseconds <= 0 {
		return 1
	}
	seconds := (int64(milliseconds) + 999) / 1000
	if seconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(seconds)
}

func boundedInt32(value int) int32 {
	if value <= 0 {
		return 1
	}
	if int64(value) > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(value)
}

func boundedInt(value int64) int {
	if value < 0 {
		return 0
	}
	if uint64(value) > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

func firstDiagnostic(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return truncate(value, 4096)
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
