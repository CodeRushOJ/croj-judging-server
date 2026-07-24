package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/discovery"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/httpapi"
	"github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"
	judgesandbox "github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	"github.com/CodeRushOJ/croj-judging-server/internal/scheduler"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/internal/worker"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	_ "github.com/go-sql-driver/mysql"
)

func TestExternalAsyncRESTEndToEndSettlesThroughMySQLRunnerAndGRPCSandbox(t *testing.T) {
	dsn := os.Getenv("ASYNC_REST_E2E_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ASYNC_REST_E2E_MYSQL_DSN is not configured")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(16)
	database.SetMaxIdleConns(8)
	resetAsyncRESTDatabase(t, database)

	pepper := bytes.Repeat([]byte{0x31}, sha256.Size)
	provisioner, err := external.NewProvisioner(database, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := provisioner.CreateTenant(context.Background(), "async-rest-e2e", external.TenantPolicy{
		MaxQueuedJobs: 8, MaxRunningJobs: 2, MaxSourceBytes: 1 << 20,
		MaxRetainedBundles: 20, DailyExecutionMillis: 60_000, MaxInfrastructureTries: 3,
		MaxTimeLimitMillis: 10_000, MaxMemoryLimitMiB: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := provisioner.CreateAPIKey(context.Background(), tenantID, []external.Scope{
		external.ScopeCapabilitiesRead, external.ScopeBundleWrite, external.ScopeBundleRead,
		external.ScopeJobSubmit, external.ScopeJobRead,
	}, nil, pepper)
	if err != nil {
		t.Fatal(err)
	}
	bundleRepository, err := external.NewSQLBundleRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	bundleStore := newAsyncBundleStore(1)
	bundleService, err := external.NewBundleService(bundleRepository, bundleStore, external.BundleServiceConfig{
		TempDir: t.TempDir(), MaxUploadBytes: 1 << 20, ArchiveLimits: bundle.DefaultArchiveLimits(),
		MaxTimeLimitMillis: 10_000, MaxMemoryLimitMiB: 1024,
		IdempotencyTTL: 24 * time.Hour, IdempotencyPepper: bytes.Repeat([]byte{0x61}, sha256.Size),
		PublicationLease: 2 * time.Second, PublicationRetry: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundleReconciler, err := external.NewBundleReconciler(bundleService)
	if err != nil {
		t.Fatal(err)
	}
	bundleCache, err := bundle.NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, bundleStore)
	if err != nil {
		t.Fatal(err)
	}
	provider := bundle.NewProvider(bundleCache, bundle.DefaultArchiveLimits())

	sourceStore := newAsyncMemorySourceStore()
	sourceCipher, err := external.NewSourceCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{0x41}, 32)}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := external.NewMySQLJobRepository(external.MySQLJobRepositoryConfig{
		Database: database, Random: rand.Reader, Now: time.Now,
		IdempotencyPepper: bytes.Repeat([]byte{0x51}, sha256.Size),
		CursorKey:         bytes.Repeat([]byte{0x52}, sha256.Size),
		SourceCipher:      sourceCipher, SourceObjects: sourceStore,
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobService, err := httpapi.NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	credentialStore, err := external.NewSQLCredentialStore(database)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := httpapi.NewAuthenticator(credentialStore, pepper)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := canonicalAsyncRESTCapabilities()
	handler, err := httpapi.NewServer(authenticator, capabilities,
		httpapi.WithJobService(jobService),
		httpapi.WithJobWriteQuota(asyncAllowQuota{}, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}),
		httpapi.WithBundleApplication(bundleService),
		httpapi.WithBundleWriteQuota(asyncAllowQuota{}, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}),
	)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	var published httpapi.Capabilities
	asyncRESTJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/capabilities", apiKey.Plaintext, "", nil, http.StatusOK, &published)
	if !reflectCanonicalCapabilities(published) {
		t.Fatalf("published canonical capabilities = %+v", published)
	}

	bundleArchive := asyncRESTBundleArchive(t)
	firstUploadStatus, firstUploadBody := asyncRESTBundleUpload(t, httpServer.Client(), httpServer.URL, apiKey.Plaintext, "async-rest-e2e-bundle-0001", bundleArchive)
	if firstUploadStatus != http.StatusServiceUnavailable {
		t.Fatalf("initial interrupted bundle publication status=%d body=%s", firstUploadStatus, firstUploadBody)
	}
	reconcileDeadline := time.Now().Add(2 * time.Second)
	for {
		processed, reconcileErr := bundleReconciler.ReconcileOnce(context.Background())
		if reconcileErr != nil {
			t.Fatalf("reconcile durable bundle publication: %v", reconcileErr)
		}
		if processed {
			break
		}
		if time.Now().After(reconcileDeadline) {
			t.Fatal("durable bundle publication did not become eligible for reconciliation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	secondUploadStatus, secondUploadBody := asyncRESTBundleUpload(t, httpServer.Client(), httpServer.URL, apiKey.Plaintext, "async-rest-e2e-bundle-0001", bundleArchive)
	if secondUploadStatus != http.StatusOK {
		t.Fatalf("replayed reconciled bundle upload status=%d body=%s", secondUploadStatus, secondUploadBody)
	}
	var bundleMetadata external.BundleMetadata
	if err := json.Unmarshal(secondUploadBody, &bundleMetadata); err != nil || bundleMetadata.BundleID == "" {
		t.Fatalf("decode reconciled bundle metadata: %+v error=%v body=%s", bundleMetadata, err, secondUploadBody)
	}
	var readBundle external.BundleMetadata
	asyncRESTJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/bundles/"+bundleMetadata.BundleID, apiKey.Plaintext, "", nil, http.StatusOK, &readBundle)
	if readBundle.BundleID != bundleMetadata.BundleID {
		t.Fatalf("read published bundle=%+v uploaded=%+v", readBundle, bundleMetadata)
	}

	var submitted httpapi.JobView
	asyncRESTJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/judge-jobs", apiKey.Plaintext,
		"async-rest-e2e-submit-0001", map[string]any{
			"bundleId": bundleMetadata.BundleID, "language": "cpp", "sourceCode": "int main(){}", "stopOnFailure": true,
		}, http.StatusAccepted, &submitted)
	if submitted.Status != httpapi.JobQueued || submitted.JobID == "" {
		t.Fatalf("submitted job = %+v", submitted)
	}

	sandboxRequests := make(chan *sandboxpb.ExecuteRequest, 1)
	address, stopSandbox := startGRPCSandbox(t, func(_ context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		sandboxRequests <- request
		return &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: request.ExpectedOutput, TimeUsed: 7, MemoryUsed: 256}, nil
	})
	t.Cleanup(stopSandbox)
	endpointAPI := newEndpointSliceAPI(t)
	endpointAPI.SetEndpoint(t, address)
	discoverer, err := discovery.NewKubernetesDiscovery("coderushoj", "croj-sandbox", "grpc", writeKubeconfig(t, endpointAPI.URL()))
	if err != nil {
		t.Fatal(err)
	}
	selector := scheduler.New(discoverer)
	if err := selector.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sandboxClient := judgesandbox.NewClientWithCache(2*time.Second, 2, time.Minute)
	t.Cleanup(func() { _ = sandboxClient.Close() })
	runner, err := worker.NewRunner(repository, provider, service.NewBatchBundlePipeline(selector, sandboxClient, 1), worker.Config{
		LeaseDuration: 30 * time.Second, HeartbeatInterval: 5 * time.Second,
		ControlPollInterval: 5 * time.Second, RetryDelay: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "async-rest-e2e-worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Job.ExternalID != submitted.JobID {
		t.Fatalf("claimed job %q, submitted %q", claim.Job.ExternalID, submitted.JobID)
	}
	if err := runner.ExecuteClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	sandboxRequest := <-sandboxRequests
	if sandboxRequest.Language != "cpp" || sandboxRequest.SourceCode != "int main(){}" || sandboxRequest.Stdin != "hidden input" || sandboxRequest.ExpectedOutput != "hidden output\n" {
		t.Fatalf("Sandbox request drifted from REST and bundle contracts: %+v", sandboxRequest)
	}

	var settled httpapi.JobView
	asyncRESTJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+submitted.StatusURL, apiKey.Plaintext, "", nil, http.StatusOK, &settled)
	if settled.Status != httpapi.JobSucceeded || settled.Result == nil || settled.Result.Verdict != string(callback.StatusAccepted) ||
		settled.Result.TimeMillis != 7 || settled.Result.MemoryBytes != 256*1024 || len(settled.Result.Cases) != 1 {
		t.Fatalf("settled job = %+v", settled)
	}
}

type asyncAllowQuota struct{}

func (asyncAllowQuota) Allow(context.Context, external.QuotaRequest) (external.QuotaDecision, error) {
	return external.QuotaDecision{Allowed: true}, nil
}

type asyncBundleStore struct {
	mu           sync.Mutex
	objects      map[string][]byte
	failPromotes int
}

func newAsyncBundleStore(failPromotes int) *asyncBundleStore {
	return &asyncBundleStore{objects: make(map[string][]byte), failPromotes: failPromotes}
}

func (store *asyncBundleStore) Stage(_ context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if int64(len(data)) != size || sha256.Sum256(data) != digest {
		return errors.New("staged bundle metadata mismatch")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = append([]byte(nil), data...)
	return nil
}

func (store *asyncBundleStore) Promote(_ context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failPromotes > 0 {
		store.failPromotes--
		return errors.New("injected outcome-ambiguous promotion failure")
	}
	data, exists := store.objects[stagingKey]
	if !exists || int64(len(data)) != size || sha256.Sum256(data) != digest {
		return errors.New("staged bundle is unavailable")
	}
	store.objects[finalKey] = append([]byte(nil), data...)
	return nil
}

func (store *asyncBundleStore) Discard(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.objects, key)
	return nil
}

func (store *asyncBundleStore) Open(_ context.Context, key string) (bundle.Object, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	data, exists := store.objects[key]
	if !exists {
		return bundle.Object{}, errors.New("bundle object not found")
	}
	copyOfData := append([]byte(nil), data...)
	return bundle.Object{Body: io.NopCloser(bytes.NewReader(copyOfData)), Size: int64(len(copyOfData))}, nil
}

type asyncMemorySourceStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newAsyncMemorySourceStore() *asyncMemorySourceStore {
	return &asyncMemorySourceStore{objects: make(map[string][]byte)}
}

func (store *asyncMemorySourceStore) Create(_ context.Context, key string, value []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.objects[key]; exists {
		return external.ErrSourceObjectExists
	}
	store.objects[key] = append([]byte(nil), value...)
	return nil
}

func (store *asyncMemorySourceStore) Get(_ context.Context, key string, maximumBytes int64) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, exists := store.objects[key]
	if !exists {
		return nil, errors.New("source object not found")
	}
	if int64(len(value)) > maximumBytes {
		return nil, external.ErrSourceEncryption
	}
	return append([]byte(nil), value...), nil
}

func (store *asyncMemorySourceStore) Delete(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.objects, key)
	return nil
}

func canonicalAsyncRESTCapabilities() httpapi.Capabilities {
	languages := make([]httpapi.LanguageCapability, 0, len(judgecontract.CanonicalLanguages()))
	for _, language := range judgecontract.CanonicalLanguages() {
		languages = append(languages, httpapi.LanguageCapability{ID: language.PublicID, DisplayName: language.DisplayName, Runtime: language.Runtime})
	}
	checkers := make([]string, 0, len(judgecontract.CanonicalCheckers()))
	for _, checker := range judgecontract.CanonicalCheckers() {
		checkers = append(checkers, string(checker))
	}
	return httpapi.Capabilities{
		APIVersion: "v1", Languages: languages, JudgeModes: []string{"ACM"}, Checkers: checkers,
		Limits: httpapi.CapabilityLimits{
			MaxSourceBytes: 1 << 20, MaxBundleBytes: 64 << 20, MaxCaseBytes: 64 << 20,
			MaxCaseCount: 256, MaxTimeLimitMillis: 10_000, MaxMemoryLimitMiB: 1024,
		},
	}
}

func reflectCanonicalCapabilities(capabilities httpapi.Capabilities) bool {
	languages := judgecontract.CanonicalLanguages()
	if len(capabilities.Languages) != len(languages) {
		return false
	}
	for index, language := range languages {
		if capabilities.Languages[index] != (httpapi.LanguageCapability{ID: language.PublicID, DisplayName: language.DisplayName, Runtime: language.Runtime}) {
			return false
		}
	}
	checkers := judgecontract.CanonicalCheckers()
	if len(capabilities.Checkers) != len(checkers) {
		return false
	}
	for index, checker := range checkers {
		if capabilities.Checkers[index] != string(checker) {
			return false
		}
	}
	return true
}

func asyncRESTBundleArchive(t *testing.T) []byte {
	t.Helper()
	manifest := bundle.Manifest{
		SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact,
		Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		Cases:  []bundle.Case{{ID: "case-01", Input: "cases/01.in", Output: "cases/01.out", Weight: 1}},
	}
	var archive bytes.Buffer
	if _, err := bundle.WriteDeterministicArchive(&archive, manifest, map[string][]byte{
		"cases/01.in": []byte("hidden input"), "cases/01.out": []byte("hidden output\n"),
	}); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func asyncRESTBundleUpload(t *testing.T, client *http.Client, serverURL, apiKey, idempotencyKey string, archive []byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("bundle", "hidden.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, serverURL+"/api/v1/bundles", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, responseBody
}

func asyncRESTJSON(t *testing.T, client *http.Client, method, url, apiKey, idempotencyKey string, body any, wantStatus int, output any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s status=%d body=%s", method, url, response.StatusCode, responseBody)
	}
	if output != nil {
		if err := json.Unmarshal(responseBody, output); err != nil {
			t.Fatalf("decode %s %s response: %v body=%s", method, url, err, responseBody)
		}
	}
}

func resetAsyncRESTDatabase(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, table := range []string{
		"t_external_retention_audit", "t_external_execution_daily", "t_external_webhook_outbox", "t_external_job_attempt", "t_external_idempotency",
		"t_external_job", "t_external_source_reservation", "t_external_source_object", "t_external_callback", "t_external_bundle",
		"t_external_api_key", "t_external_tenant", "t_judge_schema_history",
	} {
		if _, err := database.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if err := external.ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
}
