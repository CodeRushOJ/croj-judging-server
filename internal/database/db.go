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

// GetTaskByID 从数据库中获取任务
// 注意：根据之前的讨论，这个函数可能需要调整或替换为 GetSubmissionByID
// 如果 Task 和 Submission 是一回事，保留即可；如果是不同的表，需要确认。
func (d *Database) GetTaskByID(taskID int64) (*model.Task, error) {
	fmt.Printf("Getting task %d from database...\\n", taskID)
	task := &model.Task{}
	// 使用 GORM 查询，指定表名（虽然模型中定义了，但显式指定更清晰）
	// result := d.DB.Table(task.TableName()).First(task, taskID)
	// GORM v2 推荐直接使用模型查询
	result := d.DB.First(task, taskID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			fmt.Printf("Task %d not found in database.\\n", taskID)
			return nil, nil // 明确返回 nil, nil 表示未找到
		}
		return nil, fmt.Errorf("failed to get task %d: %w", taskID, result.Error)
	}
	fmt.Printf("Successfully retrieved task %d. Status: %d\\n", taskID, task.Status)
	return task, nil
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

// UpdateSubmission 更新数据库中的提交状态或结果
// 此函数现在只负责更新 Submission (Task) 本身，事务和 problem count 更新由 UpdateSubmissionResultInTx 处理
func (d *Database) UpdateSubmission(tx *gorm.DB, submission *model.Task) error { // 参数类型改为 *model.Task
	if submission == nil || submission.ID == 0 {
		return fmt.Errorf("invalid submission or submission ID for update")
	}
	// 注意：日志现在不打印状态，因为可能在事务中被多次调用
	// fmt.Printf("Updating submission %d in database... Status: %d\n", submission.ID, submission.Status)

	updates := map[string]interface{}{ // 确保字段名与 t_submission 表匹配
		"status":        submission.Status,
		"error_message": submission.ErrorMessage,
		"run_time":      submission.RunTime,
		"memory":        submission.Memory,
		"judge_info":    submission.JudgeInfo,
		"score":         submission.Score,
		// UpdateTime 会自动更新
	}

	// 使用传入的事务对象 tx 进行操作
	// 使用 Model 和 Where 定位记录，然后用 Updates 更新
	db := d.DB // 默认为原始 DB 连接
	if tx != nil { // 如果传入了事务对象，则使用事务对象
		db = tx
	}
	result := db.Model(&model.Task{}).Where("id = ?", submission.ID).Updates(updates) // 使用 model.Task{}

	if result.Error != nil {
		return fmt.Errorf("failed to update submission %d: %w", submission.ID, result.Error)
	}
	// 在事务性函数中检查 RowsAffected 可能意义不大，由调用者决定
	// if result.RowsAffected == 0 {
	// 	fmt.Printf("Warning: Update submission %d affected 0 rows. Record might not exist.\n", submission.ID)
	// }

	// fmt.Printf("Successfully updated submission %d.\n", submission.ID) // 日志移到事务函数中
	return nil
}

// UpdateSubmissionResultInTx 在单个事务中更新提交结果并增加问题通过次数（如果适用）
func (d *Database) UpdateSubmissionResultInTx(submission *model.Task) error { // 参数类型改为 *model.Task
	if submission == nil || submission.ID == 0 {
		return fmt.Errorf("invalid submission or submission ID for transactional update")
	}

	fmt.Printf("Starting transaction to update submission %d (Status: %d, ProblemID: %d)...\n", submission.ID, submission.Status, submission.ProblemID)

	tx := d.DB.Begin()
	if tx.Error != nil {
		return fmt.Errorf("failed to begin transaction for submission %d: %w", submission.ID, tx.Error)
	}

	// Defer a rollback in case of panic or error
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			// Re-panic or handle as needed
			panic(r)
		} else if tx.Error != nil {
			fmt.Printf("Rolling back transaction for submission %d due to error: %v\n", submission.ID, tx.Error)
			tx.Rollback()
		}
	}()

	// 1. 更新 Submission 表 (实际是 Task)
	if err := d.UpdateSubmission(tx, submission); err != nil { // 传入事务对象 tx
		tx.Error = err // 标记事务错误，触发 defer 中的 Rollback
		return fmt.Errorf("failed to update submission %d within transaction: %w", submission.ID, err)
	}

	// 2. 如果状态是 Accepted，则更新 Problem 表的 accepted_count
	if submission.Status == model.StatusAccepted {
		fmt.Printf("Submission %d is Accepted, incrementing accepted count for problem %d within transaction...\n", submission.ID, submission.ProblemID)
		// 使用事务对象 tx 更新 t_problem
		result := tx.Model(&model.Problem{}).Where("id = ?", submission.ProblemID).
			UpdateColumn("accepted_count", gorm.Expr("accepted_count + 1")) // 直接 +1 也可以

		if result.Error != nil {
			tx.Error = result.Error // 标记事务错误
			return fmt.Errorf("failed to increment accepted_count for problem %d within transaction: %w", submission.ProblemID, result.Error)
		}
		if result.RowsAffected == 0 {
			// 在事务中，如果 problem 不存在，这应该算是一个错误，需要回滚
			err := fmt.Errorf("problem %d not found when trying to increment accepted_count within transaction", submission.ProblemID)
			tx.Error = err // 标记事务错误
			return err
		}
		fmt.Printf("Successfully incremented accepted count for problem %d within transaction.\n", submission.ProblemID)
	}

	// 3. 提交事务
	if err := tx.Commit().Error; err != nil {
		tx.Error = err // 确保 defer 中的 Rollback 能捕获到 Commit 错误
		return fmt.Errorf("failed to commit transaction for submission %d: %w", submission.ID, err)
	}

	fmt.Printf("Successfully committed transaction for submission %d.\n", submission.ID)
	return nil
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
