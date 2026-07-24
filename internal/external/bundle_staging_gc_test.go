package external

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStagingBundleObjectKeyUsesDedicatedEnumerablePrefix(t *testing.T) {
	tenantID := strings.Repeat("a", 26)
	uploadID := strings.Repeat("b", 26)
	key := stagingBundleObjectKey(tenantID, uploadID, [sha256.Size]byte{})
	if !strings.HasPrefix(key, "external-staging/") || !validBundleStagingGCKey(key) {
		t.Fatalf("staging key is not safely enumerable: %q", key)
	}
	legacy := "external/" + tenantID + "/staging/" + uploadID + "/" + strings.Repeat("0", 64) + ".zip"
	if validBundleStagingGCKey(legacy) {
		t.Fatalf("legacy final-object prefix was accepted by staging GC: %q", legacy)
	}
}

type stagingGCStore struct {
	mu         sync.Mutex
	candidates []BundleStagingObject
	deleted    []string
	after      []string
}

func (store *stagingGCStore) ListStaging(_ context.Context, _ time.Time, after string, limit int) ([]BundleStagingObject, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.after = append(store.after, after)
	start := 0
	if after != "" {
		for index, candidate := range store.candidates {
			if candidate.Key == after {
				start = index + 1
				break
			}
		}
	}
	end := start + limit
	if end >= len(store.candidates) {
		end = len(store.candidates)
		return append([]BundleStagingObject(nil), store.candidates[start:end]...), "", nil
	}
	return append([]BundleStagingObject(nil), store.candidates[start:end]...), store.candidates[end-1].Key, nil
}

func (store *stagingGCStore) Discard(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.deleted = append(store.deleted, key)
	return nil
}

type stagingGCReferences struct {
	mu       sync.Mutex
	sequence map[string][]bool
	calls    map[string]int
}

func (references *stagingGCReferences) IsBundleStagingReferenced(_ context.Context, key string) (bool, error) {
	references.mu.Lock()
	defer references.mu.Unlock()
	if references.calls == nil {
		references.calls = make(map[string]int)
	}
	index := references.calls[key]
	references.calls[key]++
	values := references.sequence[key]
	if len(values) == 0 {
		return false, nil
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index], nil
}

type deadlineRecordingStagingGCBackend struct {
	mu           sync.Mutex
	key          string
	lastModified time.Time
	deadlines    []time.Time
}

func (backend *deadlineRecordingStagingGCBackend) recordDeadline(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("staging GC operation has no deadline")
	}
	backend.mu.Lock()
	backend.deadlines = append(backend.deadlines, deadline)
	backend.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	return nil
}

func (backend *deadlineRecordingStagingGCBackend) ListStaging(
	ctx context.Context,
	_ time.Time,
	_ string,
	_ int,
) ([]BundleStagingObject, string, error) {
	if err := backend.recordDeadline(ctx); err != nil {
		return nil, "", err
	}
	return []BundleStagingObject{{Key: backend.key, LastModified: backend.lastModified}}, "", nil
}

func (backend *deadlineRecordingStagingGCBackend) IsBundleStagingReferenced(ctx context.Context, _ string) (bool, error) {
	if err := backend.recordDeadline(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (backend *deadlineRecordingStagingGCBackend) Discard(ctx context.Context, _ string) error {
	return backend.recordDeadline(ctx)
}

func TestBundleStagingGarbageCollectorUsesFreshDeadlineForEveryIO(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	backend := &deadlineRecordingStagingGCBackend{
		key:          key,
		lastModified: time.Now().Add(-3 * time.Hour),
	}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: backend, References: backend, MinimumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := collector.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want=1", removed)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.deadlines) != 5 {
		t.Fatalf("operation deadlines=%d want=5", len(backend.deadlines))
	}
	for index := 1; index < len(backend.deadlines); index++ {
		if !backend.deadlines[index].After(backend.deadlines[index-1]) {
			t.Fatalf("operation %d did not receive a fresh deadline: %v", index, backend.deadlines)
		}
	}
}

func TestBundleStagingGarbageCollectorRequiresWindowBeyondMaximumRequestAndLease(t *testing.T) {
	_, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: &stagingGCStore{}, References: &stagingGCReferences{}, MinimumAge: 20 * time.Minute,
	})
	if err == nil {
		t.Fatal("unsafe staging collection window was accepted")
	}
}

func TestBundleStagingGarbageCollectorNeverDeletesReferencedObject(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	references := &stagingGCReferences{sequence: map[string][]bool{key: {true}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: references, MinimumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := collector.SweepOnce(context.Background())
	if err != nil || removed != 0 || len(store.deleted) != 0 {
		t.Fatalf("removed=%d deleted=%v error=%v", removed, store.deleted, err)
	}
}

func TestBundleStagingGarbageCollectorRechecksReferencesBeforeAndAfterDelete(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	references := &stagingGCReferences{sequence: map[string][]bool{key: {false, false, false}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: references, MinimumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := collector.SweepOnce(context.Background())
	if err != nil || removed != 1 || len(store.deleted) != 1 || references.calls[key] != 3 {
		t.Fatalf("removed=%d deleted=%v checks=%d error=%v", removed, store.deleted, references.calls[key], err)
	}
}

func TestBundleStagingGarbageCollectorFailsClosedWhenReferenceAppears(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	references := &stagingGCReferences{sequence: map[string][]bool{key: {false, true}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: references, MinimumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := collector.SweepOnce(context.Background())
	if err != nil || removed != 0 || len(store.deleted) != 0 {
		t.Fatalf("removed=%d deleted=%v error=%v", removed, store.deleted, err)
	}
	if references.calls[key] != 2 {
		t.Fatalf("reference checks=%d want=2", references.calls[key])
	}
}

func TestBundleStagingGarbageCollectorReportsPostDeleteReference(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	references := &stagingGCReferences{sequence: map[string][]bool{key: {false, false, true}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: references, MinimumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.SweepOnce(context.Background())
	if !errors.Is(err, ErrBundleStagingReferenceRace) {
		t.Fatalf("post-delete reference error=%v", err)
	}
}

func TestBundleStagingGarbageCollectorContinuesPastReferencedPage(t *testing.T) {
	now := time.Now().UTC()
	store := &stagingGCStore{}
	references := &stagingGCReferences{sequence: make(map[string][]bool)}
	for index := range 3 {
		key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/" + strings.Repeat(string(rune('b'+index)), 26) + "/" + strings.Repeat(string(rune('a'+index)), 64) + ".zip"
		store.candidates = append(store.candidates, BundleStagingObject{Key: key, LastModified: now.Add(-3 * time.Hour)})
		if index < 2 {
			references.sequence[key] = []bool{true}
		}
	}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: references, MinimumAge: 2 * time.Hour, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := collector.SweepOnce(context.Background()); err != nil || removed != 0 {
		t.Fatalf("first sweep removed=%d error=%v", removed, err)
	}
	if removed, err := collector.SweepOnce(context.Background()); err != nil || removed != 1 {
		t.Fatalf("continued sweep removed=%d error=%v", removed, err)
	}
	if len(store.after) != 2 || store.after[0] != "" || store.after[1] != store.candidates[1].Key {
		t.Fatalf("staging continuation=%v", store.after)
	}
}

func TestBundleStagingGarbageCollectorPromptlyRevisitsContinuation(t *testing.T) {
	now := time.Now().UTC()
	store := &stagingGCStore{}
	for index := range 3 {
		key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/" + strings.Repeat(string(rune('b'+index)), 26) + "/" + strings.Repeat(string(rune('a'+index)), 64) + ".zip"
		store.candidates = append(store.candidates, BundleStagingObject{Key: key, LastModified: now.Add(-3 * time.Hour)})
	}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: &stagingGCReferences{sequence: make(map[string][]bool)},
		BatchSize: 2, RetryDelay: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after continuation delay")
	var waited time.Duration
	collector.wait = func(_ context.Context, delay time.Duration) error {
		waited = delay
		return stop
	}
	if err := collector.Run(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("Run error=%v want=%v", err, stop)
	}
	if waited != collector.retryDelay {
		t.Fatalf("continuation delay=%s want=%s", waited, collector.retryDelay)
	}
}

func TestBundleStagingGarbageCollectorWaitsFullIntervalAtEndOfScan(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: &stagingGCReferences{sequence: make(map[string][]bool)}, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after tail delay")
	var waited time.Duration
	collector.wait = func(_ context.Context, delay time.Duration) error {
		waited = delay
		return stop
	}
	if err := collector.Run(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("Run error=%v want=%v", err, stop)
	}
	if waited != collector.interval {
		t.Fatalf("tail delay=%s want=%s", waited, collector.interval)
	}
}

type failingStagingGCStore struct {
	err   error
	calls int
}

func (store *failingStagingGCStore) ListStaging(context.Context, time.Time, string, int) ([]BundleStagingObject, string, error) {
	store.calls++
	return nil, "", store.err
}

func (*failingStagingGCStore) Discard(context.Context, string) error { return nil }

func TestBundleStagingGarbageCollectorRetriesTransientFailureThenSurfacesIt(t *testing.T) {
	want := errors.New("minio unavailable")
	store := &failingStagingGCStore{err: want}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{
		Store: store, References: &stagingGCReferences{}, FailureLimit: 2, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	collector.wait = func(context.Context, time.Duration) error { return nil }
	if err := collector.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error=%v want=%v", err, want)
	}
	if store.calls != 2 {
		t.Fatalf("collection attempts=%d want=2", store.calls)
	}
}

func TestBundleStagingGarbageCollectorImmediatelySurfacesReferenceRace(t *testing.T) {
	key := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zip"
	store := &stagingGCStore{candidates: []BundleStagingObject{{Key: key, LastModified: time.Now().Add(-3 * time.Hour)}}}
	references := &stagingGCReferences{sequence: map[string][]bool{key: {false, false, true}}}
	collector, err := NewBundleStagingGarbageCollector(BundleStagingGarbageCollectorConfig{Store: store, References: references})
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Run(context.Background()); !errors.Is(err, ErrBundleStagingReferenceRace) {
		t.Fatalf("Run error=%v", err)
	}
}
