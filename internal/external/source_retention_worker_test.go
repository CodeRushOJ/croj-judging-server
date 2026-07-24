package external

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

type flakyRetentionStore struct {
	*memorySourceStore
	failures int
}

type transientRetentionRepository struct {
	calls   int
	retried chan struct{}
}

func TestTransientDatabaseErrorClassificationUsesMySQLCodesThroughWrapping(t *testing.T) {
	for _, number := range []uint16{1205, 1213} {
		err := repositoryUnavailable("repository operation", &mysqlDriver.MySQLError{Number: number, Message: "retry"})
		if !IsTransientDatabaseError(err) {
			t.Fatalf("MySQL %d was not classified transient", number)
		}
	}
	for _, err := range []error{
		&mysqlDriver.MySQLError{Number: 1062, Message: "duplicate"},
		ErrExternalJobUnavailable,
		errors.New("invariant violated"),
	} {
		if IsTransientDatabaseError(err) {
			t.Fatalf("permanent error classified transient: %v", err)
		}
	}
}

func (repository *transientRetentionRepository) ClaimSourceRetention(context.Context, time.Duration, time.Duration) (SourceRetentionClaim, error) {
	repository.calls++
	if repository.calls == 1 {
		return SourceRetentionClaim{}, fmt.Errorf("injected transient retention error: %w", &mysqlDriver.MySQLError{Number: 1213, Message: "deadlock"})
	}
	select {
	case <-repository.retried:
	default:
		close(repository.retried)
	}
	return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
}

type failingRetentionRepository struct{ err error }

func (repository failingRetentionRepository) ClaimSourceRetention(context.Context, time.Duration, time.Duration) (SourceRetentionClaim, error) {
	return SourceRetentionClaim{}, repository.err
}
func (failingRetentionRepository) RecordSourceRetentionFailure(context.Context, SourceRetentionClaim, time.Duration) error {
	return nil
}
func (failingRetentionRepository) FinalizeSourceRetention(context.Context, SourceRetentionClaim) error {
	return nil
}

type retentionRecordFailureRepository struct{ err error }

func (repository retentionRecordFailureRepository) ClaimSourceRetention(context.Context, time.Duration, time.Duration) (SourceRetentionClaim, error) {
	return SourceRetentionClaim{ObjectKey: "external/tenant/sources/source.bin"}, nil
}
func (repository retentionRecordFailureRepository) RecordSourceRetentionFailure(context.Context, SourceRetentionClaim, time.Duration) error {
	return repository.err
}
func (retentionRecordFailureRepository) FinalizeSourceRetention(context.Context, SourceRetentionClaim) error {
	return nil
}

type idempotencyRetentionStub struct {
	errors  []error
	calls   int
	retried chan struct{}
}

func (repository *idempotencyRetentionStub) ExpireIdempotencyBatch(context.Context, int) (int64, error) {
	repository.calls++
	if len(repository.errors) > 0 {
		err := repository.errors[0]
		repository.errors = repository.errors[1:]
		return 0, err
	}
	if repository.retried != nil {
		select {
		case <-repository.retried:
		default:
			close(repository.retried)
		}
	}
	return 0, ErrIdempotencyRetentionNotAvailable
}

func (*transientRetentionRepository) RecordSourceRetentionFailure(context.Context, SourceRetentionClaim, time.Duration) error {
	return nil
}

func (*transientRetentionRepository) FinalizeSourceRetention(context.Context, SourceRetentionClaim) error {
	return nil
}

func (store *flakyRetentionStore) Delete(ctx context.Context, key string) error {
	if store.failures > 0 {
		store.failures--
		return errors.New("temporary object failure")
	}
	return store.memorySourceStore.Delete(ctx, key)
}

func TestSourceRetentionWorkerMarksRetriesAuditsAndFinalizes(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("s", 26), strings.Repeat("t", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := &flakyRetentionStore{memorySourceStore: newMemorySourceStore(), failures: 1}
	repository := newTestMySQLJobRepository(t, database, store)
	submitted, err := repository.Submit(context.Background(), tenantID, "retention-job-key-1", JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "retention-judge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Complete(context.Background(), claim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, claim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ExpireIdempotencyBatch(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	worker, err := NewSourceRetentionWorker(SourceRetentionWorkerConfig{
		Repository: repository, Objects: store, Retention: time.Hour, IdleDelay: time.Millisecond,
		DeleteTimeout: time.Second, ClaimLease: 2 * time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessNext(context.Background()); err == nil {
		t.Fatal("temporary object deletion failure was hidden")
	}
	if jobs := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job WHERE id = ?`, claim.Job.InternalID); jobs != 1 {
		t.Fatalf("failed delete removed job metadata: %d", jobs)
	}
	if retries := mustCount(t, database, `SELECT COUNT(*) FROM t_external_retention_audit WHERE event_type = 'DELETE_RETRY'`); retries != 1 {
		t.Fatalf("retry audit rows=%d", retries)
	}
	time.Sleep(5 * time.Millisecond)
	if err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job WHERE id = ?`, claim.Job.InternalID); jobs != 0 {
		t.Fatalf("retained terminal job rows=%d", jobs)
	}
	if sources := mustCount(t, database, `SELECT COUNT(*) FROM t_external_source_object WHERE id = ?`, claim.Job.Source.InternalID); sources != 0 {
		t.Fatalf("retained source rows=%d", sources)
	}
	if deleted := mustCount(t, database, `SELECT COUNT(*) FROM t_external_retention_audit WHERE event_type = 'DELETED'`); deleted != 1 {
		t.Fatalf("delete audit rows=%d", deleted)
	}
	objects, _, deletes := store.snapshot()
	if len(objects) != 0 || deletes != 1 {
		t.Fatalf("objects=%d successfulDeletes=%d", len(objects), deletes)
	}
}

func TestSourceRetentionWorkerRetriesTransientRepositoryErrors(t *testing.T) {
	repository := &transientRetentionRepository{retried: make(chan struct{})}
	worker, err := NewSourceRetentionWorker(SourceRetentionWorkerConfig{
		Repository: repository, Objects: newMemorySourceStore(), Retention: time.Hour,
		IdleDelay: time.Millisecond, DeleteTimeout: time.Second, ClaimLease: 2 * time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-repository.retried:
		cancel()
	case err := <-done:
		t.Fatalf("transient repository error stopped retention worker: %v", err)
	case <-time.After(time.Second):
		t.Fatal("retention worker did not retry transient repository error")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("retention worker shutdown error = %v", err)
	}
}

func TestSourceRetentionWorkerReturnsPermanentRepositoryInvariant(t *testing.T) {
	permanent := fmt.Errorf("%w: accounting invariant", ErrExternalJobUnavailable)
	worker, err := NewSourceRetentionWorker(SourceRetentionWorkerConfig{
		Repository: failingRetentionRepository{err: permanent}, Objects: newMemorySourceStore(), Retention: time.Hour,
		IdleDelay: time.Millisecond, DeleteTimeout: time.Second, ClaimLease: 2 * time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := worker.Run(ctx); !errors.Is(err, permanent) {
		t.Fatalf("permanent repository error=%v", err)
	}
}

func TestSourceRetentionWorkerDoesNotHidePermanentRetryRecordFailure(t *testing.T) {
	permanent := fmt.Errorf("%w: retention fence invariant", ErrExternalJobUnavailable)
	store := &flakyRetentionStore{memorySourceStore: newMemorySourceStore(), failures: 1}
	worker, err := NewSourceRetentionWorker(SourceRetentionWorkerConfig{
		Repository: retentionRecordFailureRepository{err: permanent}, Objects: store, Retention: time.Hour,
		IdleDelay: time.Millisecond, DeleteTimeout: time.Second, ClaimLease: 2 * time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(context.Background()); !errors.Is(err, permanent) {
		t.Fatalf("permanent retry-record error=%v", err)
	}
}

func TestIdempotencyRetentionWorkerRetriesTransientDatabaseError(t *testing.T) {
	repository := &idempotencyRetentionStub{
		errors:  []error{fmt.Errorf("expire batch: %w", &mysqlDriver.MySQLError{Number: 1205, Message: "lock wait timeout"})},
		retried: make(chan struct{}),
	}
	worker, err := NewIdempotencyRetentionWorker(repository, 100, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-repository.retried:
		cancel()
	case err := <-done:
		t.Fatalf("transient database error stopped idempotency retention: %v", err)
	case <-time.After(time.Second):
		t.Fatal("idempotency retention did not retry transient database error")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker shutdown error=%v", err)
	}
}

func TestIdempotencyRetentionWorkerReturnsPermanentRepositoryInvariant(t *testing.T) {
	permanent := fmt.Errorf("%w: retention invariant", ErrExternalJobUnavailable)
	worker, err := NewIdempotencyRetentionWorker(&idempotencyRetentionStub{errors: []error{permanent}}, 100, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(context.Background()); !errors.Is(err, permanent) {
		t.Fatalf("permanent repository error=%v", err)
	}
}

func TestSourceRetentionDoesNotMarkReferencedOrRecentSources(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	claim, err := repository.ClaimSourceRetention(context.Background(), 24*time.Hour, time.Minute)
	if !errors.Is(err, ErrSourceRetentionNotAvailable) || claim.ObjectKey != "" {
		t.Fatalf("empty retention claim=%+v error=%v", claim, err)
	}
}

func TestSourceRetentionLeasePreventsConcurrentTokenOverwrite(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("u", 26), strings.Repeat("v", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := newMemorySourceStore()
	firstRepository := newTestMySQLJobRepository(t, database, store)
	secondRepository := newTestMySQLJobRepository(t, database, store)
	submitted, err := firstRepository.Submit(context.Background(), tenantID, "retention-lease-key", JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")})
	if err != nil {
		t.Fatal(err)
	}
	jobClaim, err := firstRepository.ClaimNext(context.Background(), "retention-lease-judge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstRepository.Complete(context.Background(), jobClaim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, jobClaim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := firstRepository.ExpireIdempotencyBatch(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	firstClaim, err := firstRepository.ClaimSourceRetention(context.Background(), time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if secondClaim, err := secondRepository.ClaimSourceRetention(context.Background(), time.Hour, time.Minute); !errors.Is(err, ErrSourceRetentionNotAvailable) || len(secondClaim.DeleteToken) != 0 {
		t.Fatalf("concurrent repository overwrote active delete token: claim=%+v error=%v", secondClaim, err)
	}
	if _, err := database.Exec(`UPDATE t_external_source_object SET delete_lease_until = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND, delete_next_attempt_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?`, firstClaim.SourceInternalID); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := secondRepository.ClaimSourceRetention(context.Background(), time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondClaim.DeleteToken) == string(firstClaim.DeleteToken) {
		t.Fatal("expired retention lease reused its fencing token")
	}
	if err := firstRepository.FinalizeSourceRetention(context.Background(), firstClaim); !errors.Is(err, ErrSourceRetentionNotAvailable) {
		t.Fatalf("stale retention claimant finalized metadata: %v", err)
	}
}

func TestSourceRetentionClaimSkipsLockedTenantWithoutWaiting(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("4", 26), strings.Repeat("5", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	submitted, err := repository.Submit(context.Background(), tenantID, "retention-skip-locked", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	jobClaim, err := repository.ClaimNext(context.Background(), "retention-skip-judge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Complete(context.Background(), jobClaim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, jobClaim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ExpireIdempotencyBatch(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}

	blocking, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocking.Rollback()
	var lockedTenant uint64
	if err := blocking.QueryRow(`SELECT id FROM t_external_tenant WHERE external_id = ? FOR UPDATE`, tenantID).Scan(&lockedTenant); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err = repository.ClaimSourceRetention(ctx, time.Hour, time.Minute)
	if elapsed := time.Since(started); !errors.Is(err, ErrSourceRetentionNotAvailable) || elapsed > 500*time.Millisecond {
		t.Fatalf("locked tenant claim error=%v elapsed=%s", err, elapsed)
	}
}

func TestSourceRetentionClaimSkipsLockedOldestCandidateAndClaimsAnotherTenant(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	prepareCandidate := func(tenantID, bundleID, idempotencyKey, workerID string, ageHours int) WorkerJobClaim {
		t.Helper()
		insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
		submitted, err := repository.Submit(context.Background(), tenantID, idempotencyKey, JudgeJobRequest{
			BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
		})
		if err != nil {
			t.Fatal(err)
		}
		jobClaim, err := repository.ClaimNext(context.Background(), workerID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Complete(context.Background(), jobClaim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL ? HOUR WHERE id = ?`, ageHours, jobClaim.Job.InternalID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`DELETE FROM t_external_idempotency WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
			t.Fatal(err)
		}
		return jobClaim
	}
	oldest := prepareCandidate(strings.Repeat("a", 26), strings.Repeat("b", 26), "retention-oldest-locked", "retention-oldest-worker", 3)
	claimable := prepareCandidate(strings.Repeat("c", 26), strings.Repeat("d", 26), "retention-second-unlocked", "retention-second-worker", 2)

	blocking, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocking.Rollback()
	if err := blocking.QueryRow(`SELECT id FROM t_external_tenant WHERE id = ? FOR UPDATE`, oldest.Job.TenantInternalID).Scan(new(uint64)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	retentionClaim, err := repository.ClaimSourceRetention(ctx, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if retentionClaim.JobInternalID != claimable.Job.InternalID {
		t.Fatalf("claimed job=%d want unlocked candidate=%d", retentionClaim.JobInternalID, claimable.Job.InternalID)
	}
}

func TestSourceRetentionExpiresIdempotencyInBoundedBatches(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("w", 26), strings.Repeat("x", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	submitted, err := repository.Submit(context.Background(), tenantID, "retention-batch-job", JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")})
	if err != nil {
		t.Fatal(err)
	}
	jobClaim, err := repository.ClaimNext(context.Background(), "retention-batch-judge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Complete(context.Background(), jobClaim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, jobClaim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 1000; index++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("retention-expired-%04d", index)))
		if _, err := tx.Exec(`INSERT INTO t_external_idempotency(tenant_id, operation_scope, key_digest, request_hash, resource_type, resource_external_id, response_status, response_json, expires_at) SELECT id, 'retention-test', ?, ?, 'none', ?, 200, JSON_OBJECT(), CURRENT_TIMESTAMP(3) - INTERVAL 2 SECOND FROM t_external_tenant WHERE external_id = ?`, digest[:], digest[:], strings.Repeat("z", 26), tenantID); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if expired, err := repository.ExpireIdempotencyBatch(context.Background(), 1000); err != nil || expired != 1000 {
		t.Fatalf("bounded idempotency cleanup expired=%d error=%v", expired, err)
	}
	if _, err := repository.ClaimSourceRetention(context.Background(), time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	if remaining := mustCount(t, database, `SELECT COUNT(*) FROM t_external_idempotency WHERE expires_at <= CURRENT_TIMESTAMP(3)`); remaining != 1 {
		t.Fatalf("bounded idempotency cleanup remaining=%d, want 1", remaining)
	}
}

func TestSourceRetentionFinalizeAndCancelShareTenantJobSourceLockOrder(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("2", 26), strings.Repeat("3", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 40)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	for iteration := 0; iteration < 12; iteration++ {
		submitted, err := repository.Submit(context.Background(), tenantID, fmt.Sprintf("retention-cancel-%04d", iteration), JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")})
		if err != nil {
			t.Fatal(err)
		}
		jobClaim, err := repository.ClaimNext(context.Background(), "retention-cancel-judge", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Complete(context.Background(), jobClaim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, jobClaim.Job.InternalID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ExpireIdempotencyBatch(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		retentionClaim, err := repository.ClaimSourceRetention(context.Background(), time.Hour, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(context.Background(), retentionClaim.ObjectKey); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errorsChannel := make(chan error, 2)
		go func() {
			<-start
			errorsChannel <- repository.FinalizeSourceRetention(context.Background(), retentionClaim)
		}()
		go func() {
			<-start
			_, err := repository.Cancel(context.Background(), tenantID, submitted.Job.ExternalID)
			if errors.Is(err, ErrExternalJobNotFound) {
				err = nil
			}
			errorsChannel <- err
		}()
		close(start)
		for result := 0; result < 2; result++ {
			if err := <-errorsChannel; err != nil {
				t.Fatalf("iteration %d retention/cancel lock race: %v", iteration, err)
			}
		}
	}
}
