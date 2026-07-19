package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type joiningRuntime struct {
	started chan struct{}
	stopped chan struct{}
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
