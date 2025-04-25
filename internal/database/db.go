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
func (d *Database) GetTaskByID(taskID int64) (*model.Task, error) {
	fmt.Printf("Getting task %d from database...\n", taskID)
	task := &model.Task{}
	// 使用 GORM 查询，指定表名（虽然模型中定义了，但显式指定更清晰）
	// result := d.DB.Table(task.TableName()).First(task, taskID)
	// GORM v2 推荐直接使用模型查询
	result := d.DB.First(task, taskID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			fmt.Printf("Task %d not found in database.\n", taskID)
			return nil, nil // 明确返回 nil, nil 表示未找到
		}
		return nil, fmt.Errorf("failed to get task %d: %w", taskID, result.Error)
	}
	fmt.Printf("Successfully retrieved task %d. Status: %d\n", taskID, task.Status)
	return task, nil
}

// UpdateTask 更新数据库中的任务状态或结果
// 这里只更新部分字段以提高效率，特别是状态和错误信息
func (d *Database) UpdateTask(task *model.Task) error {
	if task == nil || task.ID == 0 {
		return fmt.Errorf("invalid task or task ID for update")
	}
	fmt.Printf("Updating task %d in database... Status: %d\n", task.ID, task.Status)

	// 使用 Select 更新指定字段，避免覆盖其他可能由别的服务更新的字段
	// 或者使用 Omit 来排除不想更新的字段
	// 这里我们仅更新 Status 和 ErrorMessage (如果需要的话)
	// 注意：GORM 会自动处理 UpdateTime 的更新（如果模型标签设置正确）
	updates := map[string]interface{}{
		"status":        task.Status,
		"error_message": task.ErrorMessage, // 即使是 nil 也会被更新
		"run_time":      task.RunTime,
		"memory":        task.Memory,
		"judge_info":    task.JudgeInfo,
		"score":         task.Score,
		// UpdateTime 会自动更新
	}

	// 使用 Model 和 Where 定位记录，然后用 Updates 更新
	result := d.DB.Model(&model.Task{}).Where("id = ?", task.ID).Updates(updates)

	// result := d.DB.Save(task) // Save 会更新所有字段，可能不是最优
	if result.Error != nil {
		return fmt.Errorf("failed to update task %d: %w", task.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		fmt.Printf("Warning: Update task %d affected 0 rows. Record might not exist or no changes needed.\n", task.ID)
		// 根据业务逻辑决定是否需要报错
	}

	fmt.Printf("Successfully updated task %d.\n", task.ID)
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
