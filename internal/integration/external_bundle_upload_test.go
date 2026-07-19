package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/httpapi"
	_ "github.com/go-sql-driver/mysql"
)

type integrationCredentialStore struct{ credential *external.Credential }

type integrationAllowQuota struct{}

func (integrationAllowQuota) Allow(context.Context, external.QuotaRequest) (external.QuotaDecision, error) {
	return external.QuotaDecision{Allowed: true}, nil
}

func (store integrationCredentialStore) FindCredentialByPrefix(context.Context, string) (*external.Credential, error) {
	return store.credential, nil
}

type integrationBundleRepository struct {
	mu          sync.Mutex
	idempotency map[string]integrationUploadRecord
	bundles     map[string]external.BundleMetadata
	status      map[string]external.BundlePublicationStatus
	staging     map[string]string
	claims      map[string]external.BundlePublicationClaim
	attempts    map[string]int
}

type integrationUploadRecord struct {
	hash     [sha256.Size]byte
	metadata external.BundleMetadata
}

func newIntegrationBundleRepository() *integrationBundleRepository {
	return &integrationBundleRepository{
		idempotency: map[string]integrationUploadRecord{}, bundles: map[string]external.BundleMetadata{}, status: map[string]external.BundlePublicationStatus{},
		staging: map[string]string{}, claims: map[string]external.BundlePublicationClaim{}, attempts: map[string]int{},
	}
}

func (repository *integrationBundleRepository) FindBundleUpload(_ context.Context, tenant string, digest [sha256.Size]byte) (external.BundleUploadLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, found := repository.idempotency[tenant+hex.EncodeToString(digest[:])]
	bundleKey := tenant + record.metadata.SHA256
	return external.BundleUploadLookup{Found: found, Status: repository.status[bundleKey], RequestHash: record.hash, StagingKey: repository.staging[bundleKey], Metadata: record.metadata}, nil
}

func (repository *integrationBundleRepository) CommitBundleUpload(_ context.Context, input external.BundleCommitInput) (external.BundleCommitResult, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := input.TenantID + hex.EncodeToString(input.IdempotencyDigest[:])
	if record, found := repository.idempotency[key]; found {
		if record.hash != input.RequestHash {
			return external.BundleCommitResult{}, external.ErrIdempotencyConflict
		}
		bundleKey := input.TenantID + record.metadata.SHA256
		return external.BundleCommitResult{Metadata: record.metadata, Replay: true, Status: repository.status[bundleKey], StagingKey: repository.staging[bundleKey]}, nil
	}
	bundleKey := input.TenantID + hex.EncodeToString(input.RequestHash[:])
	metadata, existed := repository.bundles[bundleKey]
	if !existed {
		metadata = input.Metadata
		repository.bundles[bundleKey] = metadata
		repository.status[bundleKey] = external.BundlePublicationPending
		repository.staging[bundleKey] = input.StagingObjectKey
	}
	repository.idempotency[key] = integrationUploadRecord{hash: input.RequestHash, metadata: metadata}
	return external.BundleCommitResult{Metadata: metadata, Replay: existed, Status: repository.status[bundleKey], StagingKey: repository.staging[bundleKey]}, nil
}

func (repository *integrationBundleRepository) ClaimBundlePublication(_ context.Context, tenant, bundleID, leaseToken string, _, _ time.Time) (external.BundlePublicationClaim, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenant) && metadata.BundleID == bundleID {
			if repository.status[key] != external.BundlePublicationPending {
				return external.BundlePublicationClaim{}, false, nil
			}
			repository.status[key] = external.BundlePublicationPublishing
			repository.attempts[key]++
			digestBytes, _ := hex.DecodeString(metadata.SHA256)
			var digest [sha256.Size]byte
			copy(digest[:], digestBytes)
			claim := external.BundlePublicationClaim{TenantID: tenant, BundleID: bundleID, ObjectKey: "external/" + tenant + "/sha256/" + metadata.SHA256 + ".zip", StagingKey: repository.staging[key], RequestHash: digest, SizeBytes: metadata.SizeBytes, LeaseToken: leaseToken, AttemptCount: repository.attempts[key]}
			repository.claims[key] = claim
			return claim, true, nil
		}
	}
	return external.BundlePublicationClaim{}, false, external.ErrBundleNotFound
}

func (repository *integrationBundleRepository) ClaimNextBundlePublication(ctx context.Context, leaseToken string, now, leaseUntil time.Time) (external.BundlePublicationClaim, bool, error) {
	repository.mu.Lock()
	var tenant, bundleID string
	for key, metadata := range repository.bundles {
		if repository.status[key] == external.BundlePublicationPending {
			tenant = strings.TrimSuffix(key, metadata.SHA256)
			bundleID = metadata.BundleID
			break
		}
	}
	repository.mu.Unlock()
	if bundleID == "" {
		return external.BundlePublicationClaim{}, false, nil
	}
	return repository.ClaimBundlePublication(ctx, tenant, bundleID, leaseToken, now, leaseUntil)
}

func (repository *integrationBundleRepository) CompleteBundlePublication(_ context.Context, claim external.BundlePublicationClaim, _ time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := claim.TenantID + hex.EncodeToString(claim.RequestHash[:])
	stored, found := repository.claims[key]
	if !found || stored.LeaseToken != claim.LeaseToken {
		return external.ErrBundlePublishing
	}
	repository.status[key] = external.BundlePublicationReady
	delete(repository.claims, key)
	return nil
}

func (repository *integrationBundleRepository) FailBundlePublication(_ context.Context, claim external.BundlePublicationClaim, _ string, _ time.Time, maxAttempts int) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := claim.TenantID + hex.EncodeToString(claim.RequestHash[:])
	abandoned := claim.AttemptCount >= maxAttempts
	if abandoned {
		repository.status[key] = external.BundlePublicationAbandoned
	} else {
		repository.status[key] = external.BundlePublicationPending
	}
	delete(repository.claims, key)
	return abandoned, nil
}

func (repository *integrationBundleRepository) SweepUnrecoverableBundlePublications(_ context.Context, _ time.Time, _ int) (int64, error) {
	return 0, nil
}

func (repository *integrationBundleRepository) FindBundle(_ context.Context, tenant, bundleID string) (external.BundleMetadata, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenant) && metadata.BundleID == bundleID && repository.status[key] == external.BundlePublicationReady {
			return metadata, nil
		}
	}
	return external.BundleMetadata{}, external.ErrBundleNotFound
}

type integrationFileObjectStore struct {
	root     string
	mu       sync.Mutex
	promotes int
}

type blockingIntegrationObjectStore struct {
	inner   external.BundleObjectStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type flakyIntegrationObjectStore struct {
	inner        external.BundleObjectStore
	mu           sync.Mutex
	failPromotes int
}

func (store *flakyIntegrationObjectStore) Stage(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	return store.inner.Stage(ctx, key, filename, size, digest)
}

func (store *flakyIntegrationObjectStore) Promote(ctx context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.mu.Lock()
	if store.failPromotes > 0 {
		store.failPromotes--
		store.mu.Unlock()
		return errors.New("injected promotion failure")
	}
	store.mu.Unlock()
	return store.inner.Promote(ctx, stagingKey, finalKey, size, digest)
}

func (store *flakyIntegrationObjectStore) Discard(ctx context.Context, key string) error {
	return store.inner.Discard(ctx, key)
}

func (store *blockingIntegrationObjectStore) Stage(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	return store.inner.Stage(ctx, key, filename, size, digest)
}

func (store *blockingIntegrationObjectStore) Promote(ctx context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.once.Do(func() { close(store.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.release:
		return store.inner.Promote(ctx, stagingKey, finalKey, size, digest)
	}
}

func (store *blockingIntegrationObjectStore) Discard(ctx context.Context, key string) error {
	return store.inner.Discard(ctx, key)
}

func (store *integrationFileObjectStore) Stage(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	target := filepath.Join(store.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".upload-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	source, err := os.Open(filename)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	written, copyErr := io.Copy(temporary, source)
	closeErr := temporary.Close()
	_ = source.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	data, err := os.ReadFile(temporaryName)
	if err != nil || written != size || sha256.Sum256(data) != digest {
		return errors.New("staged object mismatch")
	}
	return os.Rename(temporaryName, target)
}

func (store *integrationFileObjectStore) Promote(ctx context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.promotes++
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	staged := filepath.Join(store.root, filepath.FromSlash(stagingKey))
	data, err := os.ReadFile(staged)
	if err != nil || int64(len(data)) != size || sha256.Sum256(data) != digest {
		return errors.New("staged object mismatch")
	}
	target := filepath.Join(store.root, filepath.FromSlash(finalKey))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".promote-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func (store *integrationFileObjectStore) Discard(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	err := os.Remove(filepath.Join(store.root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func TestExternalBundleUploadHTTPIntegration(t *testing.T) {
	tenantID := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	pepper := make([]byte, sha256.Size)
	secretBytes := make([]byte, sha256.Size)
	if _, err := rand.Read(pepper); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	key := "croj_public12_" + secret
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(key))
	authenticator, err := httpapi.NewAuthenticator(integrationCredentialStore{credential: &external.Credential{
		TenantID: tenantID, Digest: digest.Sum(nil), Scopes: []external.Scope{external.ScopeBundleWrite, external.ScopeBundleRead},
	}}, pepper)
	if err != nil {
		t.Fatal(err)
	}
	repository := newIntegrationBundleRepository()
	objectRoot := t.TempDir()
	service, err := external.NewBundleService(repository, &integrationFileObjectStore{root: objectRoot}, external.BundleServiceConfig{
		TempDir: t.TempDir(), MaxUploadBytes: 1 << 20, ArchiveLimits: bundle.DefaultArchiveLimits(), IdempotencyTTL: 24 * time.Hour,
		IdempotencyPepper: bytes.Repeat([]byte{0x42}, sha256.Size),
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := integrationCapabilities()
	server, err := httpapi.NewServer(authenticator, capabilities, httpapi.WithBundleApplication(service),
		httpapi.WithBundleWriteQuota(integrationAllowQuota{}, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	bundleBytes := integrationBundleZIP(t)

	first := performBundleUpload(t, server, key, "upload-key-00001", bundleBytes)
	if first.Code != http.StatusCreated || strings.Contains(strings.ToLower(first.Body.String()), "object") || strings.Contains(strings.ToLower(first.Body.String()), "url") {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var metadata external.BundleMetadata
	if err := json.Unmarshal(first.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	replay := performBundleUpload(t, server, key, "upload-key-00001", bundleBytes)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/bundles/"+metadata.BundleID, nil)
	get.Header.Set("Authorization", "Bearer "+key)
	getResponse := httptest.NewRecorder()
	server.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || !bytes.Equal(getResponse.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("get status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	var visibleObjects []string
	if err := filepath.WalkDir(objectRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			visibleObjects = append(visibleObjects, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(visibleObjects) != 1 || !strings.HasSuffix(visibleObjects[0], metadata.SHA256+".zip") {
		t.Fatalf("visible objects=%v", visibleObjects)
	}
}

func TestExternalBundleSQLRepositoryIntegration(t *testing.T) {
	dsn := os.Getenv("EXTERNAL_JUDGE_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set EXTERNAL_JUDGE_MYSQL_TEST_DSN to run MySQL bundle integration")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(32)
	database.SetMaxIdleConns(32)
	if err := external.ApplyMigrations(context.Background(), database); err != nil {
		t.Fatal(err)
	}

	t.Run("same key and hash replay one row", func(t *testing.T) {
		tenantID, service, objectStore := newSQLBundleFixture(t, database)
		body := integrationBundleZIPWithInput(t, "same")
		requests := make([]sqlUploadRequest, 16)
		for index := range requests {
			requests[index] = sqlUploadRequest{idempotencyKey: "same-key-0000001", body: body}
		}
		results := runConcurrentSQLUploads(service, tenantID, requests)
		assertSuccessfulSQLUploadGroup(t, results, 1)
		assertSQLBundleRows(t, database, tenantID, 1, 1)
		assertVisibleBundleObjects(t, objectStore.root, 1)
		if objectStore.promotes != 1 {
			t.Fatalf("remote promote calls=%d want=1", objectStore.promotes)
		}
	})

	t.Run("different keys and same hash deduplicate bundle", func(t *testing.T) {
		tenantID, service, objectStore := newSQLBundleFixture(t, database)
		body := integrationBundleZIPWithInput(t, "same")
		requests := make([]sqlUploadRequest, 16)
		for index := range requests {
			requests[index] = sqlUploadRequest{idempotencyKey: "unique-key-" + strings.Repeat("0", 5-len(strconv.Itoa(index))) + strconv.Itoa(index), body: body}
		}
		results := runConcurrentSQLUploads(service, tenantID, requests)
		assertSuccessfulSQLUploadGroup(t, results, 1)
		assertSQLBundleRows(t, database, tenantID, 1, len(requests))
		assertVisibleBundleObjects(t, objectStore.root, 1)
		if objectStore.promotes != 1 {
			t.Fatalf("remote promote calls=%d want=1", objectStore.promotes)
		}
	})

	t.Run("same key and different hash conflict", func(t *testing.T) {
		tenantID, service, objectStore := newSQLBundleFixture(t, database)
		results := runConcurrentSQLUploads(service, tenantID, []sqlUploadRequest{
			{idempotencyKey: "conflict-key-0001", body: integrationBundleZIPWithInput(t, "first")},
			{idempotencyKey: "conflict-key-0001", body: integrationBundleZIPWithInput(t, "second")},
		})
		successes, conflicts := 0, 0
		for _, result := range results {
			switch {
			case result.err == nil:
				successes++
			case errors.Is(result.err, external.ErrIdempotencyConflict):
				conflicts++
			default:
				t.Fatalf("unexpected concurrent result error: %v", result.err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
		}
		assertSQLBundleRows(t, database, tenantID, 1, 1)
		assertVisibleBundleObjects(t, objectStore.root, 1)
		if objectStore.promotes != 1 {
			t.Fatalf("remote promote calls=%d want=1", objectStore.promotes)
		}
	})

	t.Run("pending ownership is invisible until publish completes", func(t *testing.T) {
		objectRoot := t.TempDir()
		store := &blockingIntegrationObjectStore{
			inner: &integrationFileObjectStore{root: objectRoot}, started: make(chan struct{}), release: make(chan struct{}),
		}
		tenantID, service := newSQLBundleFixtureWithStore(t, database, store)
		body := integrationBundleZIPWithInput(t, "pending")
		result := make(chan sqlUploadResult, 1)
		go func() {
			metadata, replay, err := service.Upload(context.Background(), tenantID, "pending-key-0001", bytes.NewReader(body))
			result <- sqlUploadResult{metadata: metadata, replay: replay, err: err}
		}()
		<-store.started

		var pendingID string
		if err := database.QueryRow(`
SELECT bundle.external_id
FROM t_external_bundle AS bundle
JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id
WHERE tenant.external_id = ? AND bundle.ready_at IS NULL`, tenantID).Scan(&pendingID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Get(context.Background(), tenantID, pendingID); !errors.Is(err, external.ErrBundleNotFound) {
			t.Fatalf("pending metadata became visible: %v", err)
		}
		close(store.release)
		select {
		case upload := <-result:
			if upload.err != nil || upload.replay || upload.metadata.BundleID != pendingID {
				t.Fatalf("upload=%+v pendingID=%s", upload, pendingID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("upload did not complete after object publication")
		}
		if _, err := service.Get(context.Background(), tenantID, pendingID); err != nil {
			t.Fatalf("ready metadata is not visible: %v", err)
		}
		assertVisibleBundleObjects(t, objectRoot, 1)
	})

	t.Run("durable reconciler completes without client replay", func(t *testing.T) {
		objectStore := &integrationFileObjectStore{root: t.TempDir()}
		flaky := &flakyIntegrationObjectStore{inner: objectStore, failPromotes: 1}
		tenantID, service := newSQLBundleFixtureWithStore(t, database, flaky)
		body := integrationBundleZIPWithInput(t, "reconcile")
		if _, _, err := service.Upload(context.Background(), tenantID, "reconcile-key-01", bytes.NewReader(body)); err == nil {
			t.Fatal("expected injected promotion failure")
		}
		time.Sleep(20 * time.Millisecond)
		reconciler, err := external.NewBundleReconciler(service)
		if err != nil {
			t.Fatal(err)
		}
		processed, err := reconciler.ReconcileOnce(context.Background())
		if err != nil || !processed {
			t.Fatalf("processed=%v error=%v", processed, err)
		}
		var bundleID string
		if err := database.QueryRow(`
SELECT bundle.external_id FROM t_external_bundle AS bundle
JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id
WHERE tenant.external_id = ? AND bundle.publication_status = 'READY'`, tenantID).Scan(&bundleID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Get(context.Background(), tenantID, bundleID); err != nil {
			t.Fatal(err)
		}
		assertVisibleBundleObjects(t, objectStore.root, 1)
	})

	t.Run("legacy row without staged bytes can be revived by a fresh upload", func(t *testing.T) {
		tenantID, service, objectStore := newSQLBundleFixture(t, database)
		body := integrationBundleZIPWithInput(t, "legacy-revival")
		digest := sha256.Sum256(body)
		digestHex := hex.EncodeToString(digest[:])
		var tenantInternalID uint64
		if err := database.QueryRow(`SELECT id FROM t_external_tenant WHERE external_id = ?`, tenantID).Scan(&tenantInternalID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`
INSERT INTO t_external_bundle(external_id, tenant_id, sha256, object_key, size_bytes, case_count, manifest_version, manifest_json, created_at)
VALUES ('zzzzzzzzzzzzzzzzzzzzzzzzzz', ?, ?, ?, ?, 1, 1, '{"schemaVersion":1}', UTC_TIMESTAMP(3))`,
			tenantInternalID, digest[:], "external/"+tenantID+"/sha256/"+digestHex+".zip", len(body)); err != nil {
			t.Fatal(err)
		}
		repository, _ := external.NewSQLBundleRepository(database)
		swept, err := repository.SweepUnrecoverableBundlePublications(context.Background(), time.Now().Add(time.Hour), 10)
		if err != nil || swept != 1 {
			t.Fatalf("swept=%d error=%v", swept, err)
		}
		var status string
		if err := database.QueryRow(`SELECT publication_status FROM t_external_bundle WHERE external_id = 'zzzzzzzzzzzzzzzzzzzzzzzzzz'`).Scan(&status); err != nil || status != "ABANDONED" {
			t.Fatalf("status=%q error=%v", status, err)
		}
		metadata, replay, err := service.Upload(context.Background(), tenantID, "legacy-revival-01", bytes.NewReader(body))
		if err != nil || !replay || metadata.BundleID != "zzzzzzzzzzzzzzzzzzzzzzzzzz" {
			t.Fatalf("metadata=%+v replay=%v error=%v", metadata, replay, err)
		}
		if err := database.QueryRow(`SELECT publication_status FROM t_external_bundle WHERE external_id = 'zzzzzzzzzzzzzzzzzzzzzzzzzz'`).Scan(&status); err != nil || status != "READY" {
			t.Fatalf("revived status=%q error=%v", status, err)
		}
		assertVisibleBundleObjects(t, objectStore.root, 1)
	})
}

type sqlUploadRequest struct {
	idempotencyKey string
	body           []byte
}

type sqlUploadResult struct {
	metadata external.BundleMetadata
	replay   bool
	err      error
}

func newSQLBundleFixture(t *testing.T, database *sql.DB) (string, *external.BundleService, *integrationFileObjectStore) {
	t.Helper()
	objectRoot := t.TempDir()
	store := &integrationFileObjectStore{root: objectRoot}
	tenantID, service := newSQLBundleFixtureWithStore(t, database, store)
	return tenantID, service, store
}

func newSQLBundleFixtureWithStore(t *testing.T, database *sql.DB, store external.BundleObjectStore) (string, *external.BundleService) {
	t.Helper()
	tenantID := randomExternalID(t)
	if _, err := database.Exec(`INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES (?, ?, 'ACTIVE', ?)`, tenantID, "bundle-integration", `{"maxQueuedJobs":10}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec(`DELETE FROM t_external_idempotency WHERE tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?)`, tenantID)
		_, _ = database.Exec(`DELETE FROM t_external_bundle WHERE tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?)`, tenantID)
		_, _ = database.Exec(`DELETE FROM t_external_tenant WHERE external_id = ?`, tenantID)
	})
	repository, err := external.NewSQLBundleRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := external.NewBundleService(repository, store, external.BundleServiceConfig{
		TempDir: t.TempDir(), MaxUploadBytes: 1 << 20, ArchiveLimits: bundle.DefaultArchiveLimits(), IdempotencyTTL: time.Hour,
		IdempotencyPepper: bytes.Repeat([]byte{0x42}, sha256.Size), PublicationRetry: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenantID, service
}

func runConcurrentSQLUploads(service *external.BundleService, tenantID string, requests []sqlUploadRequest) []sqlUploadResult {
	start := make(chan struct{})
	results := make(chan sqlUploadResult, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			metadata, replay, err := service.Upload(context.Background(), tenantID, request.idempotencyKey, bytes.NewReader(request.body))
			results <- sqlUploadResult{metadata: metadata, replay: replay, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	collected := make([]sqlUploadResult, 0, len(requests))
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func assertSuccessfulSQLUploadGroup(t *testing.T, results []sqlUploadResult, wantCreated int) {
	t.Helper()
	bundleID := ""
	created := 0
	successful := 0
	for _, result := range results {
		if result.err != nil {
			if errors.Is(result.err, external.ErrBundlePublishing) {
				continue
			}
			t.Fatalf("concurrent upload error: %v", result.err)
		}
		successful++
		if bundleID == "" {
			bundleID = result.metadata.BundleID
		}
		if result.metadata.BundleID != bundleID {
			t.Fatalf("bundle IDs differ: %q and %q", bundleID, result.metadata.BundleID)
		}
		if !result.replay {
			created++
		}
	}
	if successful == 0 || created > wantCreated {
		t.Fatalf("successful responses=%d created responses=%d max=%d", successful, created, wantCreated)
	}
}

func assertSQLBundleRows(t *testing.T, database *sql.DB, tenantID string, wantBundles, wantIdempotency int) {
	t.Helper()
	var bundles, idempotency int
	if err := database.QueryRow(`SELECT COUNT(*) FROM t_external_bundle AS bundle JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id WHERE tenant.external_id = ?`, tenantID).Scan(&bundles); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM t_external_idempotency AS idempotency JOIN t_external_tenant AS tenant ON tenant.id = idempotency.tenant_id WHERE tenant.external_id = ? AND idempotency.operation_scope = 'bundle-upload'`, tenantID).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if bundles != wantBundles || idempotency != wantIdempotency {
		t.Fatalf("bundle rows=%d/%d idempotency rows=%d/%d", bundles, wantBundles, idempotency, wantIdempotency)
	}
}

func assertVisibleBundleObjects(t *testing.T, root string, want int) {
	t.Helper()
	visible := 0
	if err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			visible++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if visible != want {
		t.Fatalf("visible bundle objects=%d want=%d", visible, want)
	}
}

func performBundleUpload(t *testing.T, server http.Handler, apiKey, idempotencyKey string, bundleBytes []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("bundle", "hidden.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bundleBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", &body)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func integrationBundleZIP(t *testing.T) []byte {
	return integrationBundleZIPWithInput(t, "input")
}

func integrationBundleZIPWithInput(t *testing.T, input string) []byte {
	t.Helper()
	manifest, err := json.Marshal(bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Cases: []bundle.Case{{ID: "case-1", Input: "1.in", Output: "1.out", Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for name, content := range map[string][]byte{"manifest.json": manifest, "1.in": []byte(input), "1.out": []byte("answer")} {
		part, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func integrationCapabilities() httpapi.Capabilities {
	return httpapi.Capabilities{APIVersion: "v1", Languages: []httpapi.LanguageCapability{{ID: "cpp20", DisplayName: "C++20", Runtime: "gcc"}}, JudgeModes: []string{"ACM"}, Checkers: []string{"EXACT"}, Limits: httpapi.CapabilityLimits{MaxSourceBytes: 1 << 20, MaxBundleBytes: 1 << 20, MaxCaseBytes: 1 << 20, MaxCaseCount: 256}}
}

func randomExternalID(t *testing.T) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random))
}
