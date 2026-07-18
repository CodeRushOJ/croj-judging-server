package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/consumer"
	"github.com/CodeRushOJ/croj-judging-server/internal/database"
	"github.com/CodeRushOJ/croj-judging-server/internal/discovery"
	"github.com/CodeRushOJ/croj-judging-server/internal/scheduler"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
	// "github.com/CodeRushOJ/croj-judging-server/internal/sandbox" // 沙盒客户端在此服务中不再需要
)

func main() {
	fmt.Println("Starting Judging Server...")

	// 加载配置
	cfg, err := config.LoadConfig("configs/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	fmt.Println("Config loaded.")

	// 初始化数据库连接
	db, err := database.NewDatabase(cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()
	fmt.Println("Database connected.")

	refreshInterval, err := time.ParseDuration(cfg.SandboxDiscovery.RefreshInterval)
	if err != nil {
		log.Fatalf("Invalid sandbox discovery refresh interval: %v", err)
	}
	discoveryClient, err := discovery.NewKubernetesDiscovery(
		cfg.SandboxDiscovery.Namespace,
		cfg.SandboxDiscovery.Service,
		cfg.SandboxDiscovery.PortName,
		cfg.SandboxDiscovery.Kubeconfig,
	)
	if err != nil {
		log.Fatalf("Failed to initialize Kubernetes sandbox discovery: %v", err)
	}
	sandboxScheduler := scheduler.New(discoveryClient)
	fmt.Println("Scheduler initialized.")

	judgeService := service.NewJudgeService(db, sandboxScheduler, nil)
	fmt.Println("Judge service initialized.")

	// 启动 RocketMQ 消费者
	rocketmqConsumer, err := consumer.NewRocketMQConsumer(cfg.RocketMQ, judgeService)
	if err != nil {
		log.Fatalf("Failed to create RocketMQ consumer: %v", err)
	}

	// 使用 context 来管理 consumer 的生命周期
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sandboxScheduler.Run(ctx, refreshInterval)

	go func() {
		fmt.Println("Starting RocketMQ consumer...")
		if err := rocketmqConsumer.Start(); err != nil {
			log.Printf("RocketMQ consumer error: %v", err)
			// 这里应该有更健壮的错误处理和重启逻辑
			cancel() // 如果消费者启动失败，则取消 context
		}
	}()

	// 等待退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
		fmt.Println("Received shutdown signal.")
		cancel() // 收到信号，取消 context
	case <-ctx.Done():
		fmt.Println("Consumer context cancelled.")
		// Consumer 出错导致退出
	}

	fmt.Println("Shutting down server...")

	// 关闭消费者
	if err := rocketmqConsumer.Shutdown(); err != nil {
		log.Printf("Failed to shutdown RocketMQ consumer: %v", err)
	}
	fmt.Println("RocketMQ consumer stopped.")

	fmt.Println("Server gracefully stopped.")
}
