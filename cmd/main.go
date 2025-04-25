package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

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

	// 初始化服务发现 (即使不直接调用沙盒，调度器可能仍需要它)
	discoveryClient, err := discovery.NewZookeeperClient(cfg.Zookeeper.Servers)
	if err != nil {
		log.Printf("Warning: Failed to connect to Zookeeper: %v. Scheduler might not work correctly.", err)
		// 根据实际情况决定是否退出
	}
	// ZK 连接可能在后台断开，需要更健壮的处理
	// defer discoveryClient.Close() // Close 的实现在 ZK 库中有时会阻塞，视情况处理
	if discoveryClient != nil {
		fmt.Println("Zookeeper client initialized (connection attempt initiated).")
	}

	// 初始化调度器 (如果 JudgeService 还需要它的话)
	// 在当前简化流程下，JudgeService 不直接用调度器选择目的地，但保留框架
	scheduler := scheduler.NewScheduler(discoveryClient)
	fmt.Println("Scheduler initialized.")

	// 初始化判题服务 (不需要 sandboxClient)
	judgeService := service.NewJudgeService(db, scheduler, nil) // sandboxClient is nil
	fmt.Println("Judge service initialized.")

	// 启动 RocketMQ 消费者
	rocketmqConsumer, err := consumer.NewRocketMQConsumer(cfg.RocketMQ, judgeService)
	if err != nil {
		log.Fatalf("Failed to create RocketMQ consumer: %v", err)
	}

	// 使用 context 来管理 consumer 的生命周期
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// 关闭 ZK 连接 (如果需要)
	if discoveryClient != nil {
		// discoveryClient.Close() // 视 ZK 库实现决定是否调用
		fmt.Println("Zookeeper client shutdown.")
	}

	fmt.Println("Server gracefully stopped.")
}
