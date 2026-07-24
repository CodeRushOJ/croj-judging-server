package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
)

func TestTaskRegistryCoalescesConcurrentDuplicate(t *testing.T) {
	registry := NewTaskRegistry(16, time.Hour)
	var executions atomic.Int32
	var publishes atomic.Int32
	start := make(chan struct{})
	process := func() error {
		return registry.Process(context.Background(), "evt/99/1", func(context.Context) (callback.Result, error) {
			executions.Add(1)
			<-start
			return validRegistryResult(), nil
		}, func(context.Context, callback.Result) error {
			publishes.Add(1)
			return nil
		})
	}

	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- process() }()
	go func() { errorsChannel <- process() }()
	time.Sleep(10 * time.Millisecond)
	close(start)
	for range 2 {
		if err := <-errorsChannel; err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	if executions.Load() != 1 || publishes.Load() != 1 {
		t.Fatalf("executions=%d publishes=%d", executions.Load(), publishes.Load())
	}
}

func TestTaskRegistryReusesStableResultAfterTransientPublishFailure(t *testing.T) {
	registry := NewTaskRegistry(16, time.Hour)
	wantErr := errors.New("backend unavailable")
	var executions atomic.Int32
	var published []callback.Result
	var mu sync.Mutex
	execute := func(context.Context) (callback.Result, error) {
		executions.Add(1)
		return validRegistryResult(), nil
	}
	publish := func(_ context.Context, result callback.Result) error {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, result)
		if len(published) == 1 {
			return wantErr
		}
		return nil
	}

	if err := registry.Process(context.Background(), "evt/99/1", execute, publish); !errors.Is(err, wantErr) {
		t.Fatalf("first Process error = %v", err)
	}
	if err := registry.Process(context.Background(), "evt/99/1", execute, publish); err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want 1", executions.Load())
	}
	if len(published) != 2 || published[0] != published[1] {
		t.Fatalf("published results changed: %+v", published)
	}
}

func TestTaskRegistryBoundsCompletedEntries(t *testing.T) {
	registry := NewTaskRegistry(2, time.Hour)
	for _, key := range []string{"one", "two", "three"} {
		if err := registry.Process(context.Background(), key,
			func(context.Context) (callback.Result, error) { return validRegistryResult(), nil },
			func(context.Context, callback.Result) error { return nil },
		); err != nil {
			t.Fatal(err)
		}
	}
	if got := registry.Len(); got != 2 {
		t.Fatalf("registry length = %d, want 2", got)
	}
}

func validRegistryResult() callback.Result {
	return callback.Result{ResultID: "evt", SubmissionID: 99, AttemptNo: 1, Status: callback.StatusAccepted}
}
