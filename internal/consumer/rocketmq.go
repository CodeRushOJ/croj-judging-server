package consumer

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

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

type hostLookup interface {
	LookupHost(context.Context, string) ([]string, error)
}

type nameServerEndpoint struct {
	host      string
	port      string
	ipLiteral bool
}

// rocketMQNameServerResolver allows RocketMQ to consume Kubernetes Service DNS
// names while still giving the client the IP:port values required by v2.1.2.
// RocketMQ refreshes NsResolver in the background, so a Service endpoint change
// is picked up without restarting the judge.
type rocketMQNameServerResolver struct {
	endpoints   []nameServerEndpoint
	lookup      hostLookup
	timeout     time.Duration
	description string

	mu       sync.RWMutex
	lastGood []string
}

func newRocketMQNameServerResolver(raw string, lookup hostLookup) (*rocketMQNameServerResolver, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("rocketmq name-server is not configured")
	}
	if lookup == nil {
		lookup = net.DefaultResolver
	}

	parts := strings.Split(raw, ";")
	endpoints := make([]nameServerEndpoint, 0, len(parts))
	for _, part := range parts {
		address := strings.TrimSpace(part)
		if address == "" {
			return nil, fmt.Errorf("rocketmq name-server contains an empty endpoint")
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid rocketmq name-server endpoint %q: %w", address, err)
		}
		if host == "" || port == "" {
			return nil, fmt.Errorf("invalid rocketmq name-server endpoint %q", address)
		}
		endpoints = append(endpoints, nameServerEndpoint{
			host:      host,
			port:      port,
			ipLiteral: net.ParseIP(host) != nil,
		})
	}

	return &rocketMQNameServerResolver{
		endpoints:   endpoints,
		lookup:      lookup,
		timeout:     5 * time.Second,
		description: "DNS-aware RocketMQ name-server resolver",
	}, nil
}

func (resolver *rocketMQNameServerResolver) Resolve() []string {
	ctx, cancel := context.WithTimeout(context.Background(), resolver.timeout)
	defer cancel()

	resolved := make(map[string]struct{})
	for _, endpoint := range resolver.endpoints {
		if endpoint.ipLiteral {
			resolved[net.JoinHostPort(endpoint.host, endpoint.port)] = struct{}{}
			continue
		}

		addresses, err := resolver.lookup.LookupHost(ctx, endpoint.host)
		if err != nil {
			log.Printf("rocketmq name-server DNS refresh failed for %s: %v", endpoint.host, err)
			return resolver.lastKnownGood()
		}
		if len(addresses) == 0 {
			log.Printf("rocketmq name-server DNS refresh returned no addresses for %s", endpoint.host)
			return resolver.lastKnownGood()
		}
		for _, address := range addresses {
			ip := net.ParseIP(strings.TrimSpace(address))
			if ip == nil {
				log.Printf("rocketmq name-server DNS refresh returned invalid IP for %s", endpoint.host)
				return resolver.lastKnownGood()
			}
			resolved[net.JoinHostPort(ip.String(), endpoint.port)] = struct{}{}
		}
	}

	if len(resolved) == 0 {
		return resolver.lastKnownGood()
	}
	addresses := make([]string, 0, len(resolved))
	for address := range resolved {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)

	resolver.mu.Lock()
	resolver.lastGood = append(resolver.lastGood[:0], addresses...)
	resolver.mu.Unlock()
	return append([]string(nil), addresses...)
}

func (resolver *rocketMQNameServerResolver) Description() string {
	return resolver.description
}

func (resolver *rocketMQNameServerResolver) lastKnownGood() []string {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return append([]string(nil), resolver.lastGood...)
}

// NewRocketMQConsumer 创建一个新的 RocketMQ 消费者
func NewRocketMQConsumer(cfg config.RocketMQConfig, processor EventProcessor) (*RocketMQConsumer, error) {
	fmt.Println("Initializing RocketMQ Consumer...")

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
	namesrvResolver, err := newRocketMQNameServerResolver(cfg.NameServer, net.DefaultResolver)
	if err != nil {
		return nil, err
	}

	c, err := rocketmq.NewPushConsumer(
		consumer.WithNsResolver(namesrvResolver),
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
