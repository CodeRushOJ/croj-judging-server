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

	// 1. 从数据库获取 Submission 详情 (使用 taskID 作为 SubmissionID)
	submission, err := s.db.GetSubmissionByID(taskID)
	if err != nil {
		// 如果获取 Submission 失败，记录错误并返回，以便 RocketMQ 重试
		return fmt.Errorf("failed to get submission %d from db: %w", taskID, err)
	}
	if submission == nil {
		// Submission 不存在，可能已经被处理或是一个无效 ID
		fmt.Printf("Submission %d not found, skipping processing.\n", taskID)
		return nil // 返回 nil，避免 RocketMQ 重试
	}

	// 检查 Submission 是否已经是最终状态，避免重复处理
	if submission.Status != model.StatusPending {
		fmt.Printf("Submission %d already processed or in progress (status: %d), skipping.\n", taskID, submission.Status)
		return nil // 返回 nil，避免 RocketMQ 重试
	}

	// (可选) 更新状态为 "Judging"
	// submission.Status = model.StatusJudging
	// if err := s.db.UpdateSubmission(submission); err != nil { // 假设有 UpdateSubmission 或调整 UpdateTask
	// 	fmt.Printf("Warning: Failed to update submission %d status to Judging: %v\n", taskID, err)
	// }

	// 检查 context 是否已取消
	select {
	case <-ctx.Done():
		fmt.Printf("Submission %d processing cancelled.\n", taskID)
		// 可选：更新状态为错误
		// submission.Status = model.StatusSystemError
		// errMsg := "Processing cancelled due to context"
		// submission.ErrorMessage = &errMsg
		// s.db.UpdateSubmission(submission) // 假设有 UpdateSubmission 或调整 UpdateTask
		return ctx.Err()
	default:
		// 继续执行
	}

	// 2. 通过调度器选择判题沙盒等逻辑 (如果需要)
	// ...

	// 3. **模拟判题过程，并更新 Submission 状态**
	//    (实际应用中这里会调用沙盒进行判题)
	fmt.Printf("Submission %d: Simulating successful judge, updating status to Accepted.\n", taskID)
	submission.Status = model.StatusAccepted
	simulatedTime := 123
	simulatedMemory := 1024
	submission.RunTime = &simulatedTime
	submission.Memory = &simulatedMemory
	judgeMsg := "Simulated Accepted"
	submission.JudgeInfo = &judgeMsg
	submission.ErrorMessage = nil

	// 4. 在单个事务中更新 Submission 结果并处理 Problem Accepted Count
	if err := s.db.UpdateSubmissionResultInTx(submission); err != nil {
		// 事务失败，可能是数据库错误或逻辑问题 (如 problem 不存在)
		// 记录严重错误，并返回错误以便 RocketMQ 重试（或者根据策略决定）
		fmt.Printf("Critical: Failed to complete transaction for submission %d: %v\n", taskID, err)
		return fmt.Errorf("transaction failed for submission %d: %w", taskID, err)
	}

	// 原来的更新逻辑已被合并到事务中
	/*
	// 更新 Submission 到数据库
	// 重要：确保 UpdateTask 能正确更新 Submission 表，或者替换为 UpdateSubmission
	if err := s.db.UpdateTask((*model.Task)(submission)); err != nil { // 注意这里的类型转换，如果 UpdateTask 参数是 *model.Task
		// 如果 Task 和 Submission 结构兼容或 UpdateTask 内部处理了，可以这样转换
		// 否则需要一个 UpdateSubmission 函数
		return fmt.Errorf("failed to update submission %d result in db: %w", taskID, err)
	}

	// 4. 如果判题结果是 Accepted，增加对应 Problem 的 Accepted Count
	if submission.Status == model.StatusAccepted {
		if err := s.db.IncrementProblemAcceptedCount(submission.ProblemID);
 err != nil {
			// 记录错误，但不一定需要阻塞主流程或导致消息重试
			// 因为主要判题流程已完成，这里是附属操作
			fmt.Printf("Warning: Failed to increment accepted count for problem %d after successful submission %d: %v\n", submission.ProblemID, taskID, err)
		}
	}
	*/

	fmt.Printf("Submission %d processed and transaction committed successfully.\n", taskID)
	return nil
}
