package external

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySourceStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	puts    int
	deletes int
}

func newMemorySourceStore() *memorySourceStore {
	return &memorySourceStore{objects: make(map[string][]byte)}
}

func (store *memorySourceStore) Create(_ context.Context, key string, value []byte) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if _, exists := store.objects[key]; exists {
		return ErrSourceObjectExists
	}
	store.puts++
	store.objects[key] = append([]byte(nil), value...)
	return nil
}

func (store *memorySourceStore) Delete(_ context.Context, key string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.deletes++
	delete(store.objects, key)
	return nil
}

func (store *memorySourceStore) Get(_ context.Context, key string) ([]byte, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	value, exists := store.objects[key]
	if !exists {
		return nil, errors.New("source object not found")
	}
	return append([]byte(nil), value...), nil
}

func (store *memorySourceStore) snapshot() (map[string][]byte, int, int) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	objects := make(map[string][]byte, len(store.objects))
	for key, value := range store.objects {
		objects[key] = append([]byte(nil), value...)
	}
	return objects, store.puts, store.deletes
}

func TestMySQLJobRepositoryAdmissionIsIdempotentEncryptedAndTenantOwned(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("a", 26)
	tenantB := strings.Repeat("b", 26)
	bundleA := strings.Repeat("c", 26)
	callbackA := strings.Repeat("d", 26)
	bundleB := strings.Repeat("e", 26)
	callbackB := strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, callbackA, 4)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, callbackB, 4)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	request := JudgeJobRequest{
		BundleID: bundleA, Language: "cpp20", SourceCode: []byte("int main(){return 0;}"),
		StopOnFailure: true, CallbackID: callbackA, ClientReference: "submission-42",
	}

	first, err := repository.Submit(context.Background(), tenantA, "job-submit-key-0001", request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Job.Status != JobStatusQueued || first.Job.ExternalID == "" || first.Job.TenantExternalID != tenantA {
		t.Fatalf("first submit = %+v", first)
	}
	replay, err := repository.Submit(context.Background(), tenantA, "job-submit-key-0001", request)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Job.ExternalID != first.Job.ExternalID {
		t.Fatalf("replay = %+v, first = %+v", replay, first)
	}
	conflicting := request
	conflicting.SourceCode = []byte("int main(){return 1;}")
	if _, err := repository.Submit(context.Background(), tenantA, "job-submit-key-0001", conflicting); !errors.Is(err, ErrExternalJobConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	crossTenant := request
	crossTenant.BundleID = bundleA
	crossTenant.CallbackID = ""
	if _, err := repository.Submit(context.Background(), tenantB, "job-submit-key-0002", crossTenant); !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("cross-tenant bundle error = %v", err)
	}
	crossTenantCallback := request
	crossTenantCallback.BundleID = bundleB
	crossTenantCallback.CallbackID = callbackA
	if _, err := repository.Submit(context.Background(), tenantB, "job-submit-key-0003", crossTenantCallback); !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("cross-tenant callback error = %v", err)
	}
	if _, err := database.Exec("UPDATE t_external_callback SET disabled_at = NOW(3) WHERE external_id = ?", callbackB); err != nil {
		t.Fatal(err)
	}
	disabledCallback := request
	disabledCallback.BundleID = bundleB
	disabledCallback.CallbackID = callbackB
	if _, err := repository.Submit(context.Background(), tenantB, "job-submit-key-0004", disabledCallback); !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("disabled callback error = %v", err)
	}

	objects, puts, deletes := store.snapshot()
	if puts != 1 || deletes != 0 || len(objects) != 1 {
		t.Fatalf("source lifecycle puts=%d deletes=%d objects=%d", puts, deletes, len(objects))
	}
	for key, ciphertext := range objects {
		if bytes.Contains(ciphertext, request.SourceCode) || strings.Contains(key, "submission-42") {
			t.Fatalf("source plaintext leaked via object key or payload")
		}
	}
	assertMySQLDoesNotContain(t, database, string(request.SourceCode))

	got, err := repository.Get(context.Background(), tenantA, first.Job.ExternalID)
	if err != nil || got.ExternalID != first.Job.ExternalID || got.Source.ObjectKey == "" {
		t.Fatalf("get = %+v, error = %v", got, err)
	}
	if _, err := repository.Get(context.Background(), tenantB, first.Job.ExternalID); !errors.Is(err, ErrExternalJobNotFound) {
		t.Fatalf("cross-tenant get error = %v", err)
	}
}

func TestMySQLJobRepositoryRejectsPendingBundleOwnership(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("6", 26)
	bundleID := strings.Repeat("7", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	if _, err := database.Exec("UPDATE t_external_bundle SET ready_at = NULL WHERE external_id = ?", bundleID); err != nil {
		t.Fatal(err)
	}
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	_, err := repository.Submit(context.Background(), tenantID, "pending-bundle-key", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}"),
	})
	if !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("pending bundle submit error = %v", err)
	}
	objects, puts, deletes := store.snapshot()
	if puts != 0 || deletes != 0 || len(objects) != 0 {
		t.Fatalf("pending bundle wrote source objects: puts=%d deletes=%d objects=%d", puts, deletes, len(objects))
	}
}

func TestMySQLJobRepositoryConcurrentReplayCreatesOneJobAndOneObject(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("g", 26)
	bundleID := strings.Repeat("h", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 8)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	request := JudgeJobRequest{BundleID: bundleID, Language: "go126", SourceCode: []byte("package main\nfunc main(){}")}

	const callers = 12
	results := make(chan SubmitJobResult, callers)
	errorsChannel := make(chan error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := repository.Submit(context.Background(), tenantID, "concurrent-job-key", request)
			results <- result
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent submit: %v", err)
		}
	}
	var jobID string
	for result := range results {
		if jobID == "" {
			jobID = result.Job.ExternalID
		}
		if result.Job.ExternalID != jobID {
			t.Fatalf("multiple logical jobs: %q and %q", jobID, result.Job.ExternalID)
		}
	}
	objects, puts, deletes := store.snapshot()
	if len(objects) != 1 || puts != 1 || deletes != 0 {
		t.Fatalf("source lifecycle puts=%d deletes=%d objects=%d", puts, deletes, len(objects))
	}
	var jobs int
	if err := database.QueryRow("SELECT COUNT(*) FROM t_external_job").Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d error=%v", jobs, err)
	}
}

func TestMySQLJobRepositoryQueuedQuotaFailsClosed(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("i", 26)
	bundleID := strings.Repeat("j", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 1)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	first := JudgeJobRequest{BundleID: bundleID, Language: "java17", SourceCode: []byte("class Main {}")}
	if _, err := repository.Submit(context.Background(), tenantID, "quota-job-key-0001", first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceCode = []byte("class Main { public static void main(String[] x){} }")
	if _, err := repository.Submit(context.Background(), tenantID, "quota-job-key-0002", second); !errors.Is(err, ErrQueuedQuotaExceeded) {
		t.Fatalf("quota error = %v", err)
	}
	if _, err := database.Exec("UPDATE t_external_tenant SET policy_json = JSON_OBJECT('maxQueuedJobs', NULL) WHERE external_id = ?", tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_job SET status = 'SUCCEEDED', completed_at = NOW(3) WHERE tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?)", tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Submit(context.Background(), tenantID, "quota-job-key-0003", second); !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("invalid policy did not fail closed: %v", err)
	}
	_, puts, _ := store.snapshot()
	if puts != 1 {
		t.Fatalf("quota failures wrote source objects: puts=%d", puts)
	}
}

func TestMySQLJobRepositoryReplaySurvivesPolicyTightening(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("e", 26)
	bundleID := strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 3)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}")}
	first, err := repository.Submit(context.Background(), tenantID, "policy-replay-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxSourceBytes', 1)
WHERE external_id = ?`, tenantID); err != nil {
		t.Fatal(err)
	}
	replay, err := repository.Submit(context.Background(), tenantID, "policy-replay-key", request)
	if err != nil || !replay.Replayed || replay.Job.ExternalID != first.Job.ExternalID {
		t.Fatalf("policy-tightened replay = %+v, error = %v", replay, err)
	}
	if _, err := repository.Submit(context.Background(), tenantID, "policy-new-key-001", request); !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("new oversized request error = %v", err)
	}
}

func TestMySQLJobRepositoryListUsesStableTenantBoundCursor(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("k", 26)
	tenantB := strings.Repeat("m", 26)
	bundleA := strings.Repeat("n", 26)
	bundleB := strings.Repeat("o", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, "", 10)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, "", 10)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	jobIDs := make([]string, 0, 4)
	for index := 0; index < 4; index++ {
		result, err := repository.Submit(context.Background(), tenantA, fmt.Sprintf("list-job-key-%04d", index), JudgeJobRequest{
			BundleID: bundleA, Language: "cpp20", SourceCode: []byte(fmt.Sprintf("int main(){return %d;}", index)),
		})
		if err != nil {
			t.Fatal(err)
		}
		jobIDs = append(jobIDs, result.Job.ExternalID)
	}
	if _, err := repository.Submit(context.Background(), tenantB, "list-other-tenant", JudgeJobRequest{
		BundleID: bundleB, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_job SET created_at = '2026-07-19 10:20:30.123'"); err != nil {
		t.Fatal(err)
	}

	first, err := repository.List(context.Background(), tenantA, JobListOptions{Limit: 2, Status: JobStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Jobs) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := repository.List(context.Background(), tenantA, JobListOptions{Limit: 2, Status: JobStatusQueued, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Jobs) != 2 || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	seen := make(map[string]bool)
	for _, page := range [][]ExternalJobRecord{first.Jobs, second.Jobs} {
		for _, job := range page {
			if job.TenantExternalID != tenantA || seen[job.ExternalID] {
				t.Fatalf("tenant leak or duplicate job: %+v", job)
			}
			seen[job.ExternalID] = true
		}
	}
	if len(seen) != len(jobIDs) {
		t.Fatalf("seen=%d want=%d", len(seen), len(jobIDs))
	}
	if _, err := repository.List(context.Background(), tenantB, JobListOptions{Limit: 2, Status: JobStatusQueued, Cursor: first.NextCursor}); !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("cross-tenant cursor error = %v", err)
	}
}

func TestMySQLJobRepositoryCancellationIsTenantSafeAndIdempotent(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("p", 26)
	tenantB := strings.Repeat("q", 26)
	bundleA := strings.Repeat("r", 26)
	bundleB := strings.Repeat("s", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, "", 4)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, "", 4)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	result, err := repository.Submit(context.Background(), tenantA, "cancel-job-key-0001", JudgeJobRequest{
		BundleID: bundleA, Language: "go126", SourceCode: []byte("package main\nfunc main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cancel(context.Background(), tenantB, result.Job.ExternalID); !errors.Is(err, ErrExternalJobNotFound) {
		t.Fatalf("cross-tenant cancel error = %v", err)
	}
	cancelled, err := repository.Cancel(context.Background(), tenantA, result.Job.ExternalID)
	if err != nil || cancelled.Status != JobStatusCancelled || cancelled.CompletedAt == nil || cancelled.CancelRequested == nil {
		t.Fatalf("cancelled = %+v, error = %v", cancelled, err)
	}
	replayed, err := repository.Cancel(context.Background(), tenantA, result.Job.ExternalID)
	if err != nil || replayed.Status != JobStatusCancelled || !replayed.CompletedAt.Equal(*cancelled.CompletedAt) {
		t.Fatalf("replayed cancellation = %+v, error = %v", replayed, err)
	}
}

func TestMySQLJobRepositoryCompensatesSourceObjectOnDatabaseFailure(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("t", 26)
	bundleID := strings.Repeat("u", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	if _, err := database.Exec(`
ALTER TABLE t_external_source_object
ADD CONSTRAINT chk_test_reject_source CHECK (external_id <> external_id)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec("ALTER TABLE t_external_source_object DROP CHECK chk_test_reject_source")
	})
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	_, err := repository.Submit(context.Background(), tenantID, "compensate-job-key", JudgeJobRequest{
		BundleID: bundleID, Language: "python313", SourceCode: []byte("print(42)"),
	})
	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("submit error = %v", err)
	}
	objects, puts, deletes := store.snapshot()
	if puts != 1 || deletes != 1 || len(objects) != 0 {
		t.Fatalf("source compensation puts=%d deletes=%d objects=%d", puts, deletes, len(objects))
	}
	if count := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job"); count != 0 {
		t.Fatalf("jobs after rollback = %d", count)
	}
}

func newTestMySQLJobRepository(t *testing.T, database *sql.DB, store SourceObjectStore) *MySQLJobRepository {
	t.Helper()
	cipher, err := NewSourceCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{0x42}, 32)}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewMySQLJobRepository(MySQLJobRepositoryConfig{
		Database: database, Random: rand.Reader, Now: time.Now,
		IdempotencyPepper: bytes.Repeat([]byte{0x51}, 32),
		CursorKey:         bytes.Repeat([]byte{0x52}, 32),
		SourceCipher:      cipher, SourceObjects: store,
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func prepareExternalJobDatabase(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"t_external_webhook_outbox", "t_external_job_attempt", "t_external_idempotency",
		"t_external_job", "t_external_source_object", "t_external_callback", "t_external_bundle",
		"t_external_api_key", "t_external_tenant",
	} {
		if _, err := database.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

func insertTenantBundleAndCallback(t *testing.T, database *sql.DB, tenantID, bundleID, callbackID string, maxQueued int) {
	t.Helper()
	policy := TenantPolicy{
		MaxQueuedJobs: maxQueued, MaxRunningJobs: 1, MaxSourceBytes: 1 << 20,
		MaxRetainedBundles: 100, DailyExecutionMillis: 3_600_000, MaxInfrastructureTries: 3,
	}
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	result, err := database.Exec("INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES (?, ?, 'ACTIVE', ?)", tenantID, "tenant-"+tenantID[:4], encodedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	tenantInternalID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
INSERT INTO t_external_bundle(external_id, tenant_id, sha256, object_key, size_bytes, case_count, manifest_version, manifest_json, ready_at)
VALUES (?, ?, UNHEX(SHA2(?, 256)), ?, 128, 1, 1, JSON_OBJECT('schemaVersion', 1, 'cases', JSON_ARRAY()), NOW(3))`,
		bundleID, tenantInternalID, bundleID, "external/"+tenantID+"/sha256/"+bundleID+".zip"); err != nil {
		t.Fatal(err)
	}
	if callbackID != "" {
		if _, err := database.Exec(`
INSERT INTO t_external_callback(external_id, tenant_id, destination_url, allowed_host, allowed_port, secret_ciphertext, secret_key_version)
VALUES (?, ?, 'https://callback.example.test/judge', 'callback.example.test', 443, X'0102', 1)`, callbackID, tenantInternalID); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMySQLDoesNotContain(t *testing.T, database *sql.DB, plaintext string) {
	t.Helper()
	var matches int
	query := `
SELECT COUNT(*) FROM (
    SELECT object_key AS value FROM t_external_source_object
    UNION ALL SELECT client_reference FROM t_external_job
    UNION ALL SELECT CAST(response_json AS CHAR) FROM t_external_idempotency
) AS persisted WHERE INSTR(COALESCE(value, ''), ?) > 0`
	if err := database.QueryRow(query, plaintext).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 0 {
		t.Fatalf("source plaintext was found in MySQL rows")
	}
}

func mustCount(t *testing.T, database *sql.DB, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := database.QueryRow(query, arguments...).Scan(&count); err != nil {
		t.Fatal(fmt.Errorf("count query: %w", err))
	}
	return count
}
