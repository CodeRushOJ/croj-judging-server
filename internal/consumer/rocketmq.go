package consumer

import (
	"context"
	"fmt"

	// "github.com/CodeRushOJ/croj-judging-server/internal/service"
	// "github.com/CodeRushOJ/croj-judging-server/pkg/config"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
	"github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

// RocketMQConsumer 结构体，包含消费者实例和判题服务
type RocketMQConsumer struct {
	consumer     rocketmq.PushConsumer
	judgeService *service.JudgeService
	topic        string
}

// NewRocketMQConsumer 创建一个新的 RocketMQ 消费者
func NewRocketMQConsumer(cfg config.RocketMQConfig, judgeService *service.JudgeService) (*RocketMQConsumer, error) {
	fmt.Println("Initializing RocketMQ Consumer...")

	// 注意：NameServer 地址需要是 []string 类型
	namesrvAddr := []string{cfg.NameServer}
	if cfg.NameServer == "" {
		return nil, fmt.Errorf("rocketmq name-server is not configured")
	}
	if cfg.Consumer.Group == "" {
		return nil, fmt.Errorf("rocketmq consumer group is not configured")
	}
	if cfg.Topic == "" {
		return nil, fmt.Errorf("rocketmq topic is not configured")
	}

	c, err := rocketmq.NewPushConsumer(
		consumer.WithNameServer(namesrvAddr),
		consumer.WithGroupName(cfg.Consumer.Group), // 使用嵌套的 Group
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create rocketmq consumer: %w", err)
	}

	rc := &RocketMQConsumer{
		consumer:     c,
		judgeService: judgeService,
		topic:        cfg.Topic,
	}

	err = c.Subscribe(cfg.Topic, consumer.MessageSelector{}, rc.handleMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe topic %s: %w", cfg.Topic, err)
	}

	return rc, nil
	// return &RocketMQConsumer{}, nil // 临时返回
}

// handleMessage 处理接收到的消息
func (rc *RocketMQConsumer) handleMessage(ctx context.Context, msgs ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
	fmt.Println("Handling RocketMQ messages...")
	for _, msg := range msgs {
		taskID := string(msg.Body)
		fmt.Printf("Received task ID: %s\n", taskID)

		// 调用 JudgeService 处理任务
		err := rc.judgeService.ProcessTask(ctx, taskID)
		if err != nil {
			fmt.Printf("Error processing task %s: %v\n", taskID, err)
			// 根据错误类型决定是否重试，例如返回 ConsumeRetryLater
			return consumer.ConsumeRetryLater, nil
		}
	}
	return consumer.ConsumeSuccess, nil
}

// Start 启动消费者
func (rc *RocketMQConsumer) Start() error {
	fmt.Println("Starting RocketMQ Consumer...")
	return rc.consumer.Start()
	// return nil // 临时返回
}

// Shutdown 关闭消费者
func (rc *RocketMQConsumer) Shutdown() error {
	fmt.Println("Shutting down RocketMQ Consumer...")
	return rc.consumer.Shutdown()
	// return nil // 临时返回
}
