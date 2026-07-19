package external

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
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
	clock := &mutableClock{now: time.Now().UTC()}
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

func TestMySQLWorkerSkipsQuotaFullTenantWithoutStarvingOthers(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("a", 26)
	tenantB := strings.Repeat("b", 26)
	bundleA := strings.Repeat("c", 26)
	bundleB := strings.Repeat("d", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, "", 4)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, "", 4)
	clock := &mutableClock{now: time.Now().UTC()}
	repository := newTestMySQLJobRepositoryWithClock(t, database, newMemorySourceStore(), clock.Now)
	for index := 0; index < 2; index++ {
		if _, err := repository.Submit(context.Background(), tenantA, "starve-tenant-a-00"+string(rune('1'+index)), JudgeJobRequest{
			BundleID: bundleA, Language: "cpp20", SourceCode: []byte("int main(){}"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.Submit(context.Background(), tenantB, "starve-tenant-b-001", JudgeJobRequest{
		BundleID: bundleB, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}); err != nil {
		t.Fatal(err)
	}
	first, err := repository.ClaimNext(context.Background(), "worker-a", time.Minute)
	if err != nil || first.Job.TenantExternalID != tenantA {
		t.Fatalf("first claim = %+v error=%v", first, err)
	}
	second, err := repository.ClaimNext(context.Background(), "worker-b", time.Minute)
	if err != nil || second.Job.TenantExternalID != tenantB {
		t.Fatalf("quota-full tenant starved runnable tenant: claim=%+v error=%v", second, err)
	}
}

func TestMySQLWorkerRetriesTenantSelectionAfterConcurrentContention(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("a", 26)
	tenantB := strings.Repeat("b", 26)
	bundleA := strings.Repeat("c", 26)
	bundleB := strings.Repeat("d", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, "", 2)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, "", 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	for _, job := range []struct{ tenant, key, bundle string }{
		{tenantA, "contention-tenant-a", bundleA},
		{tenantB, "contention-tenant-b", bundleB},
	} {
		if _, err := repository.Submit(context.Background(), job.tenant, job.key, JudgeJobRequest{
			BundleID: job.bundle, Language: "cpp20", SourceCode: []byte("int main(){}"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	blocker, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec("SELECT id FROM t_external_tenant WHERE external_id = ? FOR UPDATE", tenantA); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	claimContext, cancelClaims := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClaims()
	claims := make(chan WorkerJobClaim, 2)
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for _, workerID := range []string{"contention-worker-a", "contention-worker-b"} {
		workerID := workerID
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claim, err := repository.ClaimNext(claimContext, workerID, time.Minute)
			claims <- claim
			errorsChannel <- err
		}()
	}
	close(start)
	// Both calls can perform the unlocked candidate read, then wait on tenant A.
	time.Sleep(250 * time.Millisecond)
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(claims)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("claim after tenant contention: %v", err)
		}
	}
	seen := map[string]bool{}
	for claim := range claims {
		seen[claim.Job.TenantExternalID] = true
	}
	if !seen[tenantA] || !seen[tenantB] {
		t.Fatalf("contention starved runnable tenant: %#v", seen)
	}
}

func TestMySQLWorkerLeaseExpiryUsesDatabaseClock(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("k", 26)
	bundleID := strings.Repeat("m", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := newMemorySourceStore()
	repository := newTestMySQLJobRepository(t, database, store)
	if _, err := repository.Submit(context.Background(), tenantID, "database-clock-lease", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}); err != nil {
		t.Fatal(err)
	}
	first, err := repository.ClaimNext(context.Background(), "correct-clock-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	skewed := newTestMySQLJobRepositoryWithClock(t, database, store, func() time.Time {
		return time.Now().Add(24 * time.Hour)
	})
	if _, err := skewed.ClaimNext(context.Background(), "future-clock-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("future application clock reclaimed active lease: %v", err)
	}
	if err := skewed.Heartbeat(context.Background(), first, time.Minute); err != nil {
		t.Fatalf("future application clock rejected active lease heartbeat: %v", err)
	}
}

func TestMySQLWorkerSchedulingAndBackoffUseDatabaseClockDespiteApplicationSkew(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("6", 26)
	bundleID := strings.Repeat("7", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := newMemorySourceStore()
	futureClock := newTestMySQLJobRepositoryWithClock(t, database, store, func() time.Time {
		return time.Now().Add(10 * 365 * 24 * time.Hour)
	})
	job, err := futureClock.Submit(context.Background(), tenantID, "database-clock-schedule", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	pastClock := newTestMySQLJobRepositoryWithClock(t, database, store, func() time.Time {
		return time.Now().Add(-10 * 365 * 24 * time.Hour)
	})
	claim, err := pastClock.ClaimNext(context.Background(), "past-clock-worker", time.Minute)
	if err != nil || claim.Job.ExternalID != job.Job.ExternalID {
		t.Fatalf("database-due job with past application clock: claim=%+v error=%v", claim, err)
	}
	disposition, err := pastClock.FailInfrastructure(context.Background(), claim, InfrastructureFailure{
		Code: "SANDBOX_UNAVAILABLE", RetryDelay: 20 * time.Minute,
	})
	if err != nil || disposition != FailureRequeued {
		t.Fatalf("database backoff disposition=%s error=%v", disposition, err)
	}

	if _, err := futureClock.ClaimNext(context.Background(), "future-clock-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("future application clock bypassed database backoff: %v", err)
	}
	var backoffSeconds int
	if err := database.QueryRow(`
SELECT TIMESTAMPDIFF(SECOND, CURRENT_TIMESTAMP(3), next_attempt_at)
FROM t_external_job WHERE id = ?`, claim.Job.InternalID).Scan(&backoffSeconds); err != nil {
		t.Fatal(err)
	}
	if backoffSeconds < 1190 || backoffSeconds > 1200 {
		t.Fatalf("database-relative retry backoff seconds=%d", backoffSeconds)
	}
	if _, err := database.Exec(`UPDATE t_external_job
SET next_attempt_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?`, claim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	second, err := futureClock.ClaimNext(context.Background(), "future-clock-worker", time.Minute)
	if err != nil || second.Job.ExternalID != job.Job.ExternalID || second.AttemptNo != 2 {
		t.Fatalf("database-due retry with future application clock: claim=%+v error=%v", second, err)
	}
}

func TestMySQLSubmitIdempotencyExpiryUsesDatabaseClockDespitePastApplicationClock(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("q", 26)
	bundleID := strings.Repeat("r", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := newMemorySourceStore()
	pastClock := newTestMySQLJobRepositoryWithClock(t, database, store, func() time.Time {
		return time.Now().Add(-10 * 365 * 24 * time.Hour)
	})
	request := JudgeJobRequest{
		BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}
	first, err := pastClock.Submit(context.Background(), tenantID, "database-clock-idempotency", request)
	if err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLJobRepository(t, database, store)
	replayed, err := repository.Submit(context.Background(), tenantID, "database-clock-idempotency", request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Job.ExternalID != first.Job.ExternalID {
		t.Fatalf("past application clock expired fresh idempotency record: first=%+v replay=%+v", first, replayed)
	}
	if jobs := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job"); jobs != 1 {
		t.Fatalf("idempotent replay jobs=%d", jobs)
	}
	var ttlSeconds int
	if err := database.QueryRow(`
SELECT TIMESTAMPDIFF(SECOND, CURRENT_TIMESTAMP(3), expires_at)
FROM t_external_idempotency WHERE resource_external_id = ?`, first.Job.ExternalID).Scan(&ttlSeconds); err != nil {
		t.Fatal(err)
	}
	if ttlSeconds < int((24*time.Hour)/time.Second)-10 || ttlSeconds > int((24*time.Hour)/time.Second) {
		t.Fatalf("database-relative idempotency ttl seconds=%d", ttlSeconds)
	}
}

func TestMySQLWorkerSkipsDisabledTenantWithoutStarvingOthers(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA := strings.Repeat("e", 26)
	tenantB := strings.Repeat("f", 26)
	bundleA := strings.Repeat("g", 26)
	bundleB := strings.Repeat("h", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, "", 2)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, "", 2)
	clock := &mutableClock{now: time.Now().UTC()}
	repository := newTestMySQLJobRepositoryWithClock(t, database, newMemorySourceStore(), clock.Now)
	if _, err := repository.Submit(context.Background(), tenantA, "disabled-tenant-job", JudgeJobRequest{
		BundleID: bundleA, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}); err != nil {
		t.Fatal(err)
	}
	first, err := repository.ClaimNext(context.Background(), "worker-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_tenant SET status = 'DISABLED' WHERE external_id = ?", tenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Submit(context.Background(), tenantB, "active-tenant-job", JudgeJobRequest{
		BundleID: bundleB, Language: "cpp20", SourceCode: []byte("int main(){}"),
	}); err != nil {
		t.Fatal(err)
	}
	second, err := repository.ClaimNext(context.Background(), "worker-b", time.Minute)
	if err != nil || second.Job.TenantExternalID != tenantB || second.Job.ExternalID == first.Job.ExternalID {
		t.Fatalf("disabled tenant starved active tenant: claim=%+v error=%v", second, err)
	}
	expireClaimLease(t, database, first)
	if err := repository.Complete(context.Background(), second, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClaimNext(context.Background(), "cleanup-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("disabled tenant cleanup returned claim: %v", err)
	}
	settled, err := repository.Get(context.Background(), tenantA, first.Job.ExternalID)
	if err != nil || settled.Status != JobStatusFailed || settled.FailureCode != "TENANT_DISABLED" {
		t.Fatalf("disabled tenant job=%+v error=%v", settled, err)
	}
}

func TestMySQLWorkerHeartbeatCompletionAndRestartReclaimAreFenced(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("x", 26)
	bundleID := strings.Repeat("y", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 4)
	clock := &mutableClock{now: time.Now().UTC()}
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
	if err := repository.Heartbeat(context.Background(), first, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	expireClaimLease(t, database, first)
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
	clock := &mutableClock{now: time.Now().UTC()}
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
	forged := claim
	forged.Job.Source.ObjectKey = "external/forged/source.bin"
	forged.Job.Source.ExternalID = strings.Repeat("5", 26)
	loaded, err = repository.LoadClaimSource(context.Background(), forged)
	if err != nil || !bytes.Equal(loaded, plaintext) {
		t.Fatalf("claim-carried source metadata was trusted: loaded=%q error=%v", loaded, err)
	}
	clear(loaded)

	store.mutex.Lock()
	store.objects[claim.Job.Source.ObjectKey][0] ^= 0xff
	store.mutex.Unlock()
	if _, err := repository.LoadClaimSource(context.Background(), claim); !errors.Is(err, ErrSourceEncryption) {
		t.Fatalf("tampered source error = %v", err)
	}
}

func TestMySQLWorkerLoadsCanonicalInputOnlyForActiveClaim(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("i", 26)
	bundleID := strings.Repeat("j", 26)
	secondBundleID := strings.Repeat("l", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	insertBundleForTenant(t, database, tenantID, secondBundleID, 2750, 640)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	plaintext := []byte("package main")
	if _, err := repository.Submit(context.Background(), tenantID, "canonical-input-key", JudgeJobRequest{
		BundleID: bundleID, Language: "go126", SourceCode: plaintext, StopOnFailure: false,
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "canonical-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input, err := repository.LoadClaimInput(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input.SourceCode, plaintext) || input.Language != "go126" || input.StopOnFailure ||
		input.Bundle.ObjectKey == "" || input.Bundle.Manifest.Limits.TimeLimitMillis != 1000 || input.Bundle.Manifest.Limits.MemoryLimitMiB != 256 {
		t.Fatalf("canonical input = %+v", input)
	}
	stale := claim
	stale.AttemptNo++
	if _, err := repository.LoadClaimInput(context.Background(), stale); !errors.Is(err, ErrStaleJobClaim) {
		t.Fatalf("stale input error = %v", err)
	}
	if cancelled, err := repository.ClaimCancelled(context.Background(), claim); err != nil || cancelled {
		t.Fatalf("initial cancellation=%v error=%v", cancelled, err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, claim.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := repository.ClaimCancelled(context.Background(), claim); err != nil || !cancelled {
		t.Fatalf("requested cancellation=%v error=%v", cancelled, err)
	}
	if _, err := repository.ClaimCancelled(context.Background(), stale); !errors.Is(err, ErrStaleJobClaim) {
		t.Fatalf("stale cancellation control error = %v", err)
	}
	if err := repository.Complete(context.Background(), claim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	cancelledJob, err := repository.Get(context.Background(), tenantID, claim.Job.ExternalID)
	if err != nil || cancelledJob.Status != JobStatusCancelled || cancelledJob.Result != nil {
		t.Fatalf("cancelled completion persisted stale result: job=%+v error=%v", cancelledJob, err)
	}

	if _, err := repository.Submit(context.Background(), tenantID, "second-canonical-input", JudgeJobRequest{
		BundleID: secondBundleID, Language: "go126", SourceCode: plaintext, StopOnFailure: true,
	}); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := repository.ClaimNext(context.Background(), "canonical-worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondInput, err := repository.LoadClaimInput(context.Background(), secondClaim)
	if err != nil {
		t.Fatal(err)
	}
	if secondInput.Bundle.Manifest.Limits.TimeLimitMillis != 2750 || secondInput.Bundle.Manifest.Limits.MemoryLimitMiB != 640 || !secondInput.StopOnFailure {
		t.Fatalf("second bundle canonical input = %+v", secondInput)
	}
}

func TestMySQLWorkerInfrastructureRetryAndCancellationRecovery(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("z", 26)
	bundleID := strings.Repeat("2", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 5)
	clock := &mutableClock{now: time.Now().UTC()}
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
	if _, err := database.Exec(`UPDATE t_external_job
SET next_attempt_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?`, claim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
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
	expireClaimLease(t, database, cancelClaim)
	restarted := newTestMySQLJobRepositoryWithClock(t, database, store, clock.Now)
	if _, err := restarted.ClaimNext(context.Background(), "recovery-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("cancel recovery returned executable claim: %v", err)
	}
	cancelled, err := restarted.Get(context.Background(), tenantID, cancelJob.Job.ExternalID)
	if err != nil || cancelled.Status != JobStatusCancelled || cancelled.CompletedAt == nil {
		t.Fatalf("recovered cancellation = %+v, error = %v", cancelled, err)
	}

	exhaustedJob, err := restarted.Submit(context.Background(), tenantID, "exhausted-retry-key", JudgeJobRequest{
		BundleID: bundleID, Language: "java17", SourceCode: []byte("class Exhausted {}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		claim, err := restarted.ClaimNext(context.Background(), "failure-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		disposition, err := restarted.FailInfrastructure(context.Background(), claim, InfrastructureFailure{Code: "SANDBOX_UNAVAILABLE"})
		if err != nil {
			t.Fatal(err)
		}
		want := FailureRequeued
		if attempt == 3 {
			want = FailureTerminal
		}
		if disposition != want {
			t.Fatalf("attempt %d disposition = %s, want %s", attempt, disposition, want)
		}
	}
	exhausted, err := restarted.Get(context.Background(), tenantID, exhaustedJob.Job.ExternalID)
	if err != nil || exhausted.Status != JobStatusFailed || exhausted.FailureCode != "SANDBOX_UNAVAILABLE" || exhausted.CompletedAt == nil {
		t.Fatalf("exhausted job = %+v, error = %v", exhausted, err)
	}
}

func TestMySQLWorkerCancelAndInfrastructureFailureShareTenantJobLockOrder(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("n", 26)
	bundleID := strings.Repeat("p", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 40)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())

	for iteration := 0; iteration < 24; iteration++ {
		job, err := repository.Submit(context.Background(), tenantID,
			fmt.Sprintf("cancel-failure-race-%03d", iteration),
			JudgeJobRequest{BundleID: bundleID, Language: "cpp20", SourceCode: []byte("int main(){}")})
		if err != nil {
			t.Fatal(err)
		}
		claim, err := repository.ClaimNext(context.Background(), "race-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := make(chan struct{})
		errorsChannel := make(chan error, 2)
		go func() {
			<-start
			_, err := repository.Cancel(ctx, tenantID, job.Job.ExternalID)
			errorsChannel <- err
		}()
		go func() {
			<-start
			_, err := repository.FailInfrastructure(ctx, claim, InfrastructureFailure{Code: "SANDBOX_UNAVAILABLE"})
			errorsChannel <- err
		}()
		close(start)
		for completed := 0; completed < 2; completed++ {
			if err := <-errorsChannel; err != nil {
				cancel()
				t.Fatalf("iteration %d cancel/failure race: %v", iteration, err)
			}
		}
		cancel()
		settled, err := repository.Get(context.Background(), tenantID, job.Job.ExternalID)
		if err != nil || settled.Status != JobStatusCancelled {
			t.Fatalf("iteration %d settled job=%+v error=%v", iteration, settled, err)
		}
	}
}

func newTestMySQLJobRepositoryWithClock(
	t *testing.T,
	database *sql.DB,
	store SourceObjectStore,
	now func() time.Time,
) *testMySQLJobRepository {
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
	return &testMySQLJobRepository{MySQLJobRepository: repository}
}

func expireClaimLease(t *testing.T, database *sql.DB, claim WorkerJobClaim) {
	t.Helper()
	if _, err := database.Exec(`UPDATE t_external_job
SET lease_until = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND
WHERE id = ? AND attempt_no = ?`, claim.Job.InternalID, claim.AttemptNo); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job_attempt
SET lease_until = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND
WHERE job_id = ? AND attempt_no = ?`, claim.Job.InternalID, claim.AttemptNo); err != nil {
		t.Fatal(err)
	}
}
