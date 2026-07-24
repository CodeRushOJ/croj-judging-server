package external

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	minimumBundleStagingGarbageAge       = time.Hour
	defaultBundleStagingGarbageAge       = 2 * time.Hour
	defaultBundleStagingGCInterval       = time.Hour
	defaultBundleStagingGCBatch          = 100
	defaultBundleStagingRetryDelay       = 5 * time.Second
	defaultBundleStagingFailLimit        = 3
	defaultBundleStagingOperationTimeout = 30 * time.Second
)

var ErrBundleStagingReferenceRace = errors.New("bundle staging object became referenced during collection")

type BundleStagingReferenceRepository interface {
	IsBundleStagingReferenced(context.Context, string) (bool, error)
}

type BundleStagingGarbageCollectorConfig struct {
	Store            BundleStagingObjectStore
	References       BundleStagingReferenceRepository
	MinimumAge       time.Duration
	Interval         time.Duration
	BatchSize        int
	RetryDelay       time.Duration
	FailureLimit     int
	OperationTimeout time.Duration
	Now              func() time.Time
}

type BundleStagingGarbageCollector struct {
	store            BundleStagingObjectStore
	references       BundleStagingReferenceRepository
	minimumAge       time.Duration
	interval         time.Duration
	batchSize        int
	retryDelay       time.Duration
	failureLimit     int
	operationTimeout time.Duration
	now              func() time.Time
	wait             func(context.Context, time.Duration) error
	cursorMu         sync.Mutex
	cursor           string
}

func NewBundleStagingGarbageCollector(config BundleStagingGarbageCollectorConfig) (*BundleStagingGarbageCollector, error) {
	if config.MinimumAge == 0 {
		config.MinimumAge = defaultBundleStagingGarbageAge
	}
	if config.Interval == 0 {
		config.Interval = defaultBundleStagingGCInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultBundleStagingGCBatch
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = defaultBundleStagingRetryDelay
	}
	if config.FailureLimit == 0 {
		config.FailureLimit = defaultBundleStagingFailLimit
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = defaultBundleStagingOperationTimeout
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	// One hour is deliberately greater than the maximum 40-minute application
	// operation and the publication lease. A staging key is random and written
	// once, so no legitimate DB reference can first appear after this window.
	if config.Store == nil || config.References == nil || config.MinimumAge < minimumBundleStagingGarbageAge ||
		config.Interval <= 0 || config.BatchSize < 1 || config.BatchSize > 1000 || config.RetryDelay <= 0 ||
		config.RetryDelay > config.Interval || config.FailureLimit < 1 || config.FailureLimit > 100 ||
		config.OperationTimeout <= 0 {
		return nil, fmt.Errorf("bundle staging collector requires a safe age window, store, and reference repository")
	}
	return &BundleStagingGarbageCollector{
		store: config.Store, references: config.References, minimumAge: config.MinimumAge,
		interval: config.Interval, batchSize: config.BatchSize, retryDelay: config.RetryDelay,
		failureLimit: config.FailureLimit, operationTimeout: config.OperationTimeout, now: config.Now,
		wait: waitForBundleStagingCollection,
	}, nil
}

func (collector *BundleStagingGarbageCollector) SweepOnce(ctx context.Context) (int, error) {
	if collector == nil || collector.store == nil || collector.references == nil || collector.now == nil {
		return 0, fmt.Errorf("bundle staging collector is not configured")
	}
	cutoff := collector.now().UTC().Add(-collector.minimumAge)
	collector.cursorMu.Lock()
	after := collector.cursor
	collector.cursorMu.Unlock()
	candidates, next, err := collector.listStaging(ctx, cutoff, after)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, candidate := range candidates {
		if !validBundleStagingGCKey(candidate.Key) || candidate.LastModified.IsZero() || !candidate.LastModified.Before(cutoff) {
			continue
		}
		referenced, err := collector.isReferenced(ctx, candidate.Key)
		if err != nil {
			return removed, err
		}
		if referenced {
			continue
		}
		// A second fresh read narrows the race immediately before deletion. The
		// hard age window and never-reused random staging key make a later first
		// reference invalid; the post-delete read detects any contract breach.
		referenced, err = collector.isReferenced(ctx, candidate.Key)
		if err != nil {
			return removed, err
		}
		if referenced {
			continue
		}
		if err := collector.discard(ctx, candidate.Key); err != nil {
			return removed, err
		}
		referenced, err = collector.isReferenced(ctx, candidate.Key)
		if err != nil {
			return removed, err
		}
		if referenced {
			return removed, fmt.Errorf("%w: %s", ErrBundleStagingReferenceRace, candidate.Key)
		}
		removed++
	}
	collector.cursorMu.Lock()
	collector.cursor = next
	collector.cursorMu.Unlock()
	return removed, nil
}

func (collector *BundleStagingGarbageCollector) listStaging(
	ctx context.Context,
	cutoff time.Time,
	after string,
) ([]BundleStagingObject, string, error) {
	operationContext, cancel := context.WithTimeout(ctx, collector.operationTimeout)
	defer cancel()
	return collector.store.ListStaging(operationContext, cutoff, after, collector.batchSize)
}

func (collector *BundleStagingGarbageCollector) isReferenced(ctx context.Context, key string) (bool, error) {
	operationContext, cancel := context.WithTimeout(ctx, collector.operationTimeout)
	defer cancel()
	return collector.references.IsBundleStagingReferenced(operationContext, key)
}

func (collector *BundleStagingGarbageCollector) discard(ctx context.Context, key string) error {
	operationContext, cancel := context.WithTimeout(ctx, collector.operationTimeout)
	defer cancel()
	return collector.store.Discard(operationContext, key)
}

func (collector *BundleStagingGarbageCollector) Run(ctx context.Context) error {
	if collector == nil || collector.interval <= 0 {
		return fmt.Errorf("bundle staging collector is not configured")
	}
	consecutiveFailures := 0
	for {
		_, err := collector.SweepOnce(ctx)
		delay := collector.interval
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if errors.Is(err, ErrBundleStagingReferenceRace) {
				return err
			}
			consecutiveFailures++
			if consecutiveFailures >= collector.failureLimit {
				return fmt.Errorf("bundle staging collection failed after %d consecutive attempts: %w", consecutiveFailures, err)
			}
			delay = collector.retryDelay
		} else {
			consecutiveFailures = 0
			if collector.hasContinuation() {
				delay = collector.retryDelay
			}
		}
		if err := collector.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (collector *BundleStagingGarbageCollector) hasContinuation() bool {
	collector.cursorMu.Lock()
	defer collector.cursorMu.Unlock()
	return collector.cursor != ""
}

func waitForBundleStagingCollection(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
