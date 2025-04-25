package model

import "time"

// SubmissionStatus 定义提交状态常量
type SubmissionStatus int

const (
	StatusPending             SubmissionStatus = 0 // 排队中
	StatusAccepted            SubmissionStatus = 1 // 已通过
	StatusCompileError        SubmissionStatus = 2 // 编译错误
	StatusWrongAnswer         SubmissionStatus = 3 // 答案错误
	StatusTimeLimitExceeded   SubmissionStatus = 4 // 运行超时
	StatusMemoryLimitExceeded SubmissionStatus = 5 // 内存超限
	StatusRuntimeError        SubmissionStatus = 6 // 运行错误
	StatusSystemError         SubmissionStatus = 7 // 系统错误
	// 可以根据需要添加内部状态，但这需要数据库模式支持或在服务逻辑中处理
	// StatusJudging          SubmissionStatus = 10 // 判题中 (内部状态)
	// StatusDispatched       SubmissionStatus = 11 // 已分发 (内部状态)
)

// Task 对应数据库中的 t_submission 表
type Task struct {
	ID           int64            `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ProblemID    int64            `gorm:"column:problem_id;not null" json:"problem_id"`
	UserID       int64            `gorm:"column:user_id;not null" json:"user_id"`
	Language     string           `gorm:"column:language;size:20;not null" json:"language"`
	Code         string           `gorm:"column:code;type:text;not null" json:"code"`
	Status       SubmissionStatus `gorm:"column:status;not null;default:0" json:"status"`
	RunTime      *int             `gorm:"column:run_time" json:"run_time,omitempty"`                     // 使用指针以区分 0 和 NULL
	Memory       *int             `gorm:"column:memory" json:"memory,omitempty"`                         // 使用指针以区分 0 和 NULL
	JudgeInfo    *string          `gorm:"column:judge_info;type:text" json:"judge_info,omitempty"`       // 使用指针处理 NULL
	Score        *int             `gorm:"column:score" json:"score,omitempty"`                           // 使用指针处理 NULL
	ErrorMessage *string          `gorm:"column:error_message;type:text" json:"error_message,omitempty"` // 使用指针处理 NULL
	CreateTime   time.Time        `gorm:"column:create_time;not null;default:CURRENT_TIMESTAMP" json:"create_time"`
	UpdateTime   time.Time        `gorm:"column:update_time;not null;default:CURRENT_TIMESTAMP;onUpdate:CURRENT_TIMESTAMP" json:"update_time"`
	// IsDeleted field is omitted for simplicity, assuming soft deletes are handled elsewhere or not needed for this service
}

// TableName 指定 GORM 使用的表名
func (Task) TableName() string {
	return "t_submission"
}

// JudgeResult 表示判题沙盒返回的结果
type JudgeResult struct {
	Status     string `json:"status"`      // 判题状态 (字符串形式)
	Message    string `json:"message"`     // 判题信息
	TimeUsed   int64  `json:"time_used"`   // 时间消耗 (ms)
	MemoryUsed int64  `json:"memory_used"` // 内存消耗 (KB)
	// 可能还有编译输出、运行输出、错误详情等
}
