package consumer

import (
	"context"
	"fmt"
	"log"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

// RocketMQConsumer 结构体，包含消费者实例和判题服务
type RocketMQConsumer struct {
	consumer  rocketmq.PushConsumer
	processor EventProcessor
	topic     string
}

type EventProcessor interface {
	ProcessEvent(context.Context, model.SubmissionRequested) error
}

// NewRocketMQConsumer 创建一个新的 RocketMQ 消费者
func NewRocketMQConsumer(cfg config.RocketMQConfig, processor EventProcessor) (*RocketMQConsumer, error) {
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
	if processor == nil {
		return nil, fmt.Errorf("judge event processor is not configured")
	}
	if cfg.Consumer.MaxReconsumeTimes <= 0 {
		return nil, fmt.Errorf("rocketmq max reconsume times must be positive")
	}

	c, err := rocketmq.NewPushConsumer(
		consumer.WithNameServer(namesrvAddr),
		consumer.WithGroupName(cfg.Consumer.Group), // 使用嵌套的 Group
		consumer.WithMaxReconsumeTimes(cfg.Consumer.MaxReconsumeTimes),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create rocketmq consumer: %w", err)
	}

	rc := &RocketMQConsumer{
		consumer:  c,
		processor: processor,
		topic:     cfg.Topic,
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
	for _, msg := range msgs {
		event, err := DecodeSubmissionRequested(msg.Body)
		if err != nil {
			log.Printf("discarding invalid SubmissionRequested message: %v", err)
			continue
		}
		if err := rc.processor.ProcessEvent(ctx, event); err != nil {
			if callback.IsPermanent(err) {
				log.Printf("judge event %s submission=%d attempt=%d rejected permanently: %v",
					event.EventID, event.SubmissionID, event.AttemptNo, err)
				continue
			}
			log.Printf("judge event %s submission=%d attempt=%d will retry: %v",
				event.EventID, event.SubmissionID, event.AttemptNo, err)
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
