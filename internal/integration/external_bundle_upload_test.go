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

func (store integrationCredentialStore) FindCredentialByPrefix(context.Context, string) (*external.Credential, error) {
	return store.credential, nil
}

type integrationBundleRepository struct {
	mu          sync.Mutex
	idempotency map[string]integrationUploadRecord
	bundles     map[string]external.BundleMetadata
	ready       map[string]bool
}

type integrationUploadRecord struct {
	hash     [sha256.Size]byte
	metadata external.BundleMetadata
}

func newIntegrationBundleRepository() *integrationBundleRepository {
	return &integrationBundleRepository{idempotency: map[string]integrationUploadRecord{}, bundles: map[string]external.BundleMetadata{}, ready: map[string]bool{}}
}

func (repository *integrationBundleRepository) FindBundleUpload(_ context.Context, tenant string, digest [sha256.Size]byte) (external.BundleUploadLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, found := repository.idempotency[tenant+hex.EncodeToString(digest[:])]
	bundleKey := tenant + record.metadata.SHA256
	return external.BundleUploadLookup{Found: found, Ready: found && repository.ready[bundleKey], RequestHash: record.hash, Metadata: record.metadata}, nil
}

func (repository *integrationBundleRepository) CommitBundleUpload(_ context.Context, input external.BundleCommitInput) (external.BundleCommitResult, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := input.TenantID + hex.EncodeToString(input.IdempotencyDigest[:])
	if record, found := repository.idempotency[key]; found {
		if record.hash != input.RequestHash {
			return external.BundleCommitResult{}, external.ErrIdempotencyConflict
		}
		return external.BundleCommitResult{Metadata: record.metadata, Replay: true, Ready: repository.ready[input.TenantID+record.metadata.SHA256]}, nil
	}
	bundleKey := input.TenantID + hex.EncodeToString(input.RequestHash[:])
	metadata, existed := repository.bundles[bundleKey]
	if !existed {
		metadata = input.Metadata
		repository.bundles[bundleKey] = metadata
	}
	repository.idempotency[key] = integrationUploadRecord{hash: input.RequestHash, metadata: metadata}
	return external.BundleCommitResult{Metadata: metadata, Replay: existed, Ready: repository.ready[bundleKey]}, nil
}

func (repository *integrationBundleRepository) MarkBundleReady(_ context.Context, tenant, bundleID string, digest [sha256.Size]byte) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	bundleKey := tenant + hex.EncodeToString(digest[:])
	metadata, found := repository.bundles[bundleKey]
	if !found || metadata.BundleID != bundleID {
		return external.ErrBundleNotFound
	}
	repository.ready[bundleKey] = true
	return nil
}

func (repository *integrationBundleRepository) FindBundle(_ context.Context, tenant, bundleID string) (external.BundleMetadata, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenant) && metadata.BundleID == bundleID && repository.ready[key] {
			return metadata, nil
		}
	}
	return external.BundleMetadata{}, external.ErrBundleNotFound
}

type integrationFileObjectStore struct {
	root string
	mu   sync.Mutex
}

type blockingIntegrationObjectStore struct {
	inner   external.BundleObjectStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (store *blockingIntegrationObjectStore) Publish(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	store.once.Do(func() { close(store.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.release:
		return store.inner.Publish(ctx, key, filename, size, digest)
	}
}

func (store *integrationFileObjectStore) Publish(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
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

func TestExternalBundleUploadHTTPIntegration(t *testing.T) {
	tenantID := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	pepper := []byte("0123456789abcdef0123456789abcdef")
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
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
	server, err := httpapi.NewServer(authenticator, integrationCapabilities(), httpapi.WithBundleApplication(service))
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
		tenantID, service, objectRoot := newSQLBundleFixture(t, database)
		body := integrationBundleZIPWithInput(t, "same")
		requests := make([]sqlUploadRequest, 16)
		for index := range requests {
			requests[index] = sqlUploadRequest{idempotencyKey: "same-key-0000001", body: body}
		}
		results := runConcurrentSQLUploads(service, tenantID, requests)
		assertSuccessfulSQLUploadGroup(t, results, 1)
		assertSQLBundleRows(t, database, tenantID, 1, 1)
		assertVisibleBundleObjects(t, objectRoot, 1)
	})

	t.Run("different keys and same hash deduplicate bundle", func(t *testing.T) {
		tenantID, service, objectRoot := newSQLBundleFixture(t, database)
		body := integrationBundleZIPWithInput(t, "same")
		requests := make([]sqlUploadRequest, 16)
		for index := range requests {
			requests[index] = sqlUploadRequest{idempotencyKey: "unique-key-" + strings.Repeat("0", 5-len(strconv.Itoa(index))) + strconv.Itoa(index), body: body}
		}
		results := runConcurrentSQLUploads(service, tenantID, requests)
		assertSuccessfulSQLUploadGroup(t, results, 1)
		assertSQLBundleRows(t, database, tenantID, 1, len(requests))
		assertVisibleBundleObjects(t, objectRoot, 1)
	})

	t.Run("same key and different hash conflict", func(t *testing.T) {
		tenantID, service, objectRoot := newSQLBundleFixture(t, database)
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
		assertVisibleBundleObjects(t, objectRoot, 1)
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

func newSQLBundleFixture(t *testing.T, database *sql.DB) (string, *external.BundleService, string) {
	t.Helper()
	objectRoot := t.TempDir()
	tenantID, service := newSQLBundleFixtureWithStore(t, database, &integrationFileObjectStore{root: objectRoot})
	return tenantID, service, objectRoot
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
		IdempotencyPepper: bytes.Repeat([]byte{0x42}, sha256.Size),
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
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("concurrent upload error: %v", result.err)
		}
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
	if created != wantCreated {
		t.Fatalf("created responses=%d want=%d", created, wantCreated)
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
