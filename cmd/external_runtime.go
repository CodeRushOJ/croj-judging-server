package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/app"
	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/httpapi"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/internal/worker"
	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type externalRuntime struct {
	runtime  *app.Runtime
	redis    *redis.Client
	database *sql.DB
}

func buildWebhookWorkers(externalConfig config.ExternalAPIConfig, database *sql.DB) ([]app.Worker, error) {
	if database == nil || externalConfig.WebhookWorkerConcurrency <= 0 || strings.TrimSpace(externalConfig.WorkerID) == "" {
		return nil, fmt.Errorf("webhook database, worker ID, and positive concurrency are required")
	}
	callbackCipher, err := external.DecodeCallbackKeyRing(
		externalConfig.CallbackKeyVersion,
		externalConfig.CallbackKeysJSON,
		rand.Reader,
	)
	if err != nil {
		return nil, err
	}
	outbox, err := external.NewMySQLWebhookOutboxRepository(external.MySQLWebhookOutboxRepositoryConfig{
		Database: database, Random: rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	workers := make([]app.Worker, 0, externalConfig.WebhookWorkerConcurrency)
	for index := 0; index < externalConfig.WebhookWorkerConcurrency; index++ {
		workerID := externalConfig.WorkerID + "-webhook-" + strconv.Itoa(index)
		webhookWorker, err := external.NewWebhookWorker(external.WebhookWorkerConfig{
			Repository: outbox, CallbackCipher: callbackCipher, WorkerID: workerID,
		})
		if err != nil {
			return nil, err
		}
		workers = append(workers, app.NewWorker(webhookWorker.Run))
	}
	return workers, nil
}

type runnableRuntime interface {
	Run(context.Context) error
}

func startExternalRuntime(ctx context.Context, runtime runnableRuntime, cancel context.CancelFunc) <-chan error {
	done := make(chan error, 1)
	go func() {
		err := runtime.Run(ctx)
		cancel()
		done <- err
	}()
	return done
}

func newExternalRuntime(
	cfg *config.Config,
	database *sql.DB,
	provider *bundle.Provider,
	core *service.BatchBundlePipeline,
	archiveLimits bundle.ArchiveLimits,
	sandboxReadinessProbe func(context.Context) error,
) (*externalRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuration is required")
	}
	if !cfg.ExternalAPI.Enabled {
		runtime, err := app.NewRuntime(app.Config{}, http.NotFoundHandler(), nil, nil)
		return &externalRuntime{runtime: runtime}, err
	}
	if database == nil || provider == nil || core == nil || sandboxReadinessProbe == nil {
		return nil, fmt.Errorf("external API requires MySQL, bundle provider, and canonical execution core")
	}
	schemaContext, cancelSchema := context.WithTimeout(context.Background(), 30*time.Second)
	err := external.ValidateMigrations(schemaContext, database)
	cancelSchema()
	if err != nil {
		return nil, fmt.Errorf("validate external judge schema: %w", err)
	}
	externalConfig := cfg.ExternalAPI
	lease, err := positiveDuration(externalConfig.LeaseDuration, "external worker lease")
	if err != nil {
		return nil, err
	}
	heartbeat, err := positiveDuration(externalConfig.HeartbeatInterval, "external worker heartbeat")
	if err != nil {
		return nil, err
	}
	controlPoll, err := positiveDuration(externalConfig.ControlPollInterval, "external worker control poll")
	if err != nil {
		return nil, err
	}
	idleBackoff, err := positiveDuration(externalConfig.IdleBackoff, "external worker idle backoff")
	if err != nil {
		return nil, err
	}
	retryDelay, err := positiveDuration(externalConfig.RetryDelay, "external worker retry delay")
	if err != nil {
		return nil, err
	}
	shutdownTimeout, err := positiveDuration(externalConfig.ShutdownTimeout, "external API shutdown")
	if err != nil {
		return nil, err
	}
	readinessTimeout, err := positiveDuration(externalConfig.ReadinessTimeout, "external API readiness")
	if err != nil {
		return nil, err
	}
	idempotencyTTL, err := positiveDuration(externalConfig.IdempotencyTTL, "external idempotency retention")
	if err != nil {
		return nil, err
	}
	quotaRefill, err := positiveDuration(externalConfig.QuotaRefillPeriod, "external quota refill")
	if err != nil {
		return nil, err
	}
	if externalConfig.WorkerConcurrency <= 0 || strings.TrimSpace(externalConfig.WorkerID) == "" ||
		strings.TrimSpace(externalConfig.RedisAddress) == "" || externalConfig.SourceKeyVersion <= 0 || externalConfig.SourceKeyVersion > 65535 {
		return nil, fmt.Errorf("external worker concurrency and source key version are invalid")
	}
	authPepper, err := decode32(externalConfig.AuthPepperBase64, "external authentication pepper")
	if err != nil {
		return nil, err
	}
	idempotencyPepper, err := decode32(externalConfig.IdempotencyPepperB64, "external idempotency pepper")
	if err != nil {
		return nil, err
	}
	cursorKey, err := decode32(externalConfig.CursorKeyBase64, "external cursor key")
	if err != nil {
		return nil, err
	}
	minioClient, err := minio.New(cfg.TestBundles.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.TestBundles.AccessKey, cfg.TestBundles.SecretKey, ""),
		Secure: cfg.TestBundles.UseTLS, Region: cfg.TestBundles.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize external MinIO client: %w", err)
	}
	bundleObjects, err := external.NewMinIOBundleObjectStore(minioClient, cfg.TestBundles.Bucket)
	if err != nil {
		return nil, err
	}
	sourceObjects, err := external.NewMinIOSourceObjectStore(minioClient, cfg.TestBundles.Bucket)
	if err != nil {
		return nil, err
	}
	sourceCipher, err := external.DecodeSourceKeyRing(strconv.Itoa(externalConfig.SourceKeyVersion), externalConfig.SourceKeysJSON, rand.Reader)
	if err != nil {
		return nil, err
	}
	bundleRepository, err := external.NewSQLBundleRepository(database)
	if err != nil {
		return nil, err
	}
	bundleService, err := external.NewBundleService(bundleRepository, bundleObjects, external.BundleServiceConfig{
		MaxUploadBytes: cfg.TestBundles.MaxObjectBytes, ArchiveLimits: archiveLimits,
		MaxTimeLimitMillis: cfg.TestBundles.MaxTimeLimitMillis, MaxMemoryLimitMiB: cfg.TestBundles.MaxMemoryLimitMiB,
		IdempotencyTTL: idempotencyTTL, IdempotencyPepper: idempotencyPepper, Random: rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	jobRepository, err := external.NewMySQLJobRepository(external.MySQLJobRepositoryConfig{
		Database: database, Random: rand.Reader, Now: time.Now, IdempotencyPepper: idempotencyPepper,
		CursorKey: cursorKey, SourceCipher: sourceCipher, SourceObjects: sourceObjects, IdempotencyTTL: idempotencyTTL,
	})
	if err != nil {
		return nil, err
	}
	jobService, err := httpapi.NewMySQLJobService(jobRepository)
	if err != nil {
		return nil, err
	}
	credentialStore, err := external.NewSQLCredentialStore(database)
	if err != nil {
		return nil, err
	}
	authenticator, err := httpapi.NewAuthenticator(credentialStore, authPepper)
	if err != nil {
		return nil, err
	}
	redisClient := redis.NewClient(&redis.Options{Addr: externalConfig.RedisAddress, Password: externalConfig.RedisPassword, DB: externalConfig.RedisDB})
	quota, err := external.NewRedisQuotaFromClient(redisClient, externalConfig.RedisQuotaPrefix)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	capabilities := httpapi.Capabilities{
		APIVersion: "v1", Languages: []httpapi.LanguageCapability{{ID: "cpp20", DisplayName: "C++ 20", Runtime: "gcc"}},
		JudgeModes: []string{"ACM"}, Checkers: []string{"EXACT"},
		Limits: httpapi.CapabilityLimits{
			MaxSourceBytes: external.MaximumSourceBytes, MaxBundleBytes: cfg.TestBundles.MaxObjectBytes,
			MaxCaseBytes: cfg.TestBundles.MaxCaseBytes, MaxCaseCount: 256,
			MaxTimeLimitMillis: cfg.TestBundles.MaxTimeLimitMillis, MaxMemoryLimitMiB: cfg.TestBundles.MaxMemoryLimitMiB,
		},
	}
	handler, err := httpapi.NewServer(authenticator, capabilities,
		httpapi.WithJobService(jobService),
		httpapi.WithJobWriteQuota(quota, external.QuotaLimit{Capacity: externalConfig.JobSubmitCapacity, RefillPeriod: quotaRefill}),
		httpapi.WithBundleApplication(bundleService),
		httpapi.WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: externalConfig.BundleByteCapacity, RefillPeriod: quotaRefill}),
	)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}

	runner, err := worker.NewRunner(jobRepository, provider, core, worker.Config{
		LeaseDuration: lease, HeartbeatInterval: heartbeat, ControlPollInterval: controlPoll, RetryDelay: retryDelay,
	})
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	webhookWorkers, err := buildWebhookWorkers(externalConfig, database)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	reservationWorker, err := external.NewSourceReservationWorker(external.SourceReservationWorkerConfig{Repository: jobRepository})
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	workers := make([]app.Worker, 0, externalConfig.WorkerConcurrency+len(webhookWorkers)+2)
	for index := 0; index < externalConfig.WorkerConcurrency; index++ {
		workerID := externalConfig.WorkerID + "-" + strconv.Itoa(index)
		workers = append(workers, app.NewWorker(func(ctx context.Context) error { return runner.Run(ctx, workerID, idleBackoff) }))
	}
	workers = append(workers, webhookWorkers...)
	workers = append(workers, app.NewWorker(reservationWorker.Run))
	reconciler, err := external.NewBundleReconciler(bundleService)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	workers = append(workers, app.NewWorker(func(ctx context.Context) error { return reconciler.Run(ctx, 5*time.Second) }))
	probes := map[string]app.Probe{
		"mysql": app.NewProbe(func(ctx context.Context) error {
			if err := database.PingContext(ctx); err != nil {
				return err
			}
			return external.ValidateMigrations(ctx, database)
		}),
		"redis": app.NewProbe(func(ctx context.Context) error { return redisClient.Ping(ctx).Err() }),
		"minio": app.NewProbe(func(ctx context.Context) error {
			exists, err := minioClient.BucketExists(ctx, cfg.TestBundles.Bucket)
			if err == nil && !exists {
				return fmt.Errorf("external object bucket is unavailable")
			}
			return err
		}),
		"sandbox": app.NewProbe(sandboxReadinessProbe),
	}
	runtime, err := app.NewRuntime(app.Config{
		Enabled: true, ListenAddress: externalConfig.ListenAddress,
		ReadinessTimeout: readinessTimeout, ShutdownTimeout: shutdownTimeout,
	}, handler, workers, probes)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	return &externalRuntime{runtime: runtime, redis: redisClient, database: database}, nil
}

func (runtime *externalRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	var result error
	if runtime.redis != nil {
		result = errors.Join(result, runtime.redis.Close())
	}
	if runtime.database != nil {
		result = errors.Join(result, runtime.database.Close())
	}
	return result
}

func positiveDuration(value, name string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s duration is invalid", name)
	}
	return duration, nil
}

func decode32(value, name string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s must be 32 bytes encoded as base64", name)
	}
	return decoded, nil
}

func sandboxDNSProbe(target string) func(context.Context) error {
	return func(ctx context.Context) error {
		parsed, err := url.Parse(target)
		if err != nil || parsed.Scheme != "dns" {
			return fmt.Errorf("sandbox DNS target is invalid")
		}
		host, _, err := net.SplitHostPort(strings.TrimPrefix(parsed.Path, "/"))
		if err != nil || host == "" {
			return fmt.Errorf("sandbox DNS target is invalid")
		}
		addresses, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return err
		}
		if len(addresses) == 0 {
			return fmt.Errorf("sandbox DNS target has no endpoints")
		}
		connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return err
		}
		defer connection.Close()
		connection.Connect()
		for {
			state := connection.GetState()
			if state == connectivity.Ready {
				return nil
			}
			if !connection.WaitForStateChange(ctx, state) {
				return ctx.Err()
			}
		}
	}
}
