package main

import (
	"context"
	"database/sql"
	"errors"
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
	_ "github.com/go-sql-driver/mysql"
)

func main() {
	fmt.Println("Starting Judging Server...")

	// 加载配置
	cfg, err := config.LoadConfig("configs/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	fmt.Println("Config loaded.")
	if !cfg.LegacyJudge.Enabled && !cfg.ExternalAPI.Enabled {
		log.Fatal("at least one of legacy Judge or external REST must be enabled")
	}
	refreshInterval, err := time.ParseDuration(cfg.SandboxDiscovery.RefreshInterval)
	if err != nil {
		log.Fatalf("Invalid sandbox discovery refresh interval: %v", err)
	}
	var sandboxSelector service.SandboxSelector
	var legacyScheduler *scheduler.Scheduler
	var sandboxReadinessProbe func(context.Context) error
	if cfg.SandboxDiscovery.Target != "" {
		targetSelector, err := scheduler.NewTarget(cfg.SandboxDiscovery.Target)
		if err != nil {
			log.Fatalf("Invalid sandbox gRPC target: %v", err)
		}
		sandboxSelector = targetSelector
		sandboxReadinessProbe = sandboxDNSProbe(cfg.SandboxDiscovery.Target)
		fmt.Println("gRPC DNS round_robin sandbox target initialized.")
	} else {
		if !cfg.SandboxDiscovery.AllowLegacyEndpointSlice {
			log.Fatal("SANDBOX_GRPC_TARGET is required; set SANDBOX_ALLOW_LEGACY_ENDPOINT_SLICE=true only for the deprecated fallback")
		}
		log.Printf("DEPRECATED: direct EndpointSlice scheduling is enabled explicitly; configure SANDBOX_GRPC_TARGET for a headless Service")
		discoveryClient, err := discovery.NewKubernetesDiscovery(
			cfg.SandboxDiscovery.Namespace,
			cfg.SandboxDiscovery.Service,
			cfg.SandboxDiscovery.PortName,
			cfg.SandboxDiscovery.Kubeconfig,
		)
		if err != nil {
			log.Fatalf("Failed to initialize legacy Kubernetes sandbox discovery: %v", err)
		}
		legacyScheduler = scheduler.New(discoveryClient)
		sandboxSelector = legacyScheduler
		sandboxReadinessProbe = func(context.Context) error {
			_, err := legacyScheduler.SelectSandbox()
			return err
		}
	}

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
		sandboxSelector,
		sandboxClient,
		cfg.TestBundles.MaxInfraAttempts,
	)
	var judgeDatabase *sql.DB
	if cfg.ExternalAPI.Enabled {
		if cfg.ExternalAPI.JudgeDatabaseDSN == "" {
			log.Fatal("JUDGE_DATABASE_DSN is required when external REST is enabled")
		}
		judgeDatabase, err = sql.Open("mysql", cfg.ExternalAPI.JudgeDatabaseDSN)
		if err != nil {
			log.Fatalf("Failed to open external Judge database: %v", err)
		}
		judgeDatabase.SetMaxOpenConns(32)
		judgeDatabase.SetMaxIdleConns(16)
		judgeDatabase.SetConnMaxLifetime(5 * time.Minute)
	}
	external, err := newExternalRuntime(cfg, judgeDatabase, bundleProvider, bundlePipeline, archiveLimits, sandboxReadinessProbe)
	if err != nil {
		if judgeDatabase != nil {
			_ = judgeDatabase.Close()
		}
		log.Fatalf("Failed to initialize external runtime: %v", err)
	}
	defer func() {
		if err := external.Close(); err != nil {
			log.Printf("Failed to close external runtime: %v", err)
		}
	}()
	legacy, err := initializeLegacyRuntime(cfg.LegacyJudge.Enabled, func() (*legacyRuntime, error) {
		legacyDatabase, openErr := database.NewDatabase(cfg.Database)
		if openErr != nil {
			return nil, fmt.Errorf("connect to legacy backend database: %w", openErr)
		}
		keepDatabase := false
		defer func() {
			if !keepDatabase {
				_ = legacyDatabase.Close()
			}
		}()
		executionPipeline := service.NewHiddenTestExecutor(bundleProvider, bundlePipeline)
		callbackTimeout, err := time.ParseDuration(cfg.JudgeResult.CallbackTimeout)
		if err != nil || callbackTimeout <= 0 {
			return nil, fmt.Errorf("invalid judge result callback timeout %q", cfg.JudgeResult.CallbackTimeout)
		}
		resultClient, err := callback.NewClient(
			cfg.JudgeResult.BackendURL,
			cfg.JudgeResult.ServiceToken,
			callbackTimeout,
			http.DefaultClient,
		)
		if err != nil {
			return nil, fmt.Errorf("initialize judge result callback: %w", err)
		}
		cacheTTL, err := time.ParseDuration(cfg.JudgeResult.CacheTTL)
		if err != nil || cacheTTL <= 0 {
			return nil, fmt.Errorf("invalid judge task cache TTL %q", cfg.JudgeResult.CacheTTL)
		}
		registry := service.NewTaskRegistry(cfg.JudgeResult.CacheCapacity, cacheTTL)
		judgeService := service.NewJudgeService(legacyDatabase, executionPipeline, resultClient, registry)
		newConsumer := func() (legacyConsumer, error) {
			return consumer.NewRocketMQConsumer(cfg.RocketMQ, judgeService)
		}
		runtime, err := newSupervisedLegacyRuntime(legacyDatabase, newConsumer)
		if err != nil {
			return nil, err
		}
		keepDatabase = true
		return runtime, nil
	})
	if err != nil {
		log.Fatalf("Failed to initialize legacy judge adapter: %v", err)
	}
	defer func() {
		if err := legacy.Close(); err != nil {
			log.Printf("Failed to close legacy backend database: %v", err)
		}
	}()
	if cfg.LegacyJudge.Enabled {
		fmt.Println("Legacy backend database and RocketMQ judge adapter initialized.")
	}

	// 使用 context 来管理 consumer 的生命周期
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if legacyScheduler != nil {
		go legacyScheduler.Run(ctx, refreshInterval)
	}
	var externalDone <-chan error
	if cfg.ExternalAPI.Enabled {
		fmt.Printf("Starting external REST API on %s...\n", cfg.ExternalAPI.ListenAddress)
		externalDone = startExternalRuntime(ctx, external.runtime, cancel)
	}

	var legacyDone <-chan error
	if cfg.LegacyJudge.Enabled {
		done := make(chan error, 1)
		legacyDone = done
		go func() {
			fmt.Println("Starting supervised legacy RocketMQ consumer...")
			done <- legacy.Run(ctx)
		}()
	}

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
	if externalDone != nil {
		if err := <-externalDone; err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("External runtime error: %v", err)
		}
		fmt.Println("External durable workers and REST API stopped.")
	}

	if legacyDone != nil {
		if err := <-legacyDone; err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Legacy RocketMQ consumer shutdown error: %v", err)
		}
		fmt.Println("Legacy RocketMQ consumer stopped.")
	}

	fmt.Println("Server gracefully stopped.")
}

// initializeLegacyRuntime is the process boundary for every Backend DB,
// Backend callback, and RocketMQ dependency. External-only deployments never
// invoke the initializer, so absent legacy configuration remains inert.
func initializeLegacyRuntime(enabled bool, initializer func() (*legacyRuntime, error)) (*legacyRuntime, error) {
	if !enabled {
		return &legacyRuntime{}, nil
	}
	if initializer == nil {
		return nil, fmt.Errorf("legacy runtime initializer is required")
	}
	return initializer()
}
