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
sandbox-discovery:
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
	if config.JudgeResult.BackendURL != "http://backend.internal:7999/api" || config.JudgeResult.ServiceToken != "runtime-judge-result-token-32-bytes" {
		t.Fatal("judge result callback override not applied")
	}
	if config.RocketMQ.Consumer.MaxReconsumeTimes != 12 {
		t.Fatalf("max reconsume times = %d", config.RocketMQ.Consumer.MaxReconsumeTimes)
	}
	if config.TestBundles.Endpoint != "minio.internal:9000" || config.TestBundles.Bucket != "immutable-bundles" || config.TestBundles.Region != "cn-test-1" || config.TestBundles.AccessKey != "judge-reader" || config.TestBundles.SecretKey != "runtime-only-minio-secret" || !config.TestBundles.UseTLS || config.TestBundles.CacheDir != "/tmp/runtime-bundles" || config.TestBundles.CacheMaxBytes != 1073741824 || config.TestBundles.MaxInfraAttempts != 4 {
		t.Fatalf("test bundle overrides not applied: %+v", config.TestBundles)
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
