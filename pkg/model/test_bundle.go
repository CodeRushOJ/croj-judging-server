package model

import (
	"time"

	"gorm.io/datatypes"
)

type TestBundle struct {
	ID               int64          `gorm:"column:id;primaryKey"`
	ProblemVersionID int64          `gorm:"column:problem_version_id"`
	ObjectKey        string         `gorm:"column:object_key"`
	SHA256           string         `gorm:"column:sha256"`
	SizeBytes        int64          `gorm:"column:size_bytes"`
	ManifestJSON     datatypes.JSON `gorm:"column:manifest_json"`
	CreatedAt        time.Time      `gorm:"column:created_at"`
}

func (TestBundle) TableName() string { return "t_test_bundle" }
