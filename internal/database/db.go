package database

import (
	"errors"
	"fmt"

	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Database 结构体，封装数据库操作
type Database struct {
	DB *gorm.DB
}

// NewDatabase 创建一个新的数据库连接
func NewDatabase(cfg config.DatabaseConfig) (*Database, error) {
	fmt.Println("Connecting to database...")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.User,
		cfg.Password,
		cfg.Host,
		cfg.Port,
		cfg.Name,
	)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}
	return &Database{DB: db}, nil
	// return &Database{}, nil // 临时返回
}

// GetSubmissionByID 从数据库中获取提交记录
// 函数名保留，但内部使用 model.Task
func (d *Database) GetSubmissionByID(submissionID int64) (*model.Task, error) {
	fmt.Printf("Getting submission %d from database...\\n", submissionID)
	submission := &model.Task{} // 使用 model.Task
	// model.Task 结构体已通过 TableName() 指定映射到 t_submission 表
	result := d.DB.First(submission, submissionID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			fmt.Printf("Submission %d not found in database.\\n", submissionID)
			return nil, nil // 明确返回 nil, nil 表示未找到
		}
		return nil, fmt.Errorf("failed to get submission %d: %w", submissionID, result.Error)
	}
	fmt.Printf("Successfully retrieved submission %d. ProblemID: %d, Status: %d\\n", submissionID, submission.ProblemID, submission.Status)
	return submission, nil
}

// GetProblemByID returns the execution limits that are snapshotted into the
// sandbox compatibility request. Versioned immutable problem data remains a
// follow-up contract with the backend.
func (d *Database) GetProblemByID(problemID int64) (*model.Problem, error) {
	problem := &model.Problem{}
	result := d.DB.First(problem, problemID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get problem %d: %w", problemID, result.Error)
	}
	return problem, nil
}

// Close 关闭数据库连接
func (d *Database) Close() error {
	fmt.Println("Closing database connection...")
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
	// return nil // 临时返回
}
