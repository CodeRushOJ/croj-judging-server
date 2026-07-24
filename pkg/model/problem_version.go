package model

import (
	"time"

	"gorm.io/datatypes"
)

type ProblemVersion struct {
	ID              int64          `gorm:"column:id;primaryKey"`
	ProblemID       int64          `gorm:"column:problem_id"`
	VersionNo       int            `gorm:"column:version_no"`
	State           string         `gorm:"column:state"`
	StatementJSON   datatypes.JSON `gorm:"column:statement_json"`
	LimitsJSON      datatypes.JSON `gorm:"column:limits_json"`
	JudgeConfigJSON datatypes.JSON `gorm:"column:judge_config_json"`
	CreatedBy       int64          `gorm:"column:created_by"`
	CreatedAt       time.Time      `gorm:"column:created_at"`
	PublishedAt     *time.Time     `gorm:"column:published_at"`
}

func (ProblemVersion) TableName() string { return "t_problem_version" }
