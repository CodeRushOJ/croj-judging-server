package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type ExecutionConfig struct {
	TimeLimitMillis      int
	MemoryLimitMB        int
	JudgeMode            bundle.JudgeMode
	Checker              bundle.Checker
	CheckerPinned        bool
	TotalScore           int
	SpecialJudge         bool
	SpecialJudgeLanguage string
	SpecialJudgeSource   string
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
	Checker              *string `json:"checker"`
	Difficulty           *int    `json:"difficulty"`
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
	if limits.TotalScore != nil && (*limits.TotalScore <= 0 || *limits.TotalScore > 1_000_000_000) {
		return ExecutionConfig{}, fmt.Errorf("immutable totalScore must be positive when present")
	}
	var judge versionJudgeConfig
	if err := decodeStrictSnapshot(version.JudgeConfigJSON, &judge); err != nil {
		return ExecutionConfig{}, fmt.Errorf("invalid immutable judge_config_json: %w", err)
	}
	if judge.SpecialJudge == nil || judge.JudgeMode == nil {
		return ExecutionConfig{}, fmt.Errorf("immutable judge config is missing required fields")
	}
	config := ExecutionConfig{
		TimeLimitMillis: limits.TimeLimit,
		MemoryLimitMB:   limits.MemoryLimit,
		JudgeMode:       bundle.JudgeModeACM,
		Checker:         bundle.CheckerExact,
	}
	switch *judge.JudgeMode {
	case int(model.JudgeModeACM):
	case int(model.JudgeModeOI):
		if limits.TotalScore == nil {
			return ExecutionConfig{}, fmt.Errorf("immutable OI totalScore is required")
		}
		config.JudgeMode = bundle.JudgeModeOI
		config.TotalScore = *limits.TotalScore
	default:
		return ExecutionConfig{}, fmt.Errorf("immutable judgeMode is unsupported")
	}
	if *judge.SpecialJudge {
		if judge.SpecialJudgeCode == nil || judge.SpecialJudgeLanguage == nil ||
			strings.TrimSpace(*judge.SpecialJudgeCode) == "" || len([]byte(*judge.SpecialJudgeCode)) > 4<<20 {
			return ExecutionConfig{}, fmt.Errorf("immutable special judge source is missing or too large")
		}
		if _, ok := judgecontract.ResolveLanguage(*judge.SpecialJudgeLanguage); !ok {
			return ExecutionConfig{}, fmt.Errorf("immutable special judge language is unsupported")
		}
		config.SpecialJudge = true
		config.SpecialJudgeLanguage = *judge.SpecialJudgeLanguage
		config.SpecialJudgeSource = *judge.SpecialJudgeCode
		config.Checker = bundle.CheckerSpecial
		config.CheckerPinned = true
	} else if judge.SpecialJudgeCode != nil || judge.SpecialJudgeLanguage != nil {
		return ExecutionConfig{}, fmt.Errorf("immutable non-special judge contains stale checker fields")
	}
	if judge.Checker != nil {
		checker := bundle.Checker(*judge.Checker)
		if !judgecontract.IsCanonicalChecker(checker) {
			return ExecutionConfig{}, fmt.Errorf("immutable checker is unsupported")
		}
		if (*judge.SpecialJudge && checker != bundle.CheckerSpecial) ||
			(!*judge.SpecialJudge && checker == bundle.CheckerSpecial) {
			return ExecutionConfig{}, fmt.Errorf("immutable checker disagrees with specialJudge")
		}
		config.Checker = checker
		config.CheckerPinned = true
	}
	return config, nil
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
