package service

import (
	"context"
	"fmt"
	"strconv"

	"github.com/CodeRushOJ/croj-judging-server/internal/database"
	"github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	"github.com/CodeRushOJ/croj-judging-server/internal/scheduler"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

// JudgeService 结构体，包含判题的核心逻辑
type JudgeService struct {
	db            *database.Database
	scheduler     *scheduler.Scheduler
	sandboxClient *sandbox.Client
}

// NewJudgeService 创建一个新的判题服务
func NewJudgeService(db *database.Database, scheduler *scheduler.Scheduler, sandboxClient *sandbox.Client) *JudgeService {
	return &JudgeService{
		db:            db,
		scheduler:     scheduler,
		sandboxClient: sandboxClient,
	}
}

// ProcessTask 处理单个判题任务
// 添加 context.Context 参数以支持超时和取消
func (s *JudgeService) ProcessTask(ctx context.Context, taskIDStr string) error {
	fmt.Printf("Processing task: %s\n", taskIDStr)

	// 将字符串 ID 转换为 int64
	taskID, err := strconv.ParseInt(taskIDStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid task ID format '%s': %w", taskIDStr, err)
	}

	// 1. 从数据库获取任务详情
	task, err := s.db.GetTaskByID(taskID)
	if err != nil {
		return fmt.Errorf("failed to get task %d from db: %w", taskID, err)
	}
	if task == nil {
		// 任务不存在，可能已经被处理或是一个无效 ID
		fmt.Printf("Task %d not found, skipping processing.\n", taskID)
		return nil // 返回 nil，避免 RocketMQ 重试
	}

	// 检查任务是否已经是最终状态，避免重复处理
	if task.Status != model.StatusPending {
		fmt.Printf("Task %d already processed or in progress (status: %d), skipping.\n", taskID, task.Status)
		return nil // 返回 nil，避免 RocketMQ 重试
	}

	// (可选) 可以先更新任务状态为 "Judging" 或类似状态，但这需要数据库支持或特殊处理
	// task.Status = model.StatusJudging // 假设有此状态
	// if err := s.db.UpdateTask(task); err != nil {
	// 	fmt.Printf("Warning: Failed to update task %d status to Judging: %v\n", taskID, err)
	// }

	// 检查 context 是否已取消
	select {
	case <-ctx.Done():
		fmt.Printf("Task %d processing cancelled.\n", taskID)
		// 可以选择是否将状态更新为错误状态
		// task.Status = model.StatusSystemError
		// task.ErrorMessage = "Processing cancelled due to context" // 使用指针
		// s.db.UpdateTask(task)
		return ctx.Err()
	default:
		// 继续执行
	}

	// 2. 通过调度器选择一个判题沙盒 (如果需要与沙盒交互才需要)
	// sandboxAddr, err := s.scheduler.SelectSandbox()
	// if err != nil {
	// 	task.Status = model.StatusSystemError
	// 	errMsg := fmt.Sprintf("Failed to select sandbox: %v", err)
	// 	task.ErrorMessage = &errMsg
	// 	s.db.UpdateTask(task) // 尽力更新
	// 	return fmt.Errorf("failed to select sandbox for task %d: %w", taskID, err)
	// }
	// fmt.Printf("Task %d would be dispatched to sandbox %s.\n", taskID, sandboxAddr)

	// 3. **测试：直接修改数据库状态为 Accepted**
	fmt.Printf("Task %d: Simulating successful judge, updating status to Accepted.\n", taskID)
	task.Status = model.StatusAccepted
	// 可以设置一些模拟结果
	simulatedTime := 123
	simulatedMemory := 1024
	task.RunTime = &simulatedTime
	task.Memory = &simulatedMemory
	judgeMsg := "Simulated Accepted"
	task.JudgeInfo = &judgeMsg // 假设 JudgeInfo 存储简单消息
	task.ErrorMessage = nil    // 清除之前的错误信息

	if err := s.db.UpdateTask(task); err != nil {
		// 数据库更新失败是严重问题
		return fmt.Errorf("failed to update task %d result in db: %w", taskID, err)
	}

	fmt.Printf("Task %d processed and updated to Accepted successfully.\n", taskID)
	return nil
}
