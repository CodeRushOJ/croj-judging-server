package external

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type mutableClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *mutableClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *mutableClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(duration)
	clock.mutex.Unlock()
}

func TestMySQLWorkerClaimsUseSkipLockedAndTenantRunningQuota(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("v", 26)
	bundleID := strings.Repeat("w", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 8)
	if _, err := database.Exec(`UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxRunningJobs', 2)
WHERE external_id = ?`, tenantID); err != nil {
		t.Fatal(err)
	}
	clock := &mutableClock{now: time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)}
	repository := newTestMySQLJobRepositoryWithClock(t, database, newMemorySourceStore(), clock.Now)
	for index := 0; index < 3; index++ {
		if _, err := repository.Submit(context.Background(), tenantID, "claim-job-key-000"+string(rune('1'+index)), JudgeJobRequest{
			BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){return 0;}"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	claims := make(chan WorkerJobClaim, 2)
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for _, workerID := range []string{"worker-a", "worker-b"} {
		workerID := workerID
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claim, err := repository.ClaimNext(context.Background(), workerID, 30*time.Second)
			claims <- claim
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(claims)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("claim error = %v", err)
		}
	}
	seen := make(map[string]bool)
	for claim := range claims {
		if claim.AttemptNo != 1 || len(claim.LeaseToken) != 32 || claim.Job.Status != JobStatusRunning || seen[claim.Job.ExternalID] {
			t.Fatalf("invalid or duplicate claim = %+v", claim)
		}
		seen[claim.Job.ExternalID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("claims = %d", len(seen))
	}
	if _, err := repository.ClaimNext(context.Background(), "worker-c", 30*time.Second); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("tenant running quota claim error = %v", err)
	}
	if attempts := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job_attempt WHERE status = 'RUNNING'"); attempts != 2 {
		t.Fatalf("running attempts = %d", attempts)
	}
}

func TestMySQLWorkerHeartbeatCompletionAndRestartReclaimAreFenced(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("x", 26)
	bundleID := strings.Repeat("y", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 4)
	clock := &mutableClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	result, err := repository.Submit(context.Background(), tenantID, "reclaim-job-key-001", JudgeJobRequest{
		BundleID: bundleID, Language: "go126", SourceCode: []byte("package main\nfunc main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := repository.ClaimNext(context.Background(), "dead-worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	if err := repository.Heartbeat(context.Background(), first, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	clock.Advance(31 * time.Second)
	restarted := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	second, err := restarted.ClaimNext(context.Background(), "recovery-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Job.ExternalID != result.Job.ExternalID || second.AttemptNo != 2 || bytes.Equal(second.LeaseToken, first.LeaseToken) {
		t.Fatalf("reclaimed claim = %+v, first = %+v", second, first)
	}
	accepted := DurableJobResult{
		Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: 12, MemoryBytes: 4096,
		Cases: []DurableCaseResult{{CaseID: "case-01", Verdict: "ACCEPTED", TimeMillis: 12, MemoryBytes: 4096}},
	}
	if err := repository.Complete(context.Background(), first, accepted); !errors.Is(err, ErrStaleJobClaim) {
		t.Fatalf("stale completion error = %v", err)
	}
	if err := restarted.Complete(context.Background(), second, accepted); err != nil {
		t.Fatal(err)
	}
	completed, err := restarted.Get(context.Background(), tenantID, result.Job.ExternalID)
	if err != nil || completed.Status != JobStatusSucceeded || completed.Result == nil || completed.Result.Verdict != "ACCEPTED" {
		t.Fatalf("completed = %+v, error = %v", completed, err)
	}
	if expired := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job_attempt WHERE status = 'EXPIRED'"); expired != 1 {
		t.Fatalf("expired attempts = %d", expired)
	}
	if succeeded := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job_attempt WHERE status = 'SUCCEEDED'"); succeeded != 1 {
		t.Fatalf("succeeded attempts = %d", succeeded)
	}
}

func TestMySQLWorkerLoadsAuthenticatedSourceForItsClaim(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("3", 26)
	bundleID := strings.Repeat("4", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	clock := &mutableClock{now: time.Date(2026, 7, 19, 12, 30, 0, 0, time.UTC)}
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	plaintext := []byte("fn main() { println!(\"safe\"); }")
	if _, err := repository.Submit(context.Background(), tenantID, "source-load-key-001", JudgeJobRequest{
		BundleID: bundleID, Language: "rust185", SourceCode: plaintext,
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "source-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadClaimSource(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, plaintext) {
		t.Fatalf("loaded source mismatch")
	}
	clear(loaded)

	store.mutex.Lock()
	store.objects[claim.Job.Source.ObjectKey][0] ^= 0xff
	store.mutex.Unlock()
	if _, err := repository.LoadClaimSource(context.Background(), claim); !errors.Is(err, ErrSourceEncryption) {
		t.Fatalf("tampered source error = %v", err)
	}
}

func TestMySQLWorkerInfrastructureRetryAndCancellationRecovery(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("z", 26)
	bundleID := strings.Repeat("2", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 5)
	clock := &mutableClock{now: time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)}
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	retryJob, err := repository.Submit(context.Background(), tenantID, "retry-job-key-0001", JudgeJobRequest{
		BundleID: bundleID, Language: "java17", SourceCode: []byte("class Main {}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := repository.FailInfrastructure(context.Background(), claim, InfrastructureFailure{Code: "SANDBOX_UNAVAILABLE", RetryDelay: 20 * time.Second})
	if err != nil || disposition != FailureRequeued {
		t.Fatalf("retry disposition = %s, error = %v", disposition, err)
	}
	if _, err := repository.ClaimNext(context.Background(), "worker-b", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("job claimed before backoff elapsed: %v", err)
	}
	clock.Advance(20 * time.Second)
	second, err := repository.ClaimNext(context.Background(), "worker-b", time.Minute)
	if err != nil || second.Job.ExternalID != retryJob.Job.ExternalID || second.AttemptNo != 2 {
		t.Fatalf("retried claim = %+v, error = %v", second, err)
	}

	cancelJob, err := repository.Submit(context.Background(), tenantID, "cancel-recovery-key", JudgeJobRequest{
		BundleID: bundleID, Language: "java17", SourceCode: []byte("class Cancel {}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Finish the retry job so the tenant's running slot can claim the cancellation job.
	if err := repository.Complete(context.Background(), second, DurableJobResult{Verdict: "WRONG_ANSWER", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	cancelClaim, err := repository.ClaimNext(context.Background(), "worker-c", 10*time.Second)
	if err != nil || cancelClaim.Job.ExternalID != cancelJob.Job.ExternalID {
		t.Fatalf("cancel claim = %+v, error = %v", cancelClaim, err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, cancelJob.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	restarted := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	if _, err := restarted.ClaimNext(context.Background(), "recovery-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("cancel recovery returned executable claim: %v", err)
	}
	cancelled, err := restarted.Get(context.Background(), tenantID, cancelJob.Job.ExternalID)
	if err != nil || cancelled.Status != JobStatusCancelled || cancelled.CompletedAt == nil {
		t.Fatalf("recovered cancellation = %+v, error = %v", cancelled, err)
	}
}

func newTestMySQLJobRepositoryWithClock(
	t *testing.T,
	database *sql.DB,
	store SourceObjectStore,
	now func() time.Time,
) *MySQLJobRepository {
	t.Helper()
	cipher, err := NewSourceCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{0x42}, 32)}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewMySQLJobRepository(MySQLJobRepositoryConfig{
		Database: database, Random: rand.Reader, Now: now,
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
