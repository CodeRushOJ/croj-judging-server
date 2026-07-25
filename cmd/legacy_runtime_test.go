package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLegacyConsumer struct {
	started  chan struct{}
	startErr error
	stops    atomic.Int32
	once     sync.Once
}

func (consumer *fakeLegacyConsumer) Start() error {
	consumer.once.Do(func() {
		if consumer.started != nil {
			close(consumer.started)
		}
	})
	return consumer.startErr
}

func (consumer *fakeLegacyConsumer) Shutdown() error {
	consumer.stops.Add(1)
	return nil
}

func TestLegacyRuntimeRetriesAConsumerWhoseTopicRouteIsNotReady(t *testing.T) {
	first := &fakeLegacyConsumer{startErr: errors.New("topic route info not found")}
	second := &fakeLegacyConsumer{started: make(chan struct{})}
	var attempts atomic.Int32
	runtime := &legacyRuntime{
		initialConsumer: first,
		newConsumer: func() (legacyConsumer, error) {
			attempts.Add(1)
			return second, nil
		},
		retryDelay: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	select {
	case <-second.started:
	case <-time.After(time.Second):
		t.Fatal("legacy consumer did not retry after the transient route failure")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runtime error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy runtime did not join shutdown")
	}
	if attempts.Load() != 1 {
		t.Fatalf("consumer factory attempts = %d, want 1", attempts.Load())
	}
	if first.stops.Load() != 1 || second.stops.Load() != 1 {
		t.Fatalf("shutdown counts first=%d second=%d", first.stops.Load(), second.stops.Load())
	}
}

func TestLegacyRuntimeCancellationInterruptsRetryBackoff(t *testing.T) {
	first := &fakeLegacyConsumer{started: make(chan struct{}), startErr: errors.New("route unavailable")}
	runtime := &legacyRuntime{
		initialConsumer: first,
		newConsumer: func() (legacyConsumer, error) {
			t.Fatal("factory must not run after cancellation")
			return nil, nil
		},
		retryDelay: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-first.started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runtime error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff ignored cancellation")
	}
}
