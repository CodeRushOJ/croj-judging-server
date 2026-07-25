package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/database"
)

const defaultLegacyConsumerRetryDelay = 2 * time.Second

type legacyConsumer interface {
	Start() error
	Shutdown() error
}

// legacyRuntime owns the Backend database and supervises the RocketMQ adapter.
// The upstream RocketMQ client performs a one-shot topic-route lookup during
// Start and cannot restart the same consumer after that lookup fails, so every
// retry must use a fresh consumer instance.
type legacyRuntime struct {
	database        *database.Database
	initialConsumer legacyConsumer
	newConsumer     func() (legacyConsumer, error)
	retryDelay      time.Duration
}

func newSupervisedLegacyRuntime(
	database *database.Database,
	newConsumer func() (legacyConsumer, error),
) (*legacyRuntime, error) {
	if database == nil {
		return nil, fmt.Errorf("legacy database is required")
	}
	if newConsumer == nil {
		return nil, fmt.Errorf("legacy consumer factory is required")
	}
	return &legacyRuntime{database: database, newConsumer: newConsumer}, nil
}

func (runtime *legacyRuntime) Run(ctx context.Context) error {
	if runtime == nil || runtime.newConsumer == nil {
		return fmt.Errorf("legacy consumer factory is required")
	}
	retryDelay := runtime.retryDelay
	if retryDelay <= 0 {
		retryDelay = defaultLegacyConsumerRetryDelay
	}
	next := runtime.initialConsumer
	runtime.initialConsumer = nil
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if next == nil {
			var err error
			next, err = runtime.newConsumer()
			if err != nil {
				log.Printf("Legacy RocketMQ consumer initialization is not ready; retrying: %v", err)
				if err := waitForLegacyConsumerRetry(ctx, retryDelay); err != nil {
					return err
				}
				continue
			}
		}
		if err := next.Start(); err != nil {
			log.Printf("Legacy RocketMQ topic route is not ready; retrying with a fresh consumer: %v", err)
			if shutdownErr := next.Shutdown(); shutdownErr != nil {
				log.Printf("Failed to discard the unready legacy RocketMQ consumer: %v", shutdownErr)
			}
			next = nil
			if err := waitForLegacyConsumerRetry(ctx, retryDelay); err != nil {
				return err
			}
			continue
		}
		<-ctx.Done()
		shutdownErr := next.Shutdown()
		if cause := context.Cause(ctx); cause != nil {
			return errors.Join(cause, shutdownErr)
		}
		return shutdownErr
	}
}

func (runtime *legacyRuntime) Close() error {
	if runtime == nil || runtime.database == nil {
		return nil
	}
	return runtime.database.Close()
}

func waitForLegacyConsumerRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
