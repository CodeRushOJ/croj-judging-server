package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigAppliesRuntimeEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	contents := []byte(`
database: {host: mysql, port: 3306, user: app, password: "", name: oj}
rocketmq:
  name-server: rocketmq:9876
  topic: submission-topic
  consumer: {group: judge, max-reconsume-times: 16}
judge-result:
  backend-url: http://backend:7999/api
  service-token: ""
  callback-timeout: 10s
  cache-capacity: 10000
  cache-ttl: 6h
test-bundles:
  endpoint: minio:9000
  bucket: judge-bundles
  region: us-east-1
  use-tls: false
  access-key: ""
  secret-key: ""
  cache-dir: /tmp/croj-bundles
  cache-max-bytes: 2147483648
  max-object-bytes: 536870912
  cache-ttl: 24h
  max-files: 20001
  max-manifest-bytes: 1048576
  max-case-bytes: 67108864
  max-uncompressed-bytes: 536870912
  max-compression-ratio: 200
  max-infra-attempts: 3
  max-time-limit-millis: 10000
  max-memory-limit-mib: 1024
sandbox-discovery:
  target: dns:///sandbox-workers.coderushoj.svc.cluster.local:50051
  namespace: coderushoj
  service: croj-sandbox
  port-name: grpc
  refresh-interval: 5s
  execute-timeout: 35s
  kubeconfig: ""
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_HOST", "mysql.internal")
	t.Setenv("DATABASE_PORT", "3307")
	t.Setenv("DATABASE_PASSWORD", "runtime-only")
	t.Setenv("SANDBOX_SERVICE", "sandbox-workers")
	t.Setenv("SANDBOX_EXECUTE_TIMEOUT", "40s")
	t.Setenv("SANDBOX_GRPC_TARGET", "dns:///sandbox-workers.alt.svc.cluster.local:50051")
	t.Setenv("BACKEND_INTERNAL_URL", "http://backend.internal:7999/api")
	t.Setenv("JUDGE_RESULT_SERVICE_TOKEN", "runtime-judge-result-token-32-bytes")
	t.Setenv("ROCKETMQ_MAX_RECONSUME_TIMES", "12")
	t.Setenv("OBJECT_STORAGE_ENDPOINT", "minio.internal:9000")
	t.Setenv("OBJECT_STORAGE_BUCKET", "immutable-bundles")
	t.Setenv("OBJECT_STORAGE_REGION", "cn-test-1")
	t.Setenv("OBJECT_STORAGE_ACCESS_KEY", "judge-reader")
	t.Setenv("OBJECT_STORAGE_SECRET_KEY", "runtime-only-minio-secret")
	t.Setenv("OBJECT_STORAGE_USE_TLS", "true")
	t.Setenv("JUDGE_BUNDLE_CACHE_DIR", "/tmp/runtime-bundles")
	t.Setenv("JUDGE_BUNDLE_MAX_INFRA_ATTEMPTS", "4")
	t.Setenv("JUDGE_BUNDLE_CACHE_MAX_BYTES", "1073741824")
	t.Setenv("JUDGE_MAX_TIME_LIMIT_MILLIS", "20000")
	t.Setenv("JUDGE_MAX_MEMORY_LIMIT_MIB", "2048")
	t.Setenv("JUDGE_DATABASE_DSN", "judge:secret@tcp(judge-mysql:3306)/coderushoj_judge")
	t.Setenv("EXTERNAL_SOURCE_KEYS_JSON", `{"1":"source-key"}`)
	t.Setenv("JUDGE_CALLBACK_KEY_VERSION", "2")
	t.Setenv("JUDGE_CALLBACK_KEYS_JSON", `{"2":"callback-key"}`)
	t.Setenv("EXTERNAL_WEBHOOK_WORKER_CONCURRENCY", "4")
	t.Setenv("EXTERNAL_API_READ_HEADER_TIMEOUT", "4s")
	t.Setenv("EXTERNAL_API_READ_TIMEOUT", "45s")
	t.Setenv("EXTERNAL_API_WRITE_TIMEOUT", "50s")
	t.Setenv("EXTERNAL_API_IDLE_TIMEOUT", "70s")
	t.Setenv("EXTERNAL_BUNDLE_MIN_UPLOAD_BYTES_PER_SECOND", "1048576")
	t.Setenv("EXTERNAL_BUNDLE_UPLOAD_CONCURRENCY", "7")
	t.Setenv("EXTERNAL_SOURCE_RETENTION", "1080h")
	t.Setenv("EXTERNAL_RETENTION_IDLE_DELAY", "2m")
	t.Setenv("EXTERNAL_RETENTION_DELETE_TIMEOUT", "20s")
	t.Setenv("LEGACY_JUDGE_ENABLED", "false")

	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if config.Database.Host != "mysql.internal" || config.Database.Port != 3307 {
		t.Fatalf("database override not applied: %+v", config.Database)
	}
	if config.Database.Password != "runtime-only" {
		t.Fatal("database password override not applied")
	}
	if config.SandboxDiscovery.Service != "sandbox-workers" {
		t.Fatal("sandbox Service override not applied")
	}
	if config.SandboxDiscovery.ExecuteTimeout != "40s" {
		t.Fatal("sandbox execute timeout override not applied")
	}
	if config.SandboxDiscovery.Target != "dns:///sandbox-workers.alt.svc.cluster.local:50051" {
		t.Fatalf("sandbox target = %q", config.SandboxDiscovery.Target)
	}
	if config.JudgeResult.BackendURL != "http://backend.internal:7999/api" || config.JudgeResult.ServiceToken != "runtime-judge-result-token-32-bytes" {
		t.Fatal("judge result callback override not applied")
	}
	if config.RocketMQ.Consumer.MaxReconsumeTimes != 12 {
		t.Fatalf("max reconsume times = %d", config.RocketMQ.Consumer.MaxReconsumeTimes)
	}
	if config.TestBundles.Endpoint != "minio.internal:9000" || config.TestBundles.Bucket != "immutable-bundles" || config.TestBundles.Region != "cn-test-1" || config.TestBundles.AccessKey != "judge-reader" || config.TestBundles.SecretKey != "runtime-only-minio-secret" || !config.TestBundles.UseTLS || config.TestBundles.CacheDir != "/tmp/runtime-bundles" || config.TestBundles.CacheMaxBytes != 1073741824 || config.TestBundles.MaxInfraAttempts != 4 || config.TestBundles.MaxTimeLimitMillis != 20000 || config.TestBundles.MaxMemoryLimitMiB != 2048 {
		t.Fatalf("test bundle overrides not applied: %+v", config.TestBundles)
	}
	if config.ExternalAPI.JudgeDatabaseDSN != "judge:secret@tcp(judge-mysql:3306)/coderushoj_judge" ||
		config.ExternalAPI.SourceKeysJSON != `{"1":"source-key"}` || config.ExternalAPI.CallbackKeyVersion != "2" ||
		config.ExternalAPI.CallbackKeysJSON != `{"2":"callback-key"}` || config.ExternalAPI.WebhookWorkerConcurrency != 4 ||
		config.ExternalAPI.ReadHeaderTimeout != "4s" || config.ExternalAPI.ReadTimeout != "45s" ||
		config.ExternalAPI.WriteTimeout != "50s" || config.ExternalAPI.IdleTimeout != "70s" ||
		config.ExternalAPI.BundleMinUploadBytesPerSecond != 1048576 ||
		config.ExternalAPI.BundleUploadConcurrency != 7 || config.ExternalAPI.SourceRetention != "1080h" ||
		config.ExternalAPI.RetentionIdleDelay != "2m" || config.ExternalAPI.RetentionDeleteTimeout != "20s" {
		t.Fatalf("external secret/runtime overrides not applied: %+v", config.ExternalAPI)
	}
	if config.LegacyJudge.Enabled {
		t.Fatal("legacy adapter opt-out was not applied")
	}
}

func TestLoadConfigRejectsInvalidEnvironmentPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("database: {port: 3306}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_PORT", "not-a-port")

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid DATABASE_PORT to fail")
	}
}

func TestExternalAPIIsDisabledByDefaultAndRequiresExplicitEnvironmentOptIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("external-api:\n  listen-address: 127.0.0.1:8081\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExternalAPI.Enabled {
		t.Fatal("external API must be disabled by default")
	}
	t.Setenv("EXTERNAL_API_ENABLED", "true")
	t.Setenv("EXTERNAL_WORKER_CONCURRENCY", "3")
	t.Setenv("SANDBOX_ALLOW_LEGACY_ENDPOINT_SLICE", "true")
	loaded, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ExternalAPI.Enabled || loaded.ExternalAPI.WorkerConcurrency != 3 {
		t.Fatalf("external API opt-in not applied: %+v", loaded.ExternalAPI)
	}
	if !loaded.SandboxDiscovery.AllowLegacyEndpointSlice {
		t.Fatal("legacy EndpointSlice fallback opt-in not applied")
	}
}
