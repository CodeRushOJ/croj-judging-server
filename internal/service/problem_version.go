package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type ExecutionConfig struct {
	TimeLimitMillis int
	MemoryLimitMB   int
}

type versionLimits struct {
	TimeLimit   int  `json:"timeLimit"`
	MemoryLimit int  `json:"memoryLimit"`
	TotalScore  *int `json:"totalScore"`
}

type versionJudgeConfig struct {
	SpecialJudge         *bool   `json:"specialJudge"`
	SpecialJudgeCode     *string `json:"specialJudgeCode"`
	SpecialJudgeLanguage *string `json:"specialJudgeLanguage"`
	JudgeMode            *int    `json:"judgeMode"`
}

func ParseExecutionConfig(version *model.ProblemVersion) (ExecutionConfig, error) {
	if version == nil {
		return ExecutionConfig{}, fmt.Errorf("problem version is missing")
	}
	var limits versionLimits
	if err := decodeStrictSnapshot(version.LimitsJSON, &limits); err != nil {
		return ExecutionConfig{}, fmt.Errorf("invalid immutable limits_json: %w", err)
	}
	if limits.TimeLimit <= 0 || limits.TimeLimit > 86_400_000 || limits.MemoryLimit <= 0 || limits.MemoryLimit > 2_147_483_647 {
		return ExecutionConfig{}, fmt.Errorf("immutable execution limits are outside supported bounds")
	}
	if limits.TotalScore != nil && *limits.TotalScore <= 0 {
		return ExecutionConfig{}, fmt.Errorf("immutable totalScore must be positive when present")
	}
	var judge versionJudgeConfig
	if err := decodeStrictSnapshot(version.JudgeConfigJSON, &judge); err != nil {
		return ExecutionConfig{}, fmt.Errorf("invalid immutable judge_config_json: %w", err)
	}
	if judge.SpecialJudge == nil || judge.JudgeMode == nil {
		return ExecutionConfig{}, fmt.Errorf("immutable judge config is missing required fields")
	}
	if *judge.SpecialJudge {
		return ExecutionConfig{}, fmt.Errorf("special judge is not supported by hidden bundle v1")
	}
	if *judge.JudgeMode != int(model.JudgeModeACM) {
		return ExecutionConfig{}, fmt.Errorf("OI scoring is not supported by hidden bundle v1")
	}
	return ExecutionConfig{TimeLimitMillis: limits.TimeLimit, MemoryLimitMB: limits.MemoryLimit}, nil
}

func decodeStrictSnapshot(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("snapshot must contain exactly one JSON document")
	}
	return nil
}
