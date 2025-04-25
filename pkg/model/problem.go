package model

import (
	"time"

	"gorm.io/datatypes" // Need this for JSON type
)

// ProblemDifficulty 定义题目难度常量
type ProblemDifficulty int8

const (
	DifficultyEasy   ProblemDifficulty = 1
	DifficultyMedium ProblemDifficulty = 2
	DifficultyHard   ProblemDifficulty = 3
)

// JudgeMode 定义判题模式常量
type JudgeMode int8

const (
	JudgeModeACM JudgeMode = 0
	JudgeModeOI  JudgeMode = 1
)

// ProblemStatus 定义题目状态常量
type ProblemStatus int8

const (
	StatusPublic   ProblemStatus = 0
	StatusPrivate  ProblemStatus = 1
	StatusContest  ProblemStatus = 2
)

// Problem 对应数据库中的 t_problem 表
type Problem struct {
	ID                   int64            `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ProblemNo            string           `gorm:"column:problem_no;type:varchar(20);not null;uniqueIndex:idx_problem_no" json:"problem_no"`
	Title                string           `gorm:"column:title;type:varchar(255);not null" json:"title"`
	Description          string           `gorm:"column:description;type:text;not null" json:"description"`
	InputDescription     string           `gorm:"column:input_description;type:text;not null" json:"input_description"`
	OutputDescription    string           `gorm:"column:output_description;type:text;not null" json:"output_description"`
	Hints                datatypes.JSON   `gorm:"column:hints;type:json" json:"hints,omitempty"`                 // JSON 类型
	Samples              datatypes.JSON   `gorm:"column:samples;type:json" json:"samples,omitempty"`               // JSON 类型
	TimeLimit            int              `gorm:"column:time_limit;not null;default:1000" json:"time_limit"`     // ms
	MemoryLimit          int              `gorm:"column:memory_limit;not null;default:256" json:"memory_limit"`   // MB
	Difficulty           ProblemDifficulty `gorm:"column:difficulty;type:tinyint;not null;default:2;index:idx_difficulty" json:"difficulty"`
	IsSpecialJudge       bool             `gorm:"column:is_special_judge;type:tinyint;not null;default:0" json:"is_special_judge"` // tinyint(1) often maps to bool
	SpecialJudgeCode     *string          `gorm:"column:special_judge_code;type:text" json:"special_judge_code,omitempty"` // Nullable text
	SpecialJudgeLanguage *string          `gorm:"column:special_judge_language;type:varchar(50)" json:"special_judge_language,omitempty"` // Nullable varchar
	JudgeMode            JudgeMode        `gorm:"column:judge_mode;type:tinyint;not null;default:0" json:"judge_mode"`
	TotalScore           *int             `gorm:"column:total_score;default:100" json:"total_score,omitempty"` // Nullable int for OI mode
	Source               *string          `gorm:"column:source;type:varchar(255)" json:"source,omitempty"`     // Nullable varchar
	CreateUserID         int64            `gorm:"column:create_user_id;not null;index:idx_create_user_id" json:"create_user_id"`
	SubmitCount          int              `gorm:"column:submit_count;not null;default:0" json:"submit_count"`
	AcceptedCount        int              `gorm:"column:accepted_count;not null;default:0" json:"accepted_count"` // This is the field we need to increment
	Status               ProblemStatus    `gorm:"column:status;type:tinyint;not null;default:0;index:idx_status" json:"status"`
	CreateTime           time.Time        `gorm:"column:create_time;not null;default:CURRENT_TIMESTAMP;index:idx_create_time" json:"create_time"`
	UpdateTime           time.Time        `gorm:"column:update_time;not null;default:CURRENT_TIMESTAMP;onUpdate:CURRENT_TIMESTAMP" json:"update_time"`
	IsDeleted            bool             `gorm:"column:is_deleted;type:tinyint;not null;default:0" json:"-"` // Often excluded from JSON responses
}

// TableName 指定 GORM 使用的表名
func (Problem) TableName() string {
	return "t_problem"
} 