package external

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMySQLTerminalTransitionsAtomicallyCreateStableWebhookEvents(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("a", 26)
	bundleID := strings.Repeat("b", 26)
	callbackID := strings.Repeat("c", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 8)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())

	completed := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-complete")
	claim, err := repository.ClaimNext(context.Background(), "webhook-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result := DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{}}
	if err := repository.Complete(context.Background(), claim, result); err != nil {
		t.Fatal(err)
	}
	assertPendingTerminalEvent(t, database, completed.Job.ExternalID, "judge.job.completed", JobStatusSucceeded)

	cancelled := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-cancel-0001")
	if _, err := repository.Cancel(context.Background(), tenantID, cancelled.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	assertPendingTerminalEvent(t, database, cancelled.Job.ExternalID, "judge.job.cancelled", JobStatusCancelled)

	if _, err := database.Exec(`UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxInfrastructureTries', 1)
WHERE external_id = ?`, tenantID); err != nil {
		t.Fatal(err)
	}
	failed := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-failure-001")
	failureClaim, err := repository.ClaimNext(context.Background(), "failure-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := repository.FailInfrastructure(context.Background(), failureClaim, InfrastructureFailure{Code: "SANDBOX_UNAVAILABLE"})
	if err != nil || disposition != FailureTerminal {
		t.Fatalf("terminal failure disposition=%s error=%v", disposition, err)
	}
	assertPendingTerminalEvent(t, database, failed.Job.ExternalID, "judge.job.failed", JobStatusFailed)

	withoutCallback, err := repository.Submit(context.Background(), tenantID, "webhook-none-0001", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, withoutCallback.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox"); got != 3 {
		t.Fatalf("outbox rows=%d want=3", got)
	}
}

func TestMySQLRunningCancellationCreatesOneWebhookOnWorkerFinalization(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("d", 26)
	bundleID := strings.Repeat("e", 26)
	callbackID := strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 4)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-running-cancel")
	claim, err := repository.ClaimNext(context.Background(), "cancel-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox"); got != 0 {
		t.Fatalf("running cancel emitted early webhook rows=%d", got)
	}
	if err := repository.Complete(context.Background(), claim, DurableJobResult{
		Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{},
	}); err != nil {
		t.Fatal(err)
	}
	assertPendingTerminalEvent(t, database, job.Job.ExternalID, "judge.job.cancelled", JobStatusCancelled)
	if _, err := repository.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID); got != 1 {
		t.Fatalf("webhook rows after replay=%d want=1", got)
	}

	failedAfterCancel := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-cancel-failure")
	failureClaim, err := repository.ClaimNext(context.Background(), "cancel-failure-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, failedAfterCancel.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	disposition, err := repository.FailInfrastructure(context.Background(), failureClaim, InfrastructureFailure{Code: "SANDBOX_UNAVAILABLE"})
	if err != nil || disposition != FailureCancelled {
		t.Fatalf("cancelled failure disposition=%s error=%v", disposition, err)
	}
	assertPendingTerminalEvent(t, database, failedAfterCancel.Job.ExternalID, "judge.job.cancelled", JobStatusCancelled)
	settled, err := repository.Get(context.Background(), tenantID, failedAfterCancel.Job.ExternalID)
	if err != nil || settled.FailureCode != "" {
		t.Fatalf("cancelled failure code=%q error=%v", settled.FailureCode, err)
	}
}

func TestMySQLExpiredClaimsAtomicallyCreateCancellationAndFailureWebhooks(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("j", 26)
	bundleID := strings.Repeat("k", 26)
	callbackID := strings.Repeat("m", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 4)
	if _, err := database.Exec(`UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxInfrastructureTries', 1)
WHERE external_id = ?`, tenantID); err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())

	cancelled := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-expired-cancel")
	cancelClaim, err := repository.ClaimNext(context.Background(), "expired-cancel-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cancel(context.Background(), tenantID, cancelled.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	expireClaimLease(t, database, cancelClaim)
	if _, err := repository.ClaimNext(context.Background(), "recovery-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("expired cancellation recovery error=%v", err)
	}
	assertPendingTerminalEvent(t, database, cancelled.Job.ExternalID, "judge.job.cancelled", JobStatusCancelled)

	failed := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-expired-failure")
	failureClaim, err := repository.ClaimNext(context.Background(), "expired-failure-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expireClaimLease(t, database, failureClaim)
	if _, err := repository.ClaimNext(context.Background(), "recovery-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("expired exhausted recovery error=%v", err)
	}
	assertPendingTerminalEvent(t, database, failed.Job.ExternalID, "judge.job.failed", JobStatusFailed)
}

func TestMySQLDisabledTenantExpiredRunningClaimAtomicallyCreatesFailureWebhook(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("a", 26)
	bundleID := strings.Repeat("b", 26)
	callbackID := strings.Repeat("c", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-disabled-expired")
	claim, err := repository.ClaimNext(context.Background(), "disabled-expired-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_tenant SET status = 'DISABLED' WHERE external_id = ?", tenantID); err != nil {
		t.Fatal(err)
	}
	expireClaimLease(t, database, claim)

	if _, err := repository.ClaimNext(context.Background(), "disabled-recovery-worker", time.Minute); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("disabled tenant recovery error=%v", err)
	}
	settled, err := repository.Get(context.Background(), tenantID, job.Job.ExternalID)
	if err != nil || settled.Status != JobStatusFailed || settled.FailureCode != "TENANT_DISABLED" {
		t.Fatalf("disabled tenant terminal job=%+v error=%v", settled, err)
	}
	if got := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job_attempt
WHERE job_id = ? AND attempt_no = ? AND status = 'EXPIRED'`, job.Job.InternalID, claim.AttemptNo); got != 1 {
		t.Fatalf("expired attempts=%d want=1", got)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID); got != 1 {
		t.Fatalf("disabled tenant webhook rows=%d want=1", got)
	}
	assertPendingTerminalEvent(t, database, job.Job.ExternalID, "judge.job.failed", JobStatusFailed)
	var payloadBody []byte
	if err := database.QueryRow("SELECT payload_body FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID).Scan(&payloadBody); err != nil {
		t.Fatal(err)
	}
	var payload terminalWebhookPayload
	if err := json.Unmarshal(payloadBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FailureCode != "TENANT_DISABLED" {
		t.Fatalf("failure code=%q want=TENANT_DISABLED", payload.FailureCode)
	}
}

func TestMySQLDisabledTenantWebhookInsertFailureRollsBackExpiredRunningTransition(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("d", 26)
	bundleID := strings.Repeat("e", 26)
	callbackID := strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-disabled-rollback")
	claim, err := repository.ClaimNext(context.Background(), "disabled-rollback-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_tenant SET status = 'DISABLED' WHERE external_id = ?", tenantID); err != nil {
		t.Fatal(err)
	}
	expireClaimLease(t, database, claim)
	if _, err := database.Exec(`ALTER TABLE t_external_webhook_outbox
ADD CONSTRAINT chk_test_reject_disabled_webhook CHECK (event_type <> event_type)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec("ALTER TABLE t_external_webhook_outbox DROP CHECK chk_test_reject_disabled_webhook")
	})

	if _, err := repository.ClaimNext(context.Background(), "disabled-rollback-recovery", time.Minute); !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("disabled tenant rejected webhook error=%v", err)
	}
	settled, err := repository.Get(context.Background(), tenantID, job.Job.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != JobStatusRunning || settled.FailureCode != "" {
		t.Fatalf("rolled back job=%+v", settled)
	}
	if got := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job_attempt
WHERE job_id = ? AND attempt_no = ? AND status = 'RUNNING'`, job.Job.InternalID, claim.AttemptNo); got != 1 {
		t.Fatalf("running attempts after rollback=%d want=1", got)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID); got != 0 {
		t.Fatalf("webhook rows after rollback=%d want=0", got)
	}
}

func TestMySQLWebhookInsertFailureRollsBackTerminalJobTransition(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("n", 26)
	bundleID := strings.Repeat("p", 26)
	callbackID := strings.Repeat("q", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-atomic-rollback")
	claim, err := repository.ClaimNext(context.Background(), "rollback-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`ALTER TABLE t_external_webhook_outbox
ADD CONSTRAINT chk_test_reject_webhook CHECK (event_type <> event_type)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec("ALTER TABLE t_external_webhook_outbox DROP CHECK chk_test_reject_webhook")
	})
	err = repository.Complete(context.Background(), claim, DurableJobResult{
		Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{},
	})
	if !errors.Is(err, ErrExternalJobUnavailable) {
		t.Fatalf("completion with rejected outbox error=%v", err)
	}
	settled, err := repository.Get(context.Background(), tenantID, job.Job.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != JobStatusRunning || mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox") != 0 {
		t.Fatalf("non-atomic terminal state=%s", settled.Status)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_job_attempt WHERE status = 'RUNNING'"); got != 1 {
		t.Fatalf("running attempts=%d want=1", got)
	}
}

func TestMySQLDuplicateTerminalInsertUsesAuthoritativeStableEvent(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("r", 26)
	bundleID := strings.Repeat("s", 26)
	callbackID := strings.Repeat("t", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-authoritative")
	if _, err := repository.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
		t.Fatal(err)
	}

	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	terminal, err := getExternalJobByInternalID(context.Background(), tx, job.Job.InternalID)
	if err != nil {
		t.Fatal(err)
	}
	var authoritativeID string
	if err := tx.QueryRow("SELECT event_id FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID).Scan(&authoritativeID); err != nil {
		t.Fatal(err)
	}
	gotID, err := repository.insertTerminalWebhookEvent(context.Background(), tx, time.Now().UTC().Add(time.Hour), terminal)
	if err != nil || gotID != authoritativeID {
		t.Fatalf("duplicate event id=%q want=%q error=%v", gotID, authoritativeID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?", job.Job.InternalID); got != 1 {
		t.Fatalf("authoritative webhook rows=%d want=1", got)
	}
}

func TestMySQLEventIDCollisionRetriesWithoutLosingTerminalTransition(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("u", 26)
	bundleID := strings.Repeat("v", 26)
	callbackID := strings.Repeat("w", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 4)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	first := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-collision-first")
	if _, err := repository.Cancel(context.Background(), tenantID, first.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	second := submitWebhookJob(t, repository, tenantID, bundleID, callbackID, "webhook-collision-second")

	collisionBytes := bytes.Repeat([]byte{0x61}, externalIDRandomBytes)
	retryBytes := bytes.Repeat([]byte{0x62}, externalIDRandomBytes)
	collisionID, err := generateExternalID(bytes.NewReader(collisionBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_webhook_outbox SET event_id = ? WHERE job_id = ?", collisionID, first.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	repository.MySQLJobRepository.random = bytes.NewReader(append(collisionBytes, retryBytes...))
	cancelled, err := repository.Cancel(context.Background(), tenantID, second.Job.ExternalID)
	if err != nil || cancelled.Status != JobStatusCancelled {
		t.Fatalf("event ID collision cancellation=%+v error=%v", cancelled, err)
	}
	if got := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox"); got != 2 {
		t.Fatalf("outbox rows=%d want=2", got)
	}
}

func TestMySQLSubmitRejectsEnabledCallbackWithIncompleteCipherMetadata(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("g", 26)
	bundleID := strings.Repeat("h", 26)
	callbackID := strings.Repeat("i", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	var constraintExists int
	if err := database.QueryRow(`
SELECT COUNT(*) FROM information_schema.table_constraints
WHERE table_schema = DATABASE() AND table_name = 't_external_callback'
  AND constraint_name = 'chk_external_callback_active_cipher'`).Scan(&constraintExists); err != nil {
		t.Fatal(err)
	}
	if constraintExists == 1 {
		if _, err := database.Exec("ALTER TABLE t_external_callback DROP CHECK chk_external_callback_active_cipher"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = database.Exec("UPDATE t_external_callback SET disabled_at = CURRENT_TIMESTAMP(3) WHERE external_id = ?", callbackID)
		_, _ = database.Exec(`ALTER TABLE t_external_callback
ADD CONSTRAINT chk_external_callback_active_cipher
CHECK (
    disabled_at IS NOT NULL OR
    (secret_nonce IS NOT NULL AND OCTET_LENGTH(secret_nonce) = 12 AND
     OCTET_LENGTH(secret_ciphertext) > 16 AND secret_key_version > 0)
)`)
	})
	if _, err := database.Exec(`
INSERT INTO t_external_callback(
    external_id, tenant_id, destination_url, allowed_host, allowed_port,
    secret_ciphertext, secret_nonce, secret_key_version
)
SELECT ?, id, 'https://callback.example.test/judge', 'callback.example.test', 443,
       X'0102', NULL, 1
FROM t_external_tenant WHERE external_id = ?`, callbackID, tenantID); err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	_, err := repository.Submit(context.Background(), tenantID, "incomplete-callback", JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"), CallbackID: callbackID,
	})
	if !errors.Is(err, ErrExternalJobInvalid) {
		t.Fatalf("incomplete callback submit error=%v", err)
	}
	for table, query := range map[string]string{
		"jobs":        "SELECT COUNT(*) FROM t_external_job",
		"sources":     "SELECT COUNT(*) FROM t_external_source_object",
		"idempotency": "SELECT COUNT(*) FROM t_external_idempotency",
	} {
		if got := mustCount(t, database, query); got != 0 {
			t.Fatalf("%s after rejected callback=%d want=0", table, got)
		}
	}
}

func TestMySQLSubmitRechecksCallbackAfterExternalAdmission(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID := strings.Repeat("x", 26)
	bundleID := strings.Repeat("y", 26)
	callbackID := strings.Repeat("z", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	admitted := make(chan struct{})
	release := make(chan struct{})
	submitResult := make(chan error, 1)
	go func() {
		_, err := repository.MySQLJobRepository.Submit(context.Background(), tenantID, "callback-lock-submit", JudgeJobRequest{
			BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"), CallbackID: callbackID,
		}, func(context.Context) error {
			close(admitted)
			<-release
			return nil
		})
		submitResult <- err
	}()
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("submission did not reach callback admission")
	}
	connection, err := database.Conn(context.Background())
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
		connection.Close()
		close(release)
		t.Fatal(err)
	}
	_, updateErr := connection.ExecContext(context.Background(),
		"UPDATE t_external_callback SET disabled_at = CURRENT_TIMESTAMP(3) WHERE external_id = ?", callbackID)
	connection.Close()
	close(release)
	if submitErr := <-submitResult; !errors.Is(submitErr, ErrExternalJobInvalid) {
		t.Fatalf("submit error=%v want invalid callback", submitErr)
	}
	if updateErr != nil {
		t.Fatalf("disable callback during external admission: %v", updateErr)
	}
	var disabledAt sql.NullTime
	if err := database.QueryRow("SELECT disabled_at FROM t_external_callback WHERE external_id = ?", callbackID).Scan(&disabledAt); err != nil {
		t.Fatal(err)
	}
	if !disabledAt.Valid {
		t.Fatal("callback disable did not commit before authoritative submission recheck")
	}
	for table, query := range map[string]string{
		"jobs":        "SELECT COUNT(*) FROM t_external_job",
		"sources":     "SELECT COUNT(*) FROM t_external_source_object",
		"idempotency": "SELECT COUNT(*) FROM t_external_idempotency",
	} {
		if got := mustCount(t, database, query); got != 0 {
			t.Fatalf("%s after callback race=%d want=0", table, got)
		}
	}
}

func submitWebhookJob(t *testing.T, repository *testMySQLJobRepository, tenantID, bundleID, callbackID, key string) SubmitJobResult {
	t.Helper()
	job, err := repository.Submit(context.Background(), tenantID, key, JudgeJobRequest{
		BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}"),
		CallbackID: callbackID, ClientReference: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func assertPendingTerminalEvent(t *testing.T, database interface {
	QueryRow(string, ...any) *sql.Row
}, jobExternalID, wantType string, wantStatus JobStatus) {
	t.Helper()
	var eventID, eventType, status string
	var payloadJSON, payloadBody []byte
	var attemptCount int
	var ttlSeconds int
	err := database.QueryRow(`
SELECT outbox.event_id, outbox.event_type, outbox.status, outbox.payload_json,
       outbox.payload_body, outbox.attempt_count,
       TIMESTAMPDIFF(SECOND, CURRENT_TIMESTAMP(3), outbox.expires_at)
FROM t_external_webhook_outbox AS outbox
JOIN t_external_job AS job ON job.id = outbox.job_id
WHERE job.external_id = ?`, jobExternalID).Scan(
		&eventID, &eventType, &status, &payloadJSON, &payloadBody, &attemptCount, &ttlSeconds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !externalIDPattern.MatchString(eventID) || eventType != wantType || status != "PENDING" || attemptCount != 0 {
		t.Fatalf("event id=%q type=%q status=%q attempts=%d", eventID, eventType, status, attemptCount)
	}
	if ttlSeconds < int((24*time.Hour)/time.Second)-10 || ttlSeconds > int((24*time.Hour)/time.Second) {
		t.Fatalf("delivery ttl seconds=%d", ttlSeconds)
	}
	var semanticPayload, exactPayload any
	if err := json.Unmarshal(payloadJSON, &semanticPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payloadBody, &exactPayload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(semanticPayload, exactPayload) {
		t.Fatalf("semantic and exact payload differ: json=%q body=%q", payloadJSON, payloadBody)
	}
	var payload terminalWebhookPayload
	if err := json.Unmarshal(payloadBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.EventID != eventID || payload.EventType != wantType || payload.JobID != jobExternalID || payload.Status != wantStatus {
		t.Fatalf("payload=%+v", payload)
	}
}
