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
