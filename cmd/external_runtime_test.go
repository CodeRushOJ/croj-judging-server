package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/pkg/config"
)

type joiningRuntime struct {
	started chan struct{}
	stopped chan struct{}
}

func TestBuildWebhookWorkersRequiresKeyRingAndCreatesReplicaOwnedWorkers(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x61}, 32))
	config := config.ExternalAPIConfig{
		WorkerID: "external-a", WebhookWorkerConcurrency: 3,
		CallbackKeyVersion: "1", CallbackKeysJSON: `{"1":"` + key + `"}`,
	}
	workers, err := buildWebhookWorkers(config, &sql.DB{})
	if err != nil || len(workers) != 3 {
		t.Fatalf("workers=%d error=%v", len(workers), err)
	}
	config.CallbackKeysJSON = "{}"
	if _, err := buildWebhookWorkers(config, &sql.DB{}); err == nil {
		t.Fatal("missing active callback key was accepted")
	}
}

func (runtime *joiningRuntime) Run(ctx context.Context) error {
	close(runtime.started)
	<-ctx.Done()
	close(runtime.stopped)
	return ctx.Err()
}

func TestStartExternalRuntimeDoneJoinsRuntimeShutdown(t *testing.T) {
	runtime := &joiningRuntime{started: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := startExternalRuntime(ctx, runtime, cancel)
	<-runtime.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runtime error = %v", err)
		}
		select {
		case <-runtime.stopped:
		default:
			t.Fatal("done was signalled before runtime stopped")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime shutdown was not joinable")
	}
}

func TestExternalOnlyModeDoesNotInitializeLegacyDependencies(t *testing.T) {
	called := false
	runtime, err := initializeLegacyRuntime(false, func() (*legacyRuntime, error) {
		called = true
		return nil, fmt.Errorf("legacy Backend DB and RocketMQ must be unreachable")
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("external-only mode touched legacy Backend DB/callback/RocketMQ initialization")
	}
	if runtime == nil || runtime.database != nil || runtime.consumer != nil {
		t.Fatalf("external-only legacy runtime = %+v", runtime)
	}
}
