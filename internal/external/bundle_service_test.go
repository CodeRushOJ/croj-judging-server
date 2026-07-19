package external

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

const testTenantID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"

type memoryBundleRepository struct {
	mu              sync.Mutex
	idempotency     map[string]bundleUploadRecord
	bundles         map[string]BundleMetadata
	status          map[string]BundlePublicationStatus
	staging         map[string]string
	claims          map[string]BundlePublicationClaim
	leaseUntil      map[string]time.Time
	attempts        map[string]int
	logicalCreates  int
	commitErr       error
	completeErrOnce error
}

type bundleUploadRecord struct {
	requestHash [sha256.Size]byte
	metadata    BundleMetadata
}

func newMemoryBundleRepository() *memoryBundleRepository {
	return &memoryBundleRepository{
		idempotency: map[string]bundleUploadRecord{}, bundles: map[string]BundleMetadata{}, status: map[string]BundlePublicationStatus{},
		staging: map[string]string{}, claims: map[string]BundlePublicationClaim{}, leaseUntil: map[string]time.Time{}, attempts: map[string]int{},
	}
}

func (repository *memoryBundleRepository) FindBundleUpload(_ context.Context, tenantID string, keyDigest [sha256.Size]byte) (BundleUploadLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, found := repository.idempotency[tenantID+":"+hex.EncodeToString(keyDigest[:])]
	bundleKey := tenantID + ":" + record.metadata.SHA256
	return BundleUploadLookup{Found: found, Status: repository.status[bundleKey], RequestHash: record.requestHash, StagingKey: repository.staging[bundleKey], Metadata: record.metadata}, nil
}

func (repository *memoryBundleRepository) CommitBundleUpload(_ context.Context, input BundleCommitInput) (BundleCommitResult, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.commitErr != nil {
		return BundleCommitResult{}, repository.commitErr
	}
	idempotencyKey := input.TenantID + ":" + hex.EncodeToString(input.IdempotencyDigest[:])
	if record, found := repository.idempotency[idempotencyKey]; found {
		if record.requestHash != input.RequestHash {
			return BundleCommitResult{}, ErrIdempotencyConflict
		}
		bundleKey := input.TenantID + ":" + record.metadata.SHA256
		if bundlePublicationNeedsFreshStaging(repository.status[bundleKey], repository.staging[bundleKey]) {
			repository.status[bundleKey] = BundlePublicationPending
			repository.staging[bundleKey] = input.StagingObjectKey
			repository.attempts[bundleKey] = 0
		}
		return BundleCommitResult{Metadata: record.metadata, Replay: true, Status: repository.status[bundleKey], StagingKey: repository.staging[bundleKey]}, nil
	}
	bundleKey := input.TenantID + ":" + hex.EncodeToString(input.RequestHash[:])
	metadata, found := repository.bundles[bundleKey]
	if !found {
		metadata = input.Metadata
		repository.bundles[bundleKey] = metadata
		repository.status[bundleKey] = BundlePublicationPending
		repository.staging[bundleKey] = input.StagingObjectKey
		repository.logicalCreates++
	} else if bundlePublicationNeedsFreshStaging(repository.status[bundleKey], repository.staging[bundleKey]) {
		repository.status[bundleKey] = BundlePublicationPending
		repository.staging[bundleKey] = input.StagingObjectKey
		repository.attempts[bundleKey] = 0
	}
	repository.idempotency[idempotencyKey] = bundleUploadRecord{requestHash: input.RequestHash, metadata: metadata}
	return BundleCommitResult{Metadata: metadata, Replay: found, Status: repository.status[bundleKey], StagingKey: repository.staging[bundleKey]}, nil
}

func (repository *memoryBundleRepository) ClaimBundlePublication(_ context.Context, tenantID, bundleID, leaseToken string, now, leaseUntil time.Time) (BundlePublicationClaim, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenantID+":") && metadata.BundleID == bundleID {
			if repository.status[key] == BundlePublicationReady || repository.status[key] == BundlePublicationAbandoned ||
				(repository.status[key] == BundlePublicationPublishing && repository.leaseUntil[key].After(now)) {
				return BundlePublicationClaim{}, false, nil
			}
			repository.status[key] = BundlePublicationPublishing
			repository.attempts[key]++
			digest, _ := hex.DecodeString(metadata.SHA256)
			claim := BundlePublicationClaim{TenantID: tenantID, BundleID: bundleID, ObjectKey: bundleObjectKey(tenantID, arrayDigest(digest)), StagingKey: repository.staging[key], SizeBytes: metadata.SizeBytes, LeaseToken: leaseToken, AttemptCount: repository.attempts[key]}
			copy(claim.RequestHash[:], digest)
			repository.claims[key] = claim
			repository.leaseUntil[key] = leaseUntil
			return claim, true, nil
		}
	}
	return BundlePublicationClaim{}, false, ErrBundleNotFound
}

func (repository *memoryBundleRepository) ClaimNextBundlePublication(ctx context.Context, leaseToken string, now, leaseUntil time.Time) (BundlePublicationClaim, bool, error) {
	repository.mu.Lock()
	var tenantID, bundleID string
	for key, metadata := range repository.bundles {
		if repository.status[key] == BundlePublicationPending || (repository.status[key] == BundlePublicationPublishing && !repository.leaseUntil[key].After(now)) {
			tenantID = strings.SplitN(key, ":", 2)[0]
			bundleID = metadata.BundleID
			break
		}
	}
	repository.mu.Unlock()
	if bundleID == "" {
		return BundlePublicationClaim{}, false, nil
	}
	return repository.ClaimBundlePublication(ctx, tenantID, bundleID, leaseToken, now, leaseUntil)
}

func (repository *memoryBundleRepository) CompleteBundlePublication(_ context.Context, claim BundlePublicationClaim, _ time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := claim.TenantID + ":" + hex.EncodeToString(claim.RequestHash[:])
	stored, found := repository.claims[key]
	if !found || stored.LeaseToken != claim.LeaseToken {
		return ErrBundlePublishing
	}
	if repository.completeErrOnce != nil {
		err := repository.completeErrOnce
		repository.completeErrOnce = nil
		return err
	}
	repository.status[key] = BundlePublicationReady
	delete(repository.claims, key)
	delete(repository.leaseUntil, key)
	return nil
}

func (repository *memoryBundleRepository) FailBundlePublication(_ context.Context, claim BundlePublicationClaim, _ string, _ time.Time, maxAttempts int) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := claim.TenantID + ":" + hex.EncodeToString(claim.RequestHash[:])
	stored, found := repository.claims[key]
	if !found || stored.LeaseToken != claim.LeaseToken {
		return false, ErrBundlePublishing
	}
	abandoned := claim.AttemptCount >= maxAttempts
	if abandoned {
		repository.status[key] = BundlePublicationAbandoned
	} else {
		repository.status[key] = BundlePublicationPending
	}
	delete(repository.claims, key)
	delete(repository.leaseUntil, key)
	return abandoned, nil
}

func (repository *memoryBundleRepository) SweepUnrecoverableBundlePublications(_ context.Context, _ time.Time, limit int) (int64, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	var swept int64
	for key := range repository.bundles {
		if swept >= int64(limit) {
			break
		}
		if repository.staging[key] == "" && repository.status[key] == BundlePublicationPending {
			repository.status[key] = BundlePublicationAbandoned
			swept++
		}
	}
	return swept, nil
}

func (repository *memoryBundleRepository) FindBundle(_ context.Context, tenantID, bundleID string) (BundleMetadata, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenantID+":") && metadata.BundleID == bundleID && repository.status[key] == BundlePublicationReady {
			return metadata, nil
		}
	}
	return BundleMetadata{}, ErrBundleNotFound
}

type atomicMemoryObjectStore struct {
	mu           sync.Mutex
	staged       map[string][]byte
	objects      map[string][]byte
	publishes    int
	promoteCalls int
	failNext     int
}

type blockingMemoryObjectStore struct {
	inner   *atomicMemoryObjectStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (store *blockingMemoryObjectStore) Stage(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	return store.inner.Stage(ctx, key, filename, size, digest)
}

func (store *blockingMemoryObjectStore) Promote(ctx context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.once.Do(func() { close(store.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.release:
		return store.inner.Promote(ctx, stagingKey, finalKey, size, digest)
	}
}

func (store *blockingMemoryObjectStore) Discard(ctx context.Context, key string) error {
	return store.inner.Discard(ctx, key)
}

func (store *atomicMemoryObjectStore) Stage(_ context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if int64(len(data)) != size || actual != digest {
		return errors.New("stage metadata mismatch")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.staged == nil {
		store.staged = map[string][]byte{}
	}
	store.staged[key] = append([]byte(nil), data...)
	return nil
}

func (store *atomicMemoryObjectStore) Promote(_ context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.promoteCalls++
	if store.failNext > 0 {
		store.failNext--
		return errors.New("injected object publish failure")
	}
	data, found := store.staged[stagingKey]
	if !found || int64(len(data)) != size || sha256.Sum256(data) != digest {
		return errors.New("staged object missing or invalid")
	}
	if store.objects == nil {
		store.objects = map[string][]byte{}
	}
	if _, exists := store.objects[finalKey]; !exists {
		store.publishes++
		store.objects[finalKey] = append([]byte(nil), data...)
	}
	return nil
}

func (store *atomicMemoryObjectStore) Discard(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.staged, key)
	return nil
}

func arrayDigest(value []byte) (digest [sha256.Size]byte) { copy(digest[:], value); return digest }

func TestBundleServiceNeverPublishesUnownedFinalObject(t *testing.T) {
	repository := newMemoryBundleRepository()
	repository.commitErr = errors.New("injected commit failure")
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())

	_, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(validExternalBundle(t, "input")))
	if err == nil || len(store.objects) != 0 || store.publishes != 0 {
		t.Fatalf("error=%v objects=%d publishes=%d", err, len(store.objects), store.publishes)
	}
}

func TestBundleServiceReconcilesOwnedObjectAfterPublishFailure(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{failNext: 1}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")

	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body)); err == nil {
		t.Fatal("expected injected publish failure")
	}
	if repository.logicalCreates != 1 || len(store.objects) != 0 {
		t.Fatalf("owned rows=%d visible objects=%d", repository.logicalCreates, len(store.objects))
	}
	metadata, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || !replay || metadata.BundleID == "" || len(store.objects) != 1 {
		t.Fatalf("metadata=%+v replay=%v error=%v objects=%d", metadata, replay, err, len(store.objects))
	}
}

func TestBundleReconcilerCompletesPublicationWithoutClientReplay(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{failNext: 1}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body)); err == nil {
		t.Fatal("expected initial promotion failure")
	}
	reconciler, err := NewBundleReconciler(service)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.now = func() time.Time { return service.now().Add(service.config.PublicationRetry) }
	processed, err := reconciler.ReconcileOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	for _, metadata := range repository.bundles {
		if _, err := service.Get(context.Background(), testTenantID, metadata.BundleID); err != nil {
			t.Fatalf("reconciled metadata unavailable: %v", err)
		}
	}
}

func TestBundleReconcilerRepairsAmbiguousReadyCommitAfterLeaseExpiry(t *testing.T) {
	repository := newMemoryBundleRepository()
	repository.completeErrOnce = errors.New("ambiguous database completion")
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body)); err == nil {
		t.Fatal("expected ambiguous completion error")
	}
	reconciler, _ := NewBundleReconciler(service)
	reconciler.now = func() time.Time { return service.now().Add(service.config.PublicationLease + time.Second) }
	processed, err := reconciler.ReconcileOnce(context.Background())
	if err != nil || !processed || store.promoteCalls != 2 || len(store.objects) != 1 {
		t.Fatalf("processed=%v error=%v promotes=%d objects=%d", processed, err, store.promoteCalls, len(store.objects))
	}
}

func TestBundleReconcilerAbandonsAfterBoundedFailures(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{failNext: 2}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	service.config.MaxPublishAttempts = 2
	body := validExternalBundle(t, "input")
	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body)); err == nil {
		t.Fatal("expected first promotion failure")
	}
	reconciler, _ := NewBundleReconciler(service)
	reconciler.now = func() time.Time { return service.now().Add(service.config.PublicationRetry) }
	processed, err := reconciler.ReconcileOnce(context.Background())
	if err == nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	for key := range repository.bundles {
		if repository.status[key] != BundlePublicationAbandoned {
			t.Fatalf("publication status=%s", repository.status[key])
		}
	}
}

func TestBundleServiceRevivesAbandonedPublicationFromSameIdempotentUpload(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{failNext: 1}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	service.config.MaxPublishAttempts = 1
	body := validExternalBundle(t, "input")

	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body)); err == nil {
		t.Fatal("expected initial promotion failure")
	}
	metadata, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || !replay || metadata.BundleID == "" {
		t.Fatalf("metadata=%+v replay=%v error=%v", metadata, replay, err)
	}
	digest := sha256.Sum256(body)
	key := testTenantID + ":" + hex.EncodeToString(digest[:])
	if repository.status[key] != BundlePublicationReady || repository.attempts[key] != 1 || len(store.objects) != 1 {
		t.Fatalf("status=%s attempts=%d objects=%d", repository.status[key], repository.attempts[key], len(store.objects))
	}
}

func TestBundleServiceAttachesFreshStagingToLegacyPendingBundle(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	legacy := BundleMetadata{
		BundleID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", SHA256: digestHex, SizeBytes: int64(len(body)),
		CaseCount: 1, ManifestVersion: 1, CreatedAt: service.now(),
	}
	repository.bundles[testTenantID+":"+digestHex] = legacy
	repository.status[testTenantID+":"+digestHex] = BundlePublicationPending

	metadata, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-legacy01", bytes.NewReader(body))
	if err != nil || !replay || metadata.BundleID != legacy.BundleID {
		t.Fatalf("metadata=%+v replay=%v error=%v", metadata, replay, err)
	}
	if repository.status[testTenantID+":"+digestHex] != BundlePublicationReady || repository.staging[testTenantID+":"+digestHex] == "" || len(store.objects) != 1 {
		t.Fatalf("status=%s staging=%q objects=%d", repository.status[testTenantID+":"+digestHex], repository.staging[testTenantID+":"+digestHex], len(store.objects))
	}
}

func TestBundleServiceDoesNotExposeMetadataBeforeObjectIsPublished(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &blockingMemoryObjectStore{inner: &atomicMemoryObjectStore{}, started: make(chan struct{}), release: make(chan struct{})}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	result := make(chan BundleMetadata, 1)
	errorsChannel := make(chan error, 1)
	body := validExternalBundle(t, "input")
	go func() {
		metadata, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
		if err != nil {
			errorsChannel <- err
			return
		}
		result <- metadata
	}()
	<-store.started

	repository.mu.Lock()
	var pendingID string
	for _, metadata := range repository.bundles {
		pendingID = metadata.BundleID
	}
	repository.mu.Unlock()
	if pendingID == "" {
		t.Fatal("ownership record was not committed before object publication")
	}
	if _, err := service.Get(context.Background(), testTenantID, pendingID); !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("pending metadata became visible: %v", err)
	}
	close(store.release)
	select {
	case err := <-errorsChannel:
		t.Fatal(err)
	case metadata := <-result:
		if metadata.BundleID != pendingID {
			t.Fatalf("metadata=%+v pendingID=%s", metadata, pendingID)
		}
		if _, err := service.Get(context.Background(), testTenantID, pendingID); err != nil {
			t.Fatalf("ready metadata is not visible: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not complete after publication was released")
	}
}

func TestBundleServiceStreamsValidUploadToTenantContentAddress(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	tempDir := t.TempDir()
	service := newTestBundleService(t, repository, store, tempDir, 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")

	result, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	wantKey := "external/" + testTenantID + "/sha256/" + hex.EncodeToString(digest[:]) + ".zip"
	if replay || result.SHA256 != hex.EncodeToString(digest[:]) || result.SizeBytes != int64(len(body)) || result.CaseCount != 1 || result.ManifestVersion != 1 || result.BundleID == "" {
		t.Fatalf("result=%+v replay=%v", result, replay)
	}
	if len(store.objects) != 1 || !bytes.Equal(store.objects[wantKey], body) || repository.logicalCreates != 1 {
		t.Fatalf("objects=%v logicalCreates=%d", mapsKeys(store.objects), repository.logicalCreates)
	}
	idempotencyDigest, err := DigestIdempotencyKey("upload-key-00001", testIdempotencyPepper())
	if err != nil {
		t.Fatal(err)
	}
	if _, found := repository.idempotency[testTenantID+":"+hex.EncodeToString(idempotencyDigest)]; !found {
		t.Fatal("bundle upload must use the shared HMAC idempotency digest")
	}
	assertDirectoryEmpty(t, tempDir)
}

func TestNewBundleServiceRejectsShortIdempotencyPepper(t *testing.T) {
	_, err := NewBundleService(newMemoryBundleRepository(), &atomicMemoryObjectStore{}, BundleServiceConfig{
		TempDir: t.TempDir(), MaxUploadBytes: 1 << 20, ArchiveLimits: bundle.DefaultArchiveLimits(),
		IdempotencyTTL: time.Hour, IdempotencyPepper: bytes.Repeat([]byte{0x11}, sha256.Size-1),
	})
	if err == nil {
		t.Fatal("expected short idempotency pepper rejection")
	}
}

func TestBundleServiceRejectsOversizeAndCancelledStreamsWithoutArtifacts(t *testing.T) {
	for name, test := range map[string]struct {
		context context.Context
		reader  io.Reader
		limit   int64
		want    error
	}{
		"oversize":  {context: context.Background(), reader: strings.NewReader(strings.Repeat("x", 65)), limit: 64, want: ErrBundleTooLarge},
		"cancelled": {context: cancelledContext(), reader: strings.NewReader(strings.Repeat("x", 32)), limit: 64, want: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newMemoryBundleRepository()
			store := &atomicMemoryObjectStore{}
			tempDir := t.TempDir()
			service := newTestBundleService(t, repository, store, tempDir, test.limit, bundle.DefaultArchiveLimits())
			_, _, err := service.Upload(test.context, testTenantID, "upload-key-00001", test.reader)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if len(store.objects) != 0 || repository.logicalCreates != 0 {
				t.Fatalf("partial artifact became visible: objects=%d rows=%d", len(store.objects), repository.logicalCreates)
			}
			assertDirectoryEmpty(t, tempDir)
		})
	}
}

func TestBundleServiceReusesHardenedArchiveValidation(t *testing.T) {
	validManifest := manifestJSON(t, "cases/1.in", "cases/1.out")
	tests := map[string]struct {
		entries map[string]zipEntry
		limits  bundle.ArchiveLimits
	}{
		"traversal":          {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out")}, "../escape": {body: []byte("x")}}, limits: bundle.DefaultArchiveLimits()},
		"symlink":            {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out"), mode: os.ModeSymlink | 0o777}}, limits: bundle.DefaultArchiveLimits()},
		"missing pair":       {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}}, limits: bundle.DefaultArchiveLimits()},
		"file count":         {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(2, 1<<20, 64<<20, 512<<20, 200)},
		"compression ratio":  {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: bytes.Repeat([]byte("a"), 4096)}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(10, 1<<20, 64<<20, 512<<20, 1)},
		"uncompressed total": {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("12345678")}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(10, 1<<20, 64<<20, int64(len(validManifest)+10), 200)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			repository := newMemoryBundleRepository()
			store := &atomicMemoryObjectStore{}
			tempDir := t.TempDir()
			service := newTestBundleService(t, repository, store, tempDir, 1<<20, test.limits)
			_, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(zipBytes(t, test.entries)))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("error=%v want ErrInvalidBundle", err)
			}
			if len(store.objects) != 0 || repository.logicalCreates != 0 {
				t.Fatal("invalid bundle was published")
			}
			assertDirectoryEmpty(t, tempDir)
		})
	}
}

func TestBundleServiceRejectsManifestAboveExternalCaseLimit(t *testing.T) {
	manifest := bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact}
	entries := map[string]zipEntry{}
	for index := range 257 {
		id := fmt.Sprintf("case-%03d", index)
		input, output := id+".in", id+".out"
		manifest.Cases = append(manifest.Cases, bundle.Case{ID: id, Input: input, Output: output, Weight: 1})
		entries[input] = zipEntry{body: []byte("in")}
		entries[output] = zipEntry{body: []byte("out")}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries["manifest.json"] = zipEntry{body: manifestBytes}
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	_, _, err = service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(zipBytes(t, entries)))
	if !errors.Is(err, ErrInvalidBundle) || repository.logicalCreates != 0 || len(store.objects) != 0 {
		t.Fatalf("error=%v rows=%d objects=%d", err, repository.logicalCreates, len(store.objects))
	}
}

func TestBundleServiceRejectsCorruptCasePayloadBeforePublication(t *testing.T) {
	body := validExternalBundle(t, "input-that-must-pass-crc")
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), body...)
	for _, file := range reader.File {
		if file.Name != "cases/1.in" {
			continue
		}
		offset, err := file.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		corrupted[offset] ^= 0xff
	}
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	_, _, err = service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(corrupted))
	if !errors.Is(err, ErrInvalidBundle) || repository.logicalCreates != 0 || len(store.objects) != 0 {
		t.Fatalf("error=%v rows=%d objects=%d", err, repository.logicalCreates, len(store.objects))
	}
}

func TestBundleServiceIdempotencyAndTenantMetadataIsolation(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	first, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || replay {
		t.Fatalf("first result=%+v replay=%v error=%v", first, replay, err)
	}
	second, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || !replay || second.BundleID != first.BundleID || store.publishes != 1 {
		t.Fatalf("replay result=%+v replay=%v error=%v publishes=%d", second, replay, err, store.publishes)
	}
	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(validExternalBundle(t, "different"))); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error=%v", err)
	}
	if _, err := service.Get(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", first.BundleID); !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("cross-tenant get error=%v", err)
	}
	metadata, err := service.Get(context.Background(), testTenantID, first.BundleID)
	if err != nil || metadata.BundleID != first.BundleID {
		t.Fatalf("owner metadata=%+v error=%v", metadata, err)
	}
}

func TestBundleServiceConcurrentIdenticalUploadsCreateOneLogicalRecord(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	const uploads = 32
	results := make(chan BundleMetadata, uploads)
	errorsChannel := make(chan error, uploads)
	var wait sync.WaitGroup
	for index := range uploads {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			metadata, _, err := service.Upload(context.Background(), testTenantID, fmt.Sprintf("upload-key-%05d", index), bytes.NewReader(body))
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- metadata
		}(index)
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if !errors.Is(err, ErrBundlePublishing) {
			t.Error(err)
		}
	}
	var bundleID string
	for result := range results {
		if bundleID == "" {
			bundleID = result.BundleID
		}
		if result.BundleID != bundleID {
			t.Fatalf("concurrent uploads returned different logical IDs: %q and %q", bundleID, result.BundleID)
		}
	}
	if bundleID == "" || repository.logicalCreates != 1 || len(store.objects) != 1 || store.promoteCalls != 1 {
		t.Fatalf("bundleID=%q logical rows=%d visible objects=%d promote calls=%d", bundleID, repository.logicalCreates, len(store.objects), store.promoteCalls)
	}
}

func newTestBundleService(t *testing.T, repository BundleRepository, store BundleObjectStore, tempDir string, maxUploadBytes int64, limits bundle.ArchiveLimits) *BundleService {
	t.Helper()
	service, err := NewBundleService(repository, store, BundleServiceConfig{
		TempDir: tempDir, MaxUploadBytes: maxUploadBytes, ArchiveLimits: limits,
		IdempotencyTTL: 24 * time.Hour, IdempotencyPepper: testIdempotencyPepper(), Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC) }
	return service
}

func testIdempotencyPepper() []byte {
	return bytes.Repeat([]byte{0x42}, sha256.Size)
}

type zipEntry struct {
	body []byte
	mode os.FileMode
}

func validExternalBundle(t *testing.T, input string) []byte {
	t.Helper()
	manifest := manifestJSON(t, "cases/1.in", "cases/1.out")
	return zipBytes(t, map[string]zipEntry{
		"manifest.json": {body: manifest}, "cases/1.in": {body: []byte(input)}, "cases/1.out": {body: []byte("answer")},
	})
}

func manifestJSON(t *testing.T, input, output string) []byte {
	t.Helper()
	data, err := json.Marshal(bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Cases: []bundle.Case{{ID: "case-1", Input: input, Output: output, Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func zipBytes(t *testing.T, entries map[string]zipEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		} else {
			header.SetMode(0o600)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func archiveLimits(files int, manifest, caseBytes, total int64, ratio uint64) bundle.ArchiveLimits {
	return bundle.ArchiveLimits{MaxFiles: files, MaxManifestBytes: manifest, MaxCaseBytes: caseBytes, MaxTotalBytes: total, MaxCompressionRatio: ratio}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, filepath.Join(directory, entry.Name()))
		}
		t.Fatalf("temporary files were not cleaned: %v", names)
	}
}

func mapsKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
