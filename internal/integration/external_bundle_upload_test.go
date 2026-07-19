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
}

type integrationUploadRecord struct {
	hash     [sha256.Size]byte
	metadata external.BundleMetadata
}

func newIntegrationBundleRepository() *integrationBundleRepository {
	return &integrationBundleRepository{idempotency: map[string]integrationUploadRecord{}, bundles: map[string]external.BundleMetadata{}}
}

func (repository *integrationBundleRepository) FindBundleUpload(_ context.Context, tenant string, digest [sha256.Size]byte) (external.BundleUploadLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, found := repository.idempotency[tenant+hex.EncodeToString(digest[:])]
	return external.BundleUploadLookup{Found: found, RequestHash: record.hash, Metadata: record.metadata}, nil
}

func (repository *integrationBundleRepository) CommitBundleUpload(_ context.Context, input external.BundleCommitInput) (external.BundleCommitResult, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key := input.TenantID + hex.EncodeToString(input.IdempotencyDigest[:])
	if record, found := repository.idempotency[key]; found {
		if record.hash != input.RequestHash {
			return external.BundleCommitResult{}, external.ErrIdempotencyConflict
		}
		return external.BundleCommitResult{Metadata: record.metadata, Replay: true}, nil
	}
	bundleKey := input.TenantID + hex.EncodeToString(input.RequestHash[:])
	metadata, existed := repository.bundles[bundleKey]
	if !existed {
		metadata = input.Metadata
		repository.bundles[bundleKey] = metadata
	}
	repository.idempotency[key] = integrationUploadRecord{hash: input.RequestHash, metadata: metadata}
	return external.BundleCommitResult{Metadata: metadata, Replay: existed}, nil
}

func (repository *integrationBundleRepository) FindBundle(_ context.Context, tenant, bundleID string) (external.BundleMetadata, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenant) && metadata.BundleID == bundleID {
			return metadata, nil
		}
	}
	return external.BundleMetadata{}, external.ErrBundleNotFound
}

type integrationFileObjectStore struct {
	root string
	mu   sync.Mutex
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
	if err := external.ApplyMigrations(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	tenantID := randomExternalID(t)
	if _, err := database.Exec(`INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES (?, ?, 'ACTIVE', ?)`, tenantID, "bundle-integration", `{"maxQueuedJobs":10}`); err != nil {
		t.Fatal(err)
	}
	defer database.Exec(`DELETE FROM t_external_tenant WHERE external_id = ?`, tenantID)
	repository, err := external.NewSQLBundleRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := external.NewBundleService(repository, &integrationFileObjectStore{root: t.TempDir()}, external.BundleServiceConfig{
		TempDir: t.TempDir(), MaxUploadBytes: 1 << 20, ArchiveLimits: bundle.DefaultArchiveLimits(), IdempotencyTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, replay, err := service.Upload(context.Background(), tenantID, "upload-key-00001", bytes.NewReader(integrationBundleZIP(t)))
	if err != nil || replay {
		t.Fatalf("metadata=%+v replay=%v error=%v", metadata, replay, err)
	}
	defer func() {
		_, _ = database.Exec(`DELETE FROM t_external_idempotency WHERE tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?)`, tenantID)
		_, _ = database.Exec(`DELETE FROM t_external_bundle WHERE tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?)`, tenantID)
	}()
	got, err := service.Get(context.Background(), tenantID, metadata.BundleID)
	if err != nil || got.BundleID != metadata.BundleID {
		t.Fatalf("got=%+v error=%v", got, err)
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
	t.Helper()
	manifest, err := json.Marshal(bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Cases: []bundle.Case{{ID: "case-1", Input: "1.in", Output: "1.out", Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for name, content := range map[string][]byte{"manifest.json": manifest, "1.in": []byte("input"), "1.out": []byte("answer")} {
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
