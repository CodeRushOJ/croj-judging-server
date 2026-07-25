package service

import (
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"gorm.io/datatypes"
)

func TestParseExecutionConfigUsesImmutableVersionLimits(t *testing.T) {
	version := &model.ProblemVersion{
		LimitsJSON:      datatypes.JSON([]byte(`{"timeLimit":2500,"memoryLimit":128,"totalScore":100}`)),
		JudgeConfigJSON: datatypes.JSON([]byte(`{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`)),
	}
	config, err := ParseExecutionConfig(version)
	if err != nil {
		t.Fatal(err)
	}
	if config.TimeLimitMillis != 2500 || config.MemoryLimitMB != 128 ||
		config.JudgeMode != bundle.JudgeModeACM || config.TotalScore != 0 ||
		config.Checker != bundle.CheckerExact || config.CheckerPinned || config.SpecialJudge {
		t.Fatalf("config = %+v", config)
	}
}

func TestParseExecutionConfigAcceptsBackendProjectionFieldsAndCanonicalChecker(t *testing.T) {
	config, err := ParseExecutionConfig(&model.ProblemVersion{
		LimitsJSON: datatypes.JSON([]byte(`{"timeLimit":2500,"memoryLimit":128,"totalScore":100}`)),
		JudgeConfigJSON: datatypes.JSON([]byte(
			`{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":1,"checker":"token","difficulty":2}`,
		)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.JudgeMode != bundle.JudgeModeOI || config.Checker != bundle.CheckerToken || !config.CheckerPinned {
		t.Fatalf("config = %+v", config)
	}
}

func TestParseExecutionConfigSupportsImmutableOIAndSpecialJudgeSnapshots(t *testing.T) {
	tests := map[string]struct {
		limits string
		judge  string
		check  func(ExecutionConfig) bool
	}{
		"OI": {
			limits: `{"timeLimit":1000,"memoryLimit":64,"totalScore":100}`,
			judge:  `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":1}`,
			check: func(config ExecutionConfig) bool {
				return config.JudgeMode == bundle.JudgeModeOI && config.TotalScore == 100 && !config.SpecialJudge
			},
		},
		"special": {
			limits: `{"timeLimit":1000,"memoryLimit":64,"totalScore":100}`,
			judge:  `{"specialJudge":true,"specialJudgeCode":"package main","specialJudgeLanguage":"go","judgeMode":0}`,
			check: func(config ExecutionConfig) bool {
				return config.JudgeMode == bundle.JudgeModeACM && config.SpecialJudge &&
					config.SpecialJudgeLanguage == "go" && config.SpecialJudgeSource == "package main"
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			config, err := ParseExecutionConfig(&model.ProblemVersion{
				LimitsJSON: datatypes.JSON(test.limits), JudgeConfigJSON: datatypes.JSON(test.judge),
			})
			if err != nil || !test.check(config) {
				t.Fatalf("config=%+v error=%v", config, err)
			}
		})
	}
}

func TestParseExecutionConfigRejectsInvalidSnapshot(t *testing.T) {
	validLimits := `{"timeLimit":1000,"memoryLimit":64,"totalScore":100}`
	validJudge := `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`
	tests := map[string][2]string{
		"unknown limits field":         {`{"timeLimit":1000,"memoryLimit":64,"totalScore":100,"cpu":2}`, validJudge},
		"invalid time":                 {`{"timeLimit":0,"memoryLimit":64,"totalScore":100}`, validJudge},
		"invalid memory":               {`{"timeLimit":1000,"memoryLimit":0,"totalScore":100}`, validJudge},
		"OI missing score":             {`{"timeLimit":1000,"memoryLimit":64,"totalScore":null}`, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":1}`},
		"unknown judge mode":           {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":2}`},
		"special no code":              {validLimits, `{"specialJudge":true,"specialJudgeCode":null,"specialJudgeLanguage":"go","judgeMode":0}`},
		"special no language":          {validLimits, `{"specialJudge":true,"specialJudgeCode":"x","specialJudgeLanguage":null,"judgeMode":0}`},
		"special bad language":         {validLimits, `{"specialJudge":true,"specialJudgeCode":"x","specialJudgeLanguage":"ruby","judgeMode":0}`},
		"stale checker fields":         {validLimits, `{"specialJudge":false,"specialJudgeCode":"x","specialJudgeLanguage":"go","judgeMode":0}`},
		"unknown checker":              {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0,"checker":"custom","difficulty":1}`},
		"special checker mismatch":     {validLimits, `{"specialJudge":true,"specialJudgeCode":"x","specialJudgeLanguage":"go","judgeMode":0,"checker":"exact","difficulty":1}`},
		"non-special checker mismatch": {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0,"checker":"special","difficulty":1}`},
		"invalid difficulty":           {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0,"checker":"exact","difficulty":"hard"}`},
		"unknown judge field":          {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0,"checker":"exact","difficulty":1,"extra":true}`},
		"missing specialJudge":         {validLimits, `{"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`},
		"missing judgeMode":            {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null}`},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			version := &model.ProblemVersion{LimitsJSON: datatypes.JSON(values[0]), JudgeConfigJSON: datatypes.JSON(values[1])}
			if _, err := ParseExecutionConfig(version); err == nil {
				t.Fatal("expected immutable config error")
			}
		})
	}
}
