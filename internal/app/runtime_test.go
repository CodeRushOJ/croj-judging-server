package app

import (
	"context"
	"encoding/json"
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

func TestLivenessDoesNotDependOnReadinessProbes(t *testing.T) {
	failing := errors.New("dependency unavailable")
	probes := healthyProductionProbes()
	probes["mysql"] = probeFunc(func(context.Context) error { return failing })
	runtime, err := NewRuntime(Config{Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second}, http.NotFoundHandler(), nil, probes)
	if err != nil {
		t.Fatal(err)
	}
	for method, want := range map[string]int{http.MethodGet: http.StatusNoContent, http.MethodPost: http.StatusMethodNotAllowed} {
		request := httptest.NewRequest(method, "/livez", nil)
		response := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s /livez status=%d want=%d", method, response.Code, want)
		}
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

func TestRuntimeHandlerReturnsRetryableProblemWhileShuttingDown(t *testing.T) {
	runtime, err := NewRuntime(Config{
		Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second,
	}, http.NotFoundHandler(), nil, healthyProductionProbes())
	if err != nil {
		t.Fatal(err)
	}
	runtime.shuttingDown.Store(true)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader("{}"))
	response := httptest.NewRecorder()

	runtime.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get("Content-Type") != "application/problem+json" ||
		response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Retry-After") == "" ||
		response.Header().Get("X-Request-Id") == "" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var problem struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Detail    string `json:"detail"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "https://coderushoj.dev/problems/shutting-down" ||
		problem.Title == "" || problem.Detail == "" || problem.Status != http.StatusServiceUnavailable ||
		problem.RequestID != response.Header().Get("X-Request-Id") {
		t.Fatalf("problem=%+v headers=%v", problem, response.Header())
	}
}

func healthyProductionProbes() map[string]Probe {
	return map[string]Probe{
		"mysql": probeFunc(func(context.Context) error { return nil }), "redis": probeFunc(func(context.Context) error { return nil }),
		"minio": probeFunc(func(context.Context) error { return nil }), "sandbox": probeFunc(func(context.Context) error { return nil }),
	}
}

func TestRuntimeAppliesAndValidatesHTTPTimeouts(t *testing.T) {
	runtime, err := NewRuntime(Config{
		Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second,
		MaximumBundleBytes: 512 << 20, MinimumBundleUploadBytesPerSecond: 1 << 20,
	}, http.NotFoundHandler(), nil, healthyProductionProbes())
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.httpServer()
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 15*time.Minute || server.WriteTimeout != 20*time.Minute || server.IdleTimeout != 60*time.Second {
		t.Fatalf("HTTP timeouts = header:%s read:%s write:%s idle:%s", server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout)
	}
	maximumBundleTransfer := time.Duration((runtime.config.MaximumBundleBytes+runtime.config.MinimumBundleUploadBytesPerSecond-1)/runtime.config.MinimumBundleUploadBytesPerSecond) * time.Second
	if server.ReadTimeout < maximumBundleTransfer {
		t.Fatalf("read timeout %s cannot receive the maximum bundle at the supported minimum rate in %s", server.ReadTimeout, maximumBundleTransfer)
	}
	if server.WriteTimeout-server.ReadTimeout < 5*time.Minute {
		t.Fatalf("post-read response allowance = %s, want at least 5m", server.WriteTimeout-server.ReadTimeout)
	}

	_, err = NewRuntime(Config{
		Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second,
		ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: -1,
	}, http.NotFoundHandler(), nil, healthyProductionProbes())
	if err == nil {
		t.Fatal("negative idle timeout accepted")
	}
}

func TestRuntimeRejectsDeadlinesThatCannotFinishANearMaximumSlowUpload(t *testing.T) {
	base := Config{
		Enabled: true, ListenAddress: "127.0.0.1:0", ShutdownTimeout: time.Second,
		MaximumBundleBytes: 512 << 20, MinimumBundleUploadBytesPerSecond: 1 << 20,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
	}
	base.ReadTimeout = 8*time.Minute + 31*time.Second
	base.WriteTimeout = 20 * time.Minute
	if _, err := NewRuntime(base, http.NotFoundHandler(), nil, healthyProductionProbes()); err == nil || !strings.Contains(err.Error(), "minimum upload rate") {
		t.Fatalf("near-maximum slow upload read deadline error = %v", err)
	}
	base.ReadTimeout = 10*time.Minute + 31*time.Second
	if _, err := NewRuntime(base, http.NotFoundHandler(), nil, healthyProductionProbes()); err == nil || !strings.Contains(err.Error(), "minimum upload rate") {
		t.Fatalf("multipart/header allowance below two minutes was accepted: %v", err)
	}

	base.ReadTimeout = 15 * time.Minute
	base.WriteTimeout = 19*time.Minute + 59*time.Second
	if _, err := NewRuntime(base, http.NotFoundHandler(), nil, healthyProductionProbes()); err == nil || !strings.Contains(err.Error(), "publication response allowance") {
		t.Fatalf("post-read publication deadline error = %v", err)
	}
}
