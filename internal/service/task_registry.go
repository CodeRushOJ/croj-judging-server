package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
)

type taskEntry struct {
	processing bool
	done       chan struct{}
	result     *callback.Result
	complete   bool
	lastUsed   time.Time
}

// TaskRegistry coalesces duplicate events and retains the exact result payload
// across transient callback failures. It is deliberately process-local; the
// backend result receipt is the durable idempotency authority.
type TaskRegistry struct {
	mu       sync.Mutex
	entries  map[string]*taskEntry
	capacity int
	ttl      time.Duration
}

func NewTaskRegistry(capacity int, ttl time.Duration) *TaskRegistry {
	if capacity <= 0 {
		capacity = 10_000
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &TaskRegistry{entries: make(map[string]*taskEntry), capacity: capacity, ttl: ttl}
}

func (registry *TaskRegistry) Process(
	ctx context.Context,
	key string,
	execute func(context.Context) (callback.Result, error),
	publish func(context.Context, callback.Result) error,
) error {
	for {
		entry, cachedResult, wait, err := registry.claim(key)
		if err != nil {
			return err
		}
		if wait != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
				continue
			}
		}
		if entry == nil {
			return nil
		}

		result := cachedResult
		if result == nil {
			value, executeErr := execute(ctx)
			if executeErr != nil {
				registry.finishExecutionFailure(key, entry)
				return executeErr
			}
			result = &value
			registry.rememberResult(key, entry, value)
		}
		publishErr := publish(ctx, *result)
		registry.finishPublish(key, entry, publishErr == nil || callback.IsPermanent(publishErr))
		return publishErr
	}
}

// claim returns a claimed entry, its cached result, a channel to wait on, or a
// nil entry when the task has already completed.
func (registry *TaskRegistry) claim(key string) (*taskEntry, *callback.Result, <-chan struct{}, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := time.Now()
	registry.pruneCompleted(now)
	if entry := registry.entries[key]; entry != nil {
		entry.lastUsed = now
		if entry.complete {
			return nil, nil, nil, nil
		}
		if entry.processing {
			return nil, nil, entry.done, nil
		}
		entry.processing = true
		entry.done = make(chan struct{})
		return entry, entry.result, nil, nil
	}
	registry.evictOldestCompleted()
	if len(registry.entries) >= registry.capacity {
		return nil, nil, nil, fmt.Errorf("judge task registry is full; retry after pending callbacks drain")
	}
	entry := &taskEntry{processing: true, done: make(chan struct{}), lastUsed: now}
	registry.entries[key] = entry
	return entry, nil, nil, nil
}

func (registry *TaskRegistry) rememberResult(key string, entry *taskEntry, result callback.Result) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries[key] == entry {
		entry.result = &result
	}
}

func (registry *TaskRegistry) finishExecutionFailure(key string, entry *taskEntry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries[key] != entry {
		return
	}
	delete(registry.entries, key)
	close(entry.done)
}

func (registry *TaskRegistry) finishPublish(key string, entry *taskEntry, complete bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries[key] != entry {
		return
	}
	entry.processing = false
	entry.complete = complete
	entry.lastUsed = time.Now()
	close(entry.done)
}

func (registry *TaskRegistry) pruneCompleted(now time.Time) {
	for key, entry := range registry.entries {
		if entry.complete && !entry.processing && now.Sub(entry.lastUsed) >= registry.ttl {
			delete(registry.entries, key)
		}
	}
}

func (registry *TaskRegistry) evictOldestCompleted() {
	if len(registry.entries) < registry.capacity {
		return
	}
	var oldestKey string
	var oldestTime time.Time
	for key, entry := range registry.entries {
		if !entry.complete || entry.processing {
			continue
		}
		if oldestKey == "" || entry.lastUsed.Before(oldestTime) {
			oldestKey, oldestTime = key, entry.lastUsed
		}
	}
	if oldestKey != "" {
		delete(registry.entries, oldestKey)
	}
}

func (registry *TaskRegistry) Len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.entries)
}
