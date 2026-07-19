package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type probeFunc func(context.Context) error

func (probe probeFunc) Check(ctx context.Context) error { return probe(ctx) }

type workerFunc func(context.Context) error

func (worker workerFunc) Run(ctx context.Context) error { return worker(ctx) }

func TestRuntimeDisabledDoesNotStartAnything(t *testing.T) {
	started := make(chan struct{}, 1)
	runtime, err := NewRuntime(Config{}, http.NotFoundHandler(), []Worker{workerFunc(func(context.Context) error {
		started <- struct{}{}
		return nil
	})}, nil)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := runtime.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-started:
		t.Fatal("disabled runtime started a worker")
	default:
	}
}

func TestReadinessRequiresEveryProductionDependency(t *testing.T) {
	failing := errors.New("not ready")
	probes := map[string]Probe{
		"mysql":   probeFunc(func(context.Context) error { return nil }),
		"redis":   probeFunc(func(context.Context) error { return nil }),
		"minio":   probeFunc(func(context.Context) error { return failing }),
		"sandbox": probeFunc(func(context.Context) error { return nil }),
	}
	runtime, err := NewRuntime(Config{Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second}, http.NotFoundHandler(), nil, probes)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	probes["minio"] = probeFunc(func(context.Context) error { return nil })
	response = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("readiness status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRuntimeStopsWorkersBeforeHTTP(t *testing.T) {
	var mu sync.Mutex
	events := make([]string, 0, 2)
	workerStopped := make(chan struct{})
	runtime, err := NewRuntime(Config{Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), []Worker{
		workerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			events = append(events, "worker")
			mu.Unlock()
			close(workerStopped)
			return ctx.Err()
		}),
	}, healthyProductionProbes())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	runtime.afterHTTPShutdown = func() {
		select {
		case <-workerStopped:
		default:
			t.Error("HTTP stopped before worker")
		}
		mu.Lock()
		events = append(events, "http")
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-runtime.Started():
	case <-time.After(time.Second):
		t.Fatal("runtime did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "worker" || events[1] != "http" {
		t.Fatalf("shutdown events = %v", events)
	}
}

func TestRuntimeBoundsAStuckWorkerShutdown(t *testing.T) {
	release := make(chan struct{})
	runtime, err := NewRuntime(Config{Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: 30 * time.Millisecond}, http.NotFoundHandler(), []Worker{
		workerFunc(func(context.Context) error { <-release; return nil }),
	}, healthyProductionProbes())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-runtime.Started()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "shutdown external workers") {
			t.Fatalf("shutdown error=%v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stuck worker exceeded shutdown bound")
	}
	close(release)
}

func healthyProductionProbes() map[string]Probe {
	return map[string]Probe{
		"mysql": probeFunc(func(context.Context) error { return nil }), "redis": probeFunc(func(context.Context) error { return nil }),
		"minio": probeFunc(func(context.Context) error { return nil }), "sandbox": probeFunc(func(context.Context) error { return nil }),
	}
}
