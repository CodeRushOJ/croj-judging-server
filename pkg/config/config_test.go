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
  consumer: {group: judge}
sandbox-discovery:
  namespace: coderushoj
  service: croj-sandbox
  port-name: http
  refresh-interval: 5s
  kubeconfig: ""
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_HOST", "mysql.internal")
	t.Setenv("DATABASE_PORT", "3307")
	t.Setenv("DATABASE_PASSWORD", "runtime-only")
	t.Setenv("SANDBOX_SERVICE", "sandbox-workers")

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
