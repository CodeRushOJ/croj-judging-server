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
	"sync/atomic"
	"testing"
	"time"
)

type memorySourceStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	puts    int
	deletes int
}

type blockingSourceStore struct {
	*memorySourceStore
	createStarted chan struct{}
	deleteStarted chan struct{}
	releaseCreate chan struct{}
	releaseDelete chan struct{}
}

func (store *blockingSourceStore) Create(ctx context.Context, key string, value []byte) error {
	if store.createStarted != nil {
		select {
		case <-store.createStarted:
		default:
			close(store.createStarted)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-store.releaseCreate:
		}
	}
	return store.memorySourceStore.Create(ctx, key, value)
}

func (store *blockingSourceStore) Delete(ctx context.Context, key string) error {
	if store.deleteStarted != nil {
		select {
		case <-store.deleteStarted:
		default:
			close(store.deleteStarted)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-store.releaseDelete:
		}
	}
	return store.memorySourceStore.Delete(ctx, key)
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

func (store *memorySourceStore) Get(_ context.Context, key string, maximumBytes int64) ([]byte, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	value, exists := store.objects[key]
	if !exists {
		return nil, errors.New("source object not found")
	}
	if int64(len(value)) > maximumBytes {
		return nil, ErrSourceEncryption
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
		BundleID: bundleA, Language: "cpp", SourceCode: []byte("int main(){return 0;}"),
		StopOnFailure: true, CallbackID: callbackA, ClientReference: "submission-42",
	}

	first, err := repository.Submit(context.Background(), tenantA, "job-submit-key-0001", request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Job.Status != JobStatusQueued || first.Job.ExternalID == "" || first.Job.TenantExternalID != tenantA {
		t.Fatalf("first submit = %+v", first)
	}
	replay, err := repository.MySQLJobRepository.Submit(context.Background(), tenantA, "job-submit-key-0001", request, func(context.Context) error {
		return errors.New("quota unavailable")
	})
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
	if _, err := database.Exec("UPDATE t_external_bundle SET publication_status = 'PENDING', ready_at = NULL WHERE external_id = ?", bundleID); err != nil {
		t.Fatal(err)
	}
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	_, err := repository.Submit(context.Background(), tenantID, "pending-bundle-key", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
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
	var admissionCalls atomic.Int64
	results := make(chan SubmitJobResult, callers)
	errorsChannel := make(chan error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := repository.MySQLJobRepository.Submit(context.Background(), tenantID, "concurrent-job-key", request, func(context.Context) error {
				admissionCalls.Add(1)
				return nil
			})
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
	if admissionCalls.Load() != 1 {
		t.Fatalf("concurrent idempotent admission calls=%d want=1", admissionCalls.Load())
	}
	var jobs int
	if err := database.QueryRow("SELECT COUNT(*) FROM t_external_job").Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d error=%v", jobs, err)
	}
}

func TestMySQLJobRepositoryContendedIdempotencyReservationFailsFastWithoutAdmission(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("k", 26)
	bundleID := strings.Repeat("m", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	idempotencyKey := "contended-coordination-key"
	keyDigest, err := DigestIdempotencyKey(idempotencyKey, repository.idempotencyPepper)
	if err != nil {
		t.Fatal(err)
	}
	coordinationKey, err := SourceObjectKey(tenantID, submissionSourceExternalID(tenantID, keyDigest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
INSERT INTO t_external_source_reservation(object_key, owner_token, lease_until)
VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) + INTERVAL 25 MINUTE)`, coordinationKey); err != nil {
		t.Fatal(err)
	}
	var admissionCalls atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err = repository.MySQLJobRepository.Submit(ctx, tenantID, idempotencyKey, JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	}, func(context.Context) error {
		admissionCalls.Add(1)
		return nil
	})
	if elapsed := time.Since(started); !errors.Is(err, ErrExternalJobUnavailable) || elapsed > 2*time.Second {
		t.Fatalf("contended idempotency error=%v elapsed=%s", err, elapsed)
	}
	if admissionCalls.Load() != 0 {
		t.Fatalf("contended idempotency charged admission %d times", admissionCalls.Load())
	}
}

func TestMySQLJobRepositoryContendedIdempotencyReservationObservesCommittedReplay(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("n", 26)
	bundleID := strings.Repeat("p", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")}
	seed, err := repository.Submit(context.Background(), tenantID, "coordination-replay-seed", request)
	if err != nil {
		t.Fatal(err)
	}
	targetKey := "coordination-replay-target"
	targetDigest, err := DigestIdempotencyKey(targetKey, repository.idempotencyPepper)
	if err != nil {
		t.Fatal(err)
	}
	coordinationKey, err := SourceObjectKey(tenantID, submissionSourceExternalID(tenantID, targetDigest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
INSERT INTO t_external_source_reservation(object_key, owner_token, lease_until)
VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) + INTERVAL 25 MINUTE)`, coordinationKey); err != nil {
		t.Fatal(err)
	}
	var admissionCalls atomic.Int64
	type submitOutcome struct {
		result SubmitJobResult
		err    error
	}
	outcome := make(chan submitOutcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		result, err := repository.MySQLJobRepository.Submit(ctx, tenantID, targetKey, request, func(context.Context) error {
			admissionCalls.Add(1)
			return nil
		})
		outcome <- submitOutcome{result: result, err: err}
	}()
	select {
	case early := <-outcome:
		t.Fatalf("contended submit returned before predecessor commit: result=%+v error=%v", early.result, early.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := database.Exec(`
UPDATE t_external_idempotency SET key_digest = ?
WHERE operation_scope = ? AND resource_external_id = ?`, targetDigest, submitJobIdempotencyScope, seed.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-outcome:
		if completed.err != nil || !completed.result.Replayed || completed.result.Job.ExternalID != seed.Job.ExternalID {
			t.Fatalf("committed replay result=%+v error=%v", completed.result, completed.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contended submit did not observe committed replay")
	}
	if admissionCalls.Load() != 0 {
		t.Fatalf("committed replay charged admission %d times", admissionCalls.Load())
	}
}

func TestMySQLJobRepositoryExpiredCoordinationReservationUsesRequestBoundedLease(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("q", 26)
	bundleID := strings.Repeat("r", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")}
	keyDigest, err := DigestIdempotencyKey("expired-coordination-key", repository.idempotencyPepper)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := CanonicalJobRequestHash(request, int64(len(request.SourceCode)))
	if err != nil {
		t.Fatal(err)
	}
	coordinationKey, err := SourceObjectKey(tenantID, submissionSourceExternalID(tenantID, keyDigest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
INSERT INTO t_external_source_reservation(object_key, owner_token, lease_until, created_at)
VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND, CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR)`, coordinationKey); err != nil {
		t.Fatal(err)
	}
	ownerToken := bytes.Repeat([]byte{0x7a}, 32)
	if _, acquired, err := repository.acquireSubmissionCoordination(
		context.Background(), tenantID, keyDigest, requestHash, coordinationKey, ownerToken,
	); err != nil || !acquired {
		t.Fatalf("expired coordination takeover acquired=%v error=%v", acquired, err)
	}
	var leaseSeconds int
	if err := database.QueryRow(`
SELECT TIMESTAMPDIFF(SECOND, CURRENT_TIMESTAMP(3), lease_until)
FROM t_external_source_reservation WHERE object_key = ? AND owner_token = ?`, coordinationKey, ownerToken).Scan(&leaseSeconds); err != nil {
		t.Fatal(err)
	}
	if leaseSeconds < 180 || leaseSeconds > 300 {
		t.Fatalf("coordination lease=%ds, want request-bounded default near four minutes", leaseSeconds)
	}
}

func TestMySQLJobRepositoryCoordinationLeaseEndsAfterRequestAndCompensation(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("q", 26)
	bundleID := strings.Repeat("r", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")}
	keyDigest, err := DigestIdempotencyKey("deadline-coordination-key", repository.idempotencyPepper)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := CanonicalJobRequestHash(request, int64(len(request.SourceCode)))
	if err != nil {
		t.Fatal(err)
	}
	coordinationKey, err := SourceObjectKey(tenantID, submissionSourceExternalID(tenantID, keyDigest))
	if err != nil {
		t.Fatal(err)
	}
	ownerToken := bytes.Repeat([]byte{0x6a}, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, acquired, err := repository.acquireSubmissionCoordination(
		ctx, tenantID, keyDigest, requestHash, coordinationKey, ownerToken,
	); err != nil || !acquired {
		t.Fatalf("coordination acquired=%v error=%v", acquired, err)
	}
	var leaseMillis int64
	if err := database.QueryRow(`
SELECT TIMESTAMPDIFF(MICROSECOND, CURRENT_TIMESTAMP(3), lease_until) DIV 1000
FROM t_external_source_reservation WHERE object_key = ? AND owner_token = ?`, coordinationKey, ownerToken).Scan(&leaseMillis); err != nil {
		t.Fatal(err)
	}
	if leaseMillis < 50_000 || leaseMillis > 70_000 {
		t.Fatalf("coordination lease=%dms, want request remainder plus bounded compensation allowance", leaseMillis)
	}
}

func TestSubmissionCoordinationLeaseUsesRemainingRequestTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease := submissionCoordinationLease(ctx, 4*time.Minute)
	if lease < 61*time.Second || lease > 63*time.Second {
		t.Fatalf("coordination lease=%s, want request remainder plus one-minute compensation allowance", lease)
	}
}

func TestSubmissionOperationContextBoundsBackgroundCaller(t *testing.T) {
	ctx, cancel := submissionOperationContext(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("background submission did not receive an owned operation deadline")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("submission context error=%v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("owned submission deadline did not expire")
	}
}

func TestMySQLJobRepositoryBackgroundSubmitBoundsBlockingAdmission(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("q", 26)
	bundleID := strings.Repeat("r", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	repository.submitOperationTimeout = 40 * time.Millisecond
	admissionCalls := 0
	started := time.Now()

	_, err := repository.MySQLJobRepository.Submit(context.Background(), tenantID, "background-admission-key", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	}, func(ctx context.Context) error {
		admissionCalls++
		<-ctx.Done()
		return ctx.Err()
	})

	if !errors.Is(err, context.DeadlineExceeded) || admissionCalls != 1 {
		t.Fatalf("submit error=%v admissionCalls=%d", err, admissionCalls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("background submit exceeded owned deadline: %s", elapsed)
	}
	if jobs := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job"); jobs != 0 {
		t.Fatalf("jobs after timed-out admission=%d", jobs)
	}
}

func TestLockSubmissionReservationsRejectsExpiredLease(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	firstKey := "external/" + strings.Repeat("a", 26) + "/source/" + strings.Repeat("b", 26)
	secondKey := "external/" + strings.Repeat("a", 26) + "/source/" + strings.Repeat("c", 26)
	firstToken := bytes.Repeat([]byte{0x21}, 32)
	secondToken := bytes.Repeat([]byte{0x22}, 32)
	if _, err := database.Exec(`
INSERT INTO t_external_source_reservation(object_key, owner_token, lease_until)
VALUES (?, ?, CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND),
       (?, ?, CURRENT_TIMESTAMP(3) + INTERVAL 1 MINUTE)`,
		firstKey, firstToken, secondKey, secondToken); err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSubmissionReservations(context.Background(), tx,
		sourceReservationClaim{objectKey: firstKey, ownerToken: firstToken},
		sourceReservationClaim{objectKey: secondKey, ownerToken: secondToken},
	); err == nil {
		t.Fatal("expired coordination lease remained eligible to publish")
	}
}

func TestMySQLJobRepositoryAdmissionWorksWithSingleDatabaseConnection(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("6", 26)
	bundleID := strings.Repeat("7", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := repository.Submit(ctx, tenantID, "single-connection-admission", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	})
	if err != nil || result.Job.Status != JobStatusQueued {
		t.Fatalf("single-connection admission result=%+v error=%v", result, err)
	}
}

func TestMySQLJobRepositoryRunsAdmissionAndSourceUploadWithoutDatabaseLocks(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	tenantID := strings.Repeat("6", 26)
	bundleID := strings.Repeat("7", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := &blockingSourceStore{
		memorySourceStore: newMemorySourceStore(), createStarted: make(chan struct{}), releaseCreate: make(chan struct{}),
	}
	repository := newTestMySQLJobRepository(t, database, store)

	admissionCalled := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := repository.MySQLJobRepository.Submit(context.Background(), tenantID, "lock-free-submit-key", JudgeJobRequest{
			BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
		}, func(context.Context) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := database.ExecContext(ctx, "UPDATE t_external_tenant SET status = status WHERE external_id = ?", tenantID); err != nil {
				return fmt.Errorf("admission ran while tenant lock was held: %w", err)
			}
			close(admissionCalled)
			return nil
		})
		result <- err
	}()
	select {
	case <-admissionCalled:
	case err := <-result:
		t.Fatalf("submit failed before admission completed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("admission remained blocked behind the submit transaction")
	}
	select {
	case <-store.createStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("source upload did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, "UPDATE t_external_tenant SET updated_at = updated_at WHERE external_id = ?", tenantID); err != nil {
		t.Fatalf("source upload retained tenant database lock: %v", err)
	}
	close(store.releaseCreate)
	if err := <-result; err != nil {
		t.Fatal(err)
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
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")}
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

func TestMySQLJobRepositoryCanReuseIdempotencyKeyAfterRetentionExpiry(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("e", 26)
	bundleID := strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 3)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	request := JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")}
	first, err := repository.Submit(context.Background(), tenantID, "reusable-submit-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("DELETE FROM t_external_idempotency WHERE operation_scope = ?", submitJobIdempotencyScope); err != nil {
		t.Fatal(err)
	}
	second, err := repository.Submit(context.Background(), tenantID, "reusable-submit-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Replayed || second.Job.ExternalID == first.Job.ExternalID || second.Job.Source.ObjectKey == first.Job.Source.ObjectKey {
		t.Fatalf("expired key did not create an independent job: first=%+v second=%+v", first, second)
	}
	objects, puts, deletes := store.snapshot()
	if len(objects) != 2 || puts != 2 || deletes != 0 {
		t.Fatalf("source lifecycle after key reuse: objects=%d puts=%d deletes=%d", len(objects), puts, deletes)
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
			BundleID: bundleA, Language: "cpp", SourceCode: []byte(fmt.Sprintf("int main(){return %d;}", index)),
		})
		if err != nil {
			t.Fatal(err)
		}
		jobIDs = append(jobIDs, result.Job.ExternalID)
	}
	if _, err := repository.Submit(context.Background(), tenantB, "list-other-tenant", JudgeJobRequest{
		BundleID: bundleB, Language: "cpp", SourceCode: []byte("int main(){}"),
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
	if _, err := repository.List(context.Background(), tenantB, JobListOptions{Limit: 2, Status: JobStatusQueued, Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidJobCursor) {
		t.Fatalf("cross-tenant cursor error = %v", err)
	}
}

func TestMySQLJobRepositorySweepsOnlyUnreferencedSourceReservations(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("3", 26)
	bundleID := strings.Repeat("4", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	accepted, err := repository.Submit(context.Background(), tenantID, "reservation-linked-key", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	linkedKey := accepted.Job.Source.ObjectKey
	orphanID := strings.Repeat("5", 26)
	orphanKey, err := SourceObjectKey(tenantID, orphanID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), orphanKey, []byte("opaque ciphertext")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO t_external_source_reservation(object_key, owner_token, lease_until, created_at)
VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR, CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR),
       (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR, CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR)`, orphanKey, linkedKey); err != nil {
		t.Fatal(err)
	}
	reaped, err := repository.SweepSourceReservations(context.Background(), time.Minute, 10)
	if err != nil || reaped != 2 {
		t.Fatalf("sweep reaped=%d error=%v", reaped, err)
	}
	objects, _, _ := store.snapshot()
	if _, exists := objects[orphanKey]; exists {
		t.Fatal("orphaned source object survived reservation sweep")
	}
	if _, exists := objects[linkedKey]; !exists {
		t.Fatal("referenced source object was deleted by reservation sweep")
	}
	if count := mustCount(t, database, "SELECT COUNT(*) FROM t_external_source_reservation"); count != 0 {
		t.Fatalf("source reservations after sweep=%d", count)
	}
}

func TestSourceReservationSweepSkipsAdmissionLockedObject(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	tenantID := strings.Repeat("2", 26)
	sourceID := strings.Repeat("3", 26)
	objectKey, err := SourceObjectKey(tenantID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), objectKey, []byte("opaque ciphertext")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO t_external_source_reservation(
object_key, owner_token, lease_until, created_at
) VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR, CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR)`, objectKey); err != nil {
		t.Fatal(err)
	}
	admission, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Exec("SELECT object_key FROM t_external_source_reservation WHERE object_key = ? FOR UPDATE", objectKey); err != nil {
		_ = admission.Rollback()
		t.Fatal(err)
	}
	reaped, err := repository.SweepSourceReservations(context.Background(), time.Minute, 1)
	if err != nil || reaped != 0 {
		_ = admission.Rollback()
		t.Fatalf("locked reservation sweep reaped=%d error=%v", reaped, err)
	}
	if err := admission.Rollback(); err != nil {
		t.Fatal(err)
	}
	if reaped, err = repository.SweepSourceReservations(context.Background(), time.Minute, 1); err != nil || reaped != 1 {
		t.Fatalf("unlocked reservation sweep reaped=%d error=%v", reaped, err)
	}
}

func TestSourceReservationSweepDeletesOutsideReservationTransaction(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	store := &blockingSourceStore{
		memorySourceStore: newMemorySourceStore(), deleteStarted: make(chan struct{}), releaseDelete: make(chan struct{}),
	}
	repository := newTestMySQLJobRepository(t, database, store)
	tenantID := strings.Repeat("2", 26)
	sourceID := strings.Repeat("4", 26)
	objectKey, err := SourceObjectKey(tenantID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), objectKey, []byte("opaque ciphertext")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO t_external_source_reservation(
object_key, owner_token, lease_until, created_at
) VALUES (?, RANDOM_BYTES(32), CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR, CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR)`, objectKey); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repository.SweepSourceReservations(context.Background(), time.Minute, 1)
		done <- err
	}()
	select {
	case <-store.deleteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reservation deletion did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT object_key FROM t_external_source_reservation WHERE object_key = ? FOR UPDATE", objectKey).Scan(new(string)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("object deletion retained reservation row lock: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	close(store.releaseDelete)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type mysqlNumberedError struct{ Number uint16 }

func (err *mysqlNumberedError) Error() string { return fmt.Sprintf("mysql error %d", err.Number) }

func TestRepositoryUnavailablePreservesUnderlyingErrorChain(t *testing.T) {
	root := &mysqlNumberedError{Number: 1213}
	err := repositoryUnavailable("lock tenant", root)
	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("availability classification lost: %v", err)
	}
	var recovered *mysqlNumberedError
	if !errors.As(err, &recovered) || recovered != root {
		t.Fatalf("database error chain lost: %v", err)
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

func TestMySQLJobRepositoryBoundsSourceObjectCreate(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("v", 26)
	bundleID := strings.Repeat("w", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := &blockingSourceStore{
		memorySourceStore: newMemorySourceStore(),
		createStarted:     make(chan struct{}),
		releaseCreate:     make(chan struct{}),
	}
	repository := newTestMySQLJobRepository(t, database, store)
	repository.sourceObjectOperationTimeout = 50 * time.Millisecond
	started := time.Now()
	_, err := repository.Submit(context.Background(), tenantID, "bounded-source-create", JudgeJobRequest{
		BundleID: bundleID, Language: "go", SourceCode: []byte("package main\nfunc main(){}"),
	})
	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("submit error=%v want source storage unavailable", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("source create was not bounded: %s", elapsed)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_source_reservation"); got != 2 {
		t.Fatalf("ambiguous create reservations=%d want=2", got)
	}
}

func TestMySQLJobRepositoryOwnedDeadlineBoundsSourceObjectCreate(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("v", 26)
	bundleID := strings.Repeat("w", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := &blockingSourceStore{
		memorySourceStore: newMemorySourceStore(),
		createStarted:     make(chan struct{}),
		releaseCreate:     make(chan struct{}),
	}
	repository := newTestMySQLJobRepository(t, database, store)
	repository.submitOperationTimeout = 50 * time.Millisecond
	repository.sourceObjectOperationTimeout = 2 * time.Minute
	started := time.Now()

	_, err := repository.Submit(context.Background(), tenantID, "owned-deadline-source", JudgeJobRequest{
		BundleID: bundleID, Language: "go", SourceCode: []byte("package main\nfunc main(){}"),
	})

	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("submit error=%v want source storage unavailable", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("owned submit deadline did not cap source create: %s", elapsed)
	}
}

func TestMySQLJobRepositoryOwnedDeadlineBoundsFinalDatabaseLockAndCompensates(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("x", 26)
	bundleID := strings.Repeat("y", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	blocker, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow("SELECT id FROM t_external_tenant WHERE external_id = ? FOR UPDATE", tenantID).Scan(new(uint64)); err != nil {
		_ = blocker.Rollback()
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	repository.submitOperationTimeout = 50 * time.Millisecond
	started := time.Now()

	_, err = repository.Submit(context.Background(), tenantID, "owned-deadline-database", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	})

	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("submit error=%v want repository unavailable", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("owned submit deadline did not cap final database lock: %s", elapsed)
	}
	objects, puts, deletes := store.snapshot()
	if len(objects) != 0 || puts != 1 || deletes != 1 {
		t.Fatalf("source compensation objects=%d puts=%d deletes=%d", len(objects), puts, deletes)
	}
	if reservations := mustCount(t, database, "SELECT COUNT(*) FROM t_external_source_reservation"); reservations != 2 {
		t.Fatalf("fenced reservations after database timeout=%d want=2 for reconciliation", reservations)
	}
	if jobs := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job"); jobs != 0 {
		t.Fatalf("jobs after database timeout=%d", jobs)
	}
}

type testMySQLJobRepository struct{ *MySQLJobRepository }

func (repository *testMySQLJobRepository) Submit(ctx context.Context, tenantID, idempotencyKey string, request JudgeJobRequest) (SubmitJobResult, error) {
	return repository.MySQLJobRepository.Submit(ctx, tenantID, idempotencyKey, request, func(context.Context) error { return nil })
}

func newTestMySQLJobRepository(t *testing.T, database *sql.DB, store SourceObjectStore) *testMySQLJobRepository {
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
	return &testMySQLJobRepository{MySQLJobRepository: repository}
}

func prepareExternalJobDatabase(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"t_external_retention_audit", "t_external_execution_daily", "t_external_webhook_outbox", "t_external_job_attempt", "t_external_idempotency",
		"t_external_job", "t_external_source_reservation", "t_external_source_object", "t_external_callback", "t_external_bundle",
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
		MaxTimeLimitMillis: 10_000, MaxMemoryLimitMiB: 1024,
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
INSERT INTO t_external_bundle(external_id, tenant_id, sha256, object_key, size_bytes, case_count, manifest_version, manifest_json, publication_status, ready_at)
VALUES (?, ?, UNHEX(SHA2(?, 256)), ?, 128, 1, 1,
  JSON_OBJECT(
    'schemaVersion', 1, 'judgeMode', 'ACM', 'checker', 'exact',
    'limits', JSON_OBJECT('timeLimitMillis', 1000, 'memoryLimitMiB', 256),
    'cases', JSON_ARRAY(JSON_OBJECT('id', 'case-1', 'input', '1.in', 'output', '1.out', 'weight', 1))
  ), 'READY', NOW(3))`,
		bundleID, tenantInternalID, bundleID, "external/"+tenantID+"/sha256/"+bundleID+".zip"); err != nil {
		t.Fatal(err)
	}
	if callbackID != "" {
		if _, err := database.Exec(`
INSERT INTO t_external_callback(
    external_id, tenant_id, destination_url, allowed_host, allowed_port,
    secret_ciphertext, secret_nonce, secret_key_version
)
VALUES (
    ?, ?, 'https://callback.example.test/judge', 'callback.example.test', 443,
    X'0102030405060708090A0B0C0D0E0F1011', X'000102030405060708090A0B', 1
)`, callbackID, tenantInternalID); err != nil {
			t.Fatal(err)
		}
	}
}

func insertBundleForTenant(t *testing.T, database *sql.DB, tenantID, bundleID string, timeLimitMillis, memoryLimitMiB int) {
	t.Helper()
	if _, err := database.Exec(`
INSERT INTO t_external_bundle(external_id, tenant_id, sha256, object_key, size_bytes, case_count, manifest_version, manifest_json, publication_status, ready_at)
SELECT ?, tenant.id, UNHEX(SHA2(?, 256)), ?, 128, 1, 1,
  JSON_OBJECT(
    'schemaVersion', 1, 'judgeMode', 'ACM', 'checker', 'exact',
    'limits', JSON_OBJECT('timeLimitMillis', ?, 'memoryLimitMiB', ?),
    'cases', JSON_ARRAY(JSON_OBJECT('id', 'case-1', 'input', '1.in', 'output', '1.out', 'weight', 1))
  ), 'READY', NOW(3)
FROM t_external_tenant AS tenant WHERE tenant.external_id = ?`,
		bundleID, bundleID, "external/"+tenantID+"/sha256/"+bundleID+".zip", timeLimitMillis, memoryLimitMiB, tenantID); err != nil {
		t.Fatal(err)
	}
}

func assertMySQLDoesNotContain(t *testing.T, database *sql.DB, plaintext string) {
	t.Helper()
	var matches int
	query := `
SELECT COUNT(*) FROM (
    SELECT CONVERT(object_key USING utf8mb4) AS value FROM t_external_source_object
    UNION ALL SELECT CONVERT(client_reference USING utf8mb4) FROM t_external_job
    UNION ALL SELECT CONVERT(CAST(response_json AS CHAR) USING utf8mb4) FROM t_external_idempotency
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
