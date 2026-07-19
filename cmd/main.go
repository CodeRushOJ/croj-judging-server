package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/consumer"
	"github.com/CodeRushOJ/croj-judging-server/internal/database"
	"github.com/CodeRushOJ/croj-judging-server/internal/discovery"
	"github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	"github.com/CodeRushOJ/croj-judging-server/internal/scheduler"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
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

	executeTimeout, err := time.ParseDuration(cfg.SandboxDiscovery.ExecuteTimeout)
	if err != nil || executeTimeout <= 0 {
		log.Fatalf("Invalid sandbox execute timeout: %q", cfg.SandboxDiscovery.ExecuteTimeout)
	}
	connectionIdleTTL, err := time.ParseDuration(cfg.SandboxDiscovery.ConnectionIdleTTL)
	if err != nil || connectionIdleTTL <= 0 {
		log.Fatalf("Invalid sandbox connection idle TTL: %q", cfg.SandboxDiscovery.ConnectionIdleTTL)
	}
	sandboxClient := sandbox.NewClientWithCache(
		executeTimeout,
		cfg.SandboxDiscovery.MaxConnections,
		connectionIdleTTL,
	)
	defer func() {
		if err := sandboxClient.Close(); err != nil {
			log.Printf("Failed to close sandbox client: %v", err)
		}
	}()
	bundleCacheTTL, err := time.ParseDuration(cfg.TestBundles.CacheTTL)
	if err != nil || bundleCacheTTL <= 0 {
		log.Fatalf("Invalid test bundle cache TTL: %q", cfg.TestBundles.CacheTTL)
	}
	objectStore, err := bundle.NewMinIOStore(bundle.MinIOConfig{
		Endpoint:  cfg.TestBundles.Endpoint,
		Bucket:    cfg.TestBundles.Bucket,
		Region:    cfg.TestBundles.Region,
		UseTLS:    cfg.TestBundles.UseTLS,
		AccessKey: cfg.TestBundles.AccessKey,
		SecretKey: cfg.TestBundles.SecretKey,
	})
	if err != nil {
		log.Fatalf("Failed to initialize test bundle object storage: %v", err)
	}
	bundleCache, err := bundle.NewCache(
		cfg.TestBundles.CacheDir,
		cfg.TestBundles.CacheMaxBytes,
		cfg.TestBundles.MaxObjectBytes,
		bundleCacheTTL,
		objectStore,
	)
	if err != nil {
		log.Fatalf("Failed to initialize test bundle cache: %v", err)
	}
	archiveLimits := bundle.ArchiveLimits{
		MaxFiles:            cfg.TestBundles.MaxFiles,
		MaxManifestBytes:    cfg.TestBundles.MaxManifestBytes,
		MaxCaseBytes:        cfg.TestBundles.MaxCaseBytes,
		MaxTotalBytes:       cfg.TestBundles.MaxUncompressedBytes,
		MaxCompressionRatio: cfg.TestBundles.MaxCompressionRatio,
	}
	if err := bundle.ValidateArchiveLimits(archiveLimits); err != nil {
		log.Fatalf("Invalid test bundle archive limits: %v", err)
	}
	bundleProvider := bundle.NewProvider(bundleCache, archiveLimits)
	bundlePipeline := service.NewBatchBundlePipeline(
		sandboxScheduler,
		sandboxClient,
		cfg.TestBundles.MaxInfraAttempts,
	)
	executionPipeline := service.NewHiddenTestExecutor(bundleProvider, bundlePipeline)
	callbackTimeout, err := time.ParseDuration(cfg.JudgeResult.CallbackTimeout)
	if err != nil || callbackTimeout <= 0 {
		log.Fatalf("Invalid judge result callback timeout: %q", cfg.JudgeResult.CallbackTimeout)
	}
	resultClient, err := callback.NewClient(
		cfg.JudgeResult.BackendURL,
		cfg.JudgeResult.ServiceToken,
		callbackTimeout,
		http.DefaultClient,
	)
	if err != nil {
		log.Fatalf("Failed to initialize judge result callback: %v", err)
	}
	cacheTTL, err := time.ParseDuration(cfg.JudgeResult.CacheTTL)
	if err != nil || cacheTTL <= 0 {
		log.Fatalf("Invalid judge task cache TTL: %q", cfg.JudgeResult.CacheTTL)
	}
	registry := service.NewTaskRegistry(cfg.JudgeResult.CacheCapacity, cacheTTL)
	judgeService := service.NewJudgeService(db, executionPipeline, resultClient, registry)
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
