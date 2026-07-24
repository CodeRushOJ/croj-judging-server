package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var requiredProbes = [...]string{"mysql", "redis", "minio", "sandbox"}

type Probe interface {
	Check(context.Context) error
}

type Worker interface {
	Run(context.Context) error
}

type probeAdapter func(context.Context) error

func (probe probeAdapter) Check(ctx context.Context) error { return probe(ctx) }

func NewProbe(check func(context.Context) error) Probe {
	if check == nil {
		return nil
	}
	return probeAdapter(check)
}

type workerAdapter func(context.Context) error

func (worker workerAdapter) Run(ctx context.Context) error { return worker(ctx) }

func NewWorker(run func(context.Context) error) Worker {
	if run == nil {
		return nil
	}
	return workerAdapter(run)
}

type Config struct {
	Enabled                           bool
	ListenAddress                     string
	ReadinessTimeout                  time.Duration
	ShutdownTimeout                   time.Duration
	MaximumBundleBytes                int64
	MinimumBundleUploadBytesPerSecond int64
	ReadHeaderTimeout                 time.Duration
	ReadTimeout                       time.Duration
	WriteTimeout                      time.Duration
	IdleTimeout                       time.Duration
}

type Runtime struct {
	config Config
	api    http.Handler
	worker []Worker
	probes map[string]Probe

	started           chan struct{}
	runOnce           sync.Once
	afterHTTPShutdown func()
	shuttingDown      atomic.Bool
}

func NewRuntime(config Config, api http.Handler, workers []Worker, probes map[string]Probe) (*Runtime, error) {
	if api == nil {
		return nil, fmt.Errorf("external REST handler is required")
	}
	runtime := &Runtime{config: config, api: api, worker: append([]Worker(nil), workers...), probes: probes, started: make(chan struct{})}
	if !config.Enabled {
		return runtime, nil
	}
	if config.ListenAddress == "" || config.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("external REST listen address and shutdown timeout are required")
	}
	if config.ReadinessTimeout == 0 {
		runtime.config.ReadinessTimeout = 2 * time.Second
	} else if config.ReadinessTimeout < 0 || config.ReadinessTimeout > 30*time.Second {
		return nil, fmt.Errorf("external REST readiness timeout is invalid")
	}
	if err := normalizeHTTPTimeouts(&runtime.config); err != nil {
		return nil, err
	}
	for _, worker := range runtime.worker {
		if worker == nil {
			return nil, fmt.Errorf("external durable worker is required")
		}
	}
	for _, name := range requiredProbes {
		if probes[name] == nil {
			return nil, fmt.Errorf("%s readiness probe is required", name)
		}
	}
	return runtime, nil
}

func (runtime *Runtime) Started() <-chan struct{} { return runtime.started }

func (runtime *Runtime) Handler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if runtime.shuttingDown.Load() {
			writeShutdownProblem(response)
			return
		}
		if request.URL.Path == "/livez" {
			if request.Method != http.MethodGet || !runtime.config.Enabled {
				response.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if request.URL.Path == "/readyz" {
			runtime.serveReadiness(response, request)
			return
		}
		runtime.api.ServeHTTP(response, request)
	})
}

func writeShutdownProblem(response http.ResponseWriter) {
	requestID := runtimeRequestID()
	response.Header().Set("Content-Type", "application/problem+json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Retry-After", "5")
	response.Header().Set("X-Request-Id", requestID)
	response.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(response).Encode(struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Detail    string `json:"detail"`
		RequestID string `json:"requestId"`
	}{
		Type:      "https://coderushoj.dev/problems/shutting-down",
		Title:     "Service shutting down",
		Status:    http.StatusServiceUnavailable,
		Detail:    "Retry the request later.",
		RequestID: requestID,
	})
}

func runtimeRequestID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(random[:])
}

func (runtime *Runtime) Run(ctx context.Context) error {
	if runtime == nil {
		return fmt.Errorf("external runtime is required")
	}
	if !runtime.config.Enabled {
		runtime.runOnce.Do(func() { close(runtime.started) })
		return nil
	}
	var alreadyRunning bool
	runtime.runOnce.Do(func() { alreadyRunning = true })
	if !alreadyRunning {
		return fmt.Errorf("external runtime may only run once")
	}
	listener, err := net.Listen("tcp", runtime.config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for external REST: %w", err)
	}
	server := runtime.httpServer()
	workerContext, stopWorkers := context.WithCancel(ctx)
	var workers sync.WaitGroup
	errorsChannel := make(chan error, len(runtime.worker)+1)
	for _, durableWorker := range runtime.worker {
		workers.Add(1)
		go func(worker Worker) {
			defer workers.Done()
			if err := worker.Run(workerContext); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errorsChannel <- err:
				default:
				}
			}
		}(durableWorker)
	}
	serverDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverDone <- err
	}()
	close(runtime.started)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errorsChannel:
	case runErr = <-serverDone:
	}
	runtime.shuttingDown.Store(true)
	stopWorkers()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), runtime.config.ShutdownTimeout)
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	var workerShutdownErr error
	select {
	case <-workersDone:
	case <-shutdownContext.Done():
		workerShutdownErr = fmt.Errorf("shutdown external workers: %w", shutdownContext.Err())
	}
	shutdownErr := server.Shutdown(shutdownContext)
	if shutdownErr != nil {
		_ = server.Close()
	}
	cancelShutdown()
	if runtime.afterHTTPShutdown != nil {
		runtime.afterHTTPShutdown()
	}
	if runErr != nil {
		return runErr
	}
	if workerShutdownErr != nil {
		return workerShutdownErr
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown external REST: %w", shutdownErr)
	}
	return nil
}

func normalizeHTTPTimeouts(config *Config) error {
	defaults := []struct {
		value    *time.Duration
		name     string
		fallback time.Duration
		maximum  time.Duration
	}{
		{&config.ReadHeaderTimeout, "read header", 5 * time.Second, time.Minute},
		{&config.ReadTimeout, "read", 15 * time.Minute, 30 * time.Minute},
		{&config.WriteTimeout, "write", 20 * time.Minute, 40 * time.Minute},
		{&config.IdleTimeout, "idle", 60 * time.Second, 10 * time.Minute},
	}
	for _, item := range defaults {
		if *item.value == 0 {
			*item.value = item.fallback
		}
		if *item.value < 0 || *item.value > item.maximum {
			return fmt.Errorf("external REST %s timeout is invalid", item.name)
		}
	}
	if config.MaximumBundleBytes < 0 || config.MinimumBundleUploadBytesPerSecond < 0 {
		return fmt.Errorf("external REST bundle upload size and minimum rate are invalid")
	}
	if config.MaximumBundleBytes == 0 {
		return nil
	}
	if config.MinimumBundleUploadBytesPerSecond == 0 {
		config.MinimumBundleUploadBytesPerSecond = 1 << 20
	}
	transferSeconds := config.MaximumBundleBytes / config.MinimumBundleUploadBytesPerSecond
	if config.MaximumBundleBytes%config.MinimumBundleUploadBytesPerSecond != 0 {
		transferSeconds++
	}
	const requestFramingAllowance = 2 * time.Minute
	if config.ReadTimeout < requestFramingAllowance || transferSeconds > int64((config.ReadTimeout-requestFramingAllowance)/time.Second) {
		return fmt.Errorf("external REST read timeout cannot receive the maximum bundle at the minimum upload rate")
	}
	const publicationResponseAllowance = 5 * time.Minute
	if config.WriteTimeout-config.ReadTimeout < publicationResponseAllowance {
		return fmt.Errorf("external REST write timeout must preserve the publication response allowance")
	}
	return nil
}

func (runtime *Runtime) httpServer() *http.Server {
	return &http.Server{
		Handler: runtime.Handler(), ReadHeaderTimeout: runtime.config.ReadHeaderTimeout,
		ReadTimeout: runtime.config.ReadTimeout, WriteTimeout: runtime.config.WriteTimeout,
		IdleTimeout: runtime.config.IdleTimeout,
	}
}

func (runtime *Runtime) serveReadiness(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || !runtime.config.Enabled {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	for _, name := range requiredProbes {
		probeContext, cancel := context.WithTimeout(request.Context(), runtime.config.ReadinessTimeout)
		err := runtime.probes[name].Check(probeContext)
		cancel()
		if err != nil {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	response.WriteHeader(http.StatusNoContent)
}
