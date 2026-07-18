package service

import (
	"testing"

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
	if config.TimeLimitMillis != 2500 || config.MemoryLimitMB != 128 {
		t.Fatalf("config = %+v", config)
	}
}

func TestParseExecutionConfigRejectsInvalidOrUnsupportedSnapshot(t *testing.T) {
	validLimits := `{"timeLimit":1000,"memoryLimit":64,"totalScore":100}`
	validJudge := `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`
	tests := map[string][2]string{
		"unknown limits field": {`{"timeLimit":1000,"memoryLimit":64,"totalScore":100,"cpu":2}`, validJudge},
		"invalid time":         {`{"timeLimit":0,"memoryLimit":64,"totalScore":100}`, validJudge},
		"invalid memory":       {`{"timeLimit":1000,"memoryLimit":0,"totalScore":100}`, validJudge},
		"special judge":        {validLimits, `{"specialJudge":true,"specialJudgeCode":"x","specialJudgeLanguage":"go","judgeMode":0}`},
		"OI":                   {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":1}`},
		"unknown judge field":  {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0,"checker":"exact"}`},
		"missing specialJudge": {validLimits, `{"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`},
		"missing judgeMode":    {validLimits, `{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null}`},
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
