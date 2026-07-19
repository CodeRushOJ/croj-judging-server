package external

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMySQLWebhookEndToEndHMACAndAtLeastOnceRecovery(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)

	callbackCipher, err := NewCallbackCipher(1, map[uint16][]byte{
		1: bytes.Repeat([]byte{0x91}, 32),
	}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewProvisioner(database, rand.Reader,
		WithCallbackCipher(callbackCipher),
		WithCallbackResolver(callbackResolverStub{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := provisioner.CreateTenant(context.Background(), "Webhook contract", TenantPolicy{
		MaxQueuedJobs: 8, MaxRunningJobs: 1, MaxSourceBytes: 1 << 20,
		MaxRetainedBundles: 16, DailyExecutionMillis: 3_600_000, MaxInfrastructureTries: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundleID := strings.Repeat("b", 26)
	insertWebhookContractBundle(t, database, tenantID, bundleID)
	callback, err := provisioner.CreateCallback(context.Background(), tenantID, "https://webhook.example.test/judge")
	if err != nil {
		t.Fatal(err)
	}

	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	outbox := newTestMySQLWebhookOutboxRepository(t, database, 12)
	complete := func(idempotencyKey, workerID string) {
		t.Helper()
		submitted := submitWebhookJob(t, jobs, tenantID, bundleID, callback.CallbackID, idempotencyKey)
		claim, claimErr := jobs.ClaimNext(context.Background(), workerID, time.Minute)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if claim.Job.ExternalID != submitted.Job.ExternalID {
			t.Fatalf("claimed job=%s want=%s", claim.Job.ExternalID, submitted.Job.ExternalID)
		}
		if completeErr := jobs.Complete(context.Background(), claim, DurableJobResult{
			Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{},
		}); completeErr != nil {
			t.Fatal(completeErr)
		}
	}

	type capturedDelivery struct {
		eventID, timestamp, signature string
		body                          []byte
	}
	var deliveries []capturedDelivery
	deliverer, err := newWebhookDelivererForTest(webhookContractRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			return nil, readErr
		}
		deliveries = append(deliveries, capturedDelivery{
			eventID:   request.Header.Get("X-CodeRushOJ-Event-Id"),
			timestamp: request.Header.Get("X-CodeRushOJ-Timestamp"),
			signature: request.Header.Get("X-CodeRushOJ-Signature"),
			body:      append([]byte(nil), body...),
		})
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(claim WebhookClaim) {
		t.Helper()
		secret, decryptErr := callbackCipher.Decrypt(claim.TenantID, claim.CallbackID, claim.DestinationURL, claim.EncryptedSecret())
		if decryptErr != nil {
			t.Fatal(decryptErr)
		}
		defer clear(secret)
		if string(secret) != callback.Secret {
			t.Fatal("decrypted callback secret differs from one-time provisioning material")
		}
		outcome := deliverer.DeliverOutcome(context.Background(), WebhookDelivery{
			EventID: claim.EventID, DestinationURL: claim.DestinationURL, Secret: secret, Body: claim.Body,
		}, maximumWebhookRetryAfter)
		if outcome.Disposition != WebhookDelivered || outcome.HTTPStatus != http.StatusNoContent || outcome.err != nil {
			t.Fatalf("delivery outcome=%+v", outcome)
		}
	}
	assertSignedDelivery := func(index int, claim WebhookClaim) {
		t.Helper()
		got := deliveries[index]
		if got.eventID != claim.EventID || !bytes.Equal(got.body, claim.Body) {
			t.Fatalf("delivery[%d] event/body changed", index)
		}
		if _, parseErr := strconv.ParseInt(got.timestamp, 10, 64); parseErr != nil {
			t.Fatalf("delivery[%d] timestamp=%q is not a Unix second", index, got.timestamp)
		}
		if got.signature != webhookContractSignature([]byte(callback.Secret), got.eventID, got.timestamp, got.body) {
			t.Fatalf("delivery[%d] signature=%q is invalid", index, got.signature)
		}
	}

	complete("webhook-contract-first", "job-worker-first")
	first, err := outbox.ClaimNextWebhook(context.Background(), "callback-worker-first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deliver(first)
	assertSignedDelivery(0, first)
	if err := outbox.SettleWebhook(context.Background(), first, WebhookSettlement{
		Disposition: WebhookDelivered, HTTPStatus: http.StatusNoContent,
	}); err != nil {
		t.Fatal(err)
	}
	assertWebhookState(t, database, first.OutboxID, "DELIVERED", http.StatusNoContent, "", true, false)

	complete("webhook-contract-retry", "job-worker-retry")
	second, err := outbox.ClaimNextWebhook(context.Background(), "callback-worker-crashed", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deliver(second) // Remote accepted it, but the worker crashes before settlement.
	assertSignedDelivery(1, second)
	if _, err := database.Exec(`UPDATE t_external_webhook_outbox
SET lease_until = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?`, second.OutboxID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := outbox.ClaimNextWebhook(context.Background(), "callback-worker-recovery", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.EventID != second.EventID || !bytes.Equal(reclaimed.Body, second.Body) || reclaimed.AttemptCount != second.AttemptCount+1 {
		t.Fatalf("reclaimed delivery changed: first=%s reclaimed=%s", second, reclaimed)
	}
	deliver(reclaimed)
	assertSignedDelivery(2, reclaimed)
	if err := outbox.SettleWebhook(context.Background(), second, WebhookSettlement{
		Disposition: WebhookDelivered, HTTPStatus: http.StatusNoContent,
	}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("stale settlement error=%v", err)
	}
	if err := outbox.SettleWebhook(context.Background(), reclaimed, WebhookSettlement{
		Disposition: WebhookDelivered, HTTPStatus: http.StatusNoContent,
	}); err != nil {
		t.Fatal(err)
	}
	assertWebhookState(t, database, reclaimed.OutboxID, "DELIVERED", http.StatusNoContent, "", true, false)
	if len(deliveries) != 3 || deliveries[1].eventID != deliveries[2].eventID || !bytes.Equal(deliveries[1].body, deliveries[2].body) {
		t.Fatalf("at-least-once retry was not byte-stable: %#v", deliveries)
	}
}

type webhookContractRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper webhookContractRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

func webhookContractSignature(secret []byte, eventID, timestamp string, body []byte) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(digest, "v1\n%d\n%s\n%s\n", len(eventID), eventID, timestamp)
	_, _ = digest.Write(body)
	return "v1=" + hex.EncodeToString(digest.Sum(nil))
}

func insertWebhookContractBundle(t *testing.T, database *sql.DB, tenantID, bundleID string) {
	t.Helper()
	if _, err := database.Exec(`
INSERT INTO t_external_bundle(
    external_id, tenant_id, sha256, object_key, size_bytes, case_count,
    manifest_version, manifest_json, publication_status, ready_at
)
SELECT ?, tenant.id, UNHEX(SHA2(?, 256)), ?, 128, 1,
       1, JSON_OBJECT('schemaVersion', 1, 'cases', JSON_ARRAY()), 'READY', CURRENT_TIMESTAMP(3)
FROM t_external_tenant AS tenant WHERE tenant.external_id = ?`,
		bundleID, bundleID, "external/"+tenantID+"/sha256/"+bundleID+".zip", tenantID); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLWebhookRepositoryDefaultsMaximumAttemptsAndRedactsJSON(t *testing.T) {
	repository, err := NewMySQLWebhookOutboxRepository(MySQLWebhookOutboxRepositoryConfig{
		Database: &sql.DB{}, Random: rand.Reader,
	})
	if err != nil || repository.maximumAttempts != 12 {
		t.Fatalf("repository=%+v error=%v", repository, err)
	}
	claim := WebhookClaim{secret: EncryptedCallbackSecret{Ciphertext: []byte("secret-ciphertext"), Nonce: []byte("secret-nonce")}}
	encoded, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret")) || !bytes.Contains(encoded, []byte("REDACTED")) {
		t.Fatalf("claim JSON leaked: %s", encoded)
	}
}

func TestWebhookSettlementRejectsDispositionMatrixContradictions(t *testing.T) {
	retryAt := time.Now().Add(time.Minute)
	invalid := []WebhookSettlement{
		{Disposition: WebhookRetry, HTTPStatus: 201, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: retryAt},
		{Disposition: WebhookRetry, HTTPStatus: 400, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: retryAt},
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: retryAt},
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, RetryAt: retryAt, RetryDelay: time.Second},
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, RetryDelay: -time.Second},
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, RetryDelay: maximumWebhookRetryAfter + time.Millisecond},
		{Disposition: WebhookPermanentFailure, HTTPStatus: 503, ErrorCode: WebhookErrorHTTPPermanent},
		{Disposition: WebhookPermanentFailure, HTTPStatus: 400, ErrorCode: WebhookErrorNetwork},
	}
	for _, settlement := range invalid {
		if validWebhookSettlement(settlement) {
			t.Fatalf("matrix contradiction accepted: %+v", settlement)
		}
	}
	valid := []WebhookSettlement{
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, RetryAt: retryAt},
		{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, RetryDelay: time.Second},
		{Disposition: WebhookRetry, HTTPStatus: 429, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: retryAt},
		{Disposition: WebhookPermanentFailure, HTTPStatus: 301, ErrorCode: WebhookErrorHTTPPermanent},
		{Disposition: WebhookPermanentFailure, HTTPStatus: 400, ErrorCode: WebhookErrorHTTPPermanent},
		{Disposition: WebhookPermanentFailure, ErrorCode: WebhookErrorUnsafeDestination},
	}
	for _, settlement := range valid {
		if !validWebhookSettlement(settlement) {
			t.Fatalf("valid matrix rejected: %+v", settlement)
		}
	}
}

func TestMySQLWebhookClaimIsFIFOLeasedAndCrashRecoverable(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantA, tenantB := strings.Repeat("a", 26), strings.Repeat("b", 26)
	bundleA, bundleB := strings.Repeat("c", 26), strings.Repeat("d", 26)
	callbackA, callbackB := strings.Repeat("e", 26), strings.Repeat("f", 26)
	insertTenantBundleAndCallback(t, database, tenantA, bundleA, callbackA, 8)
	insertTenantBundleAndCallback(t, database, tenantB, bundleB, callbackB, 8)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	first := submitWebhookJob(t, jobs, tenantA, bundleA, callbackA, "webhook-fifo-first-01")
	second := submitWebhookJob(t, jobs, tenantA, bundleA, callbackA, "webhook-fifo-second-1")
	third := submitWebhookJob(t, jobs, tenantB, bundleB, callbackB, "webhook-fifo-third-01")
	for _, item := range []struct {
		tenant string
		job    SubmitJobResult
	}{{tenantA, first}, {tenantA, second}, {tenantB, third}} {
		if _, err := jobs.Cancel(context.Background(), item.tenant, item.job.Job.ExternalID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`
UPDATE t_external_webhook_outbox AS outbox
JOIN t_external_job AS job ON job.id = outbox.job_id
SET outbox.next_attempt_at = CASE job.external_id
    WHEN ? THEN CURRENT_TIMESTAMP(3) - INTERVAL 3 SECOND
    WHEN ? THEN CURRENT_TIMESTAMP(3) - INTERVAL 2 SECOND
    ELSE CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND END`, first.Job.ExternalID, third.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLWebhookOutboxRepository(t, database, 4)
	claim, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.EventID == "" || claim.EventType != "judge.job.cancelled" || claim.TenantID != tenantA || claim.CallbackID != callbackA ||
		claim.DestinationURL != "https://callback.example.test/judge" || claim.AllowedHost != "callback.example.test" || claim.AllowedPort != 443 ||
		len(claim.EncryptedSecret().Ciphertext) <= 16 || len(claim.EncryptedSecret().Nonce) != 12 || claim.EncryptedSecret().KeyVersion != 1 ||
		claim.AttemptCount != 1 || claim.WorkerID != "webhook-worker-1" || len(claim.LeaseToken) != 32 || len(claim.Body) == 0 {
		t.Fatalf("unexpected redacted claim: %s", claim)
	}
	if got := fmt.Sprintf("%#v", claim); strings.Contains(got, "010203") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("claim formatting leaked or failed to redact: %s", got)
	}
	var databaseNow time.Time
	if err := database.QueryRow("SELECT CURRENT_TIMESTAMP(3)").Scan(&databaseNow); err != nil {
		t.Fatal(err)
	}
	if claim.LeaseUntil.Before(databaseNow.Add(55*time.Second)) || claim.LeaseUntil.After(databaseNow.Add(65*time.Second)) {
		t.Fatalf("lease %v was not based on database clock %v", claim.LeaseUntil, databaseNow)
	}

	next, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.TenantID != tenantB {
		t.Fatalf("second tenant head was not selected: %s", next)
	}
	if _, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-3", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-4", time.Minute); !errors.Is(err, ErrWebhookNotAvailable) {
		t.Fatalf("live leases were reclaimed: %v", err)
	}

	stale := claim
	if _, err := database.Exec("UPDATE t_external_webhook_outbox SET lease_until = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?", claim.OutboxID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-5", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.EventID != stale.EventID || !bytes.Equal(reclaimed.Body, stale.Body) || reclaimed.AttemptCount != stale.AttemptCount+1 ||
		reclaimed.WorkerID == stale.WorkerID || bytes.Equal(reclaimed.LeaseToken, stale.LeaseToken) {
		t.Fatalf("invalid crash recovery old=%s new=%s", stale, reclaimed)
	}
	if err := repository.SettleWebhook(context.Background(), stale, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("stale claim settled: %v", err)
	}
	tamperedAttempt := reclaimed
	tamperedAttempt.AttemptCount++
	if err := repository.SettleWebhook(context.Background(), tamperedAttempt, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("tampered attempt settled: %v", err)
	}
	tamperedWorker := reclaimed
	tamperedWorker.WorkerID = "webhook-worker-other"
	if err := repository.SettleWebhook(context.Background(), tamperedWorker, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("tampered worker settled: %v", err)
	}
	tamperedToken := reclaimed
	tamperedToken.LeaseToken = bytes.Repeat([]byte{0x7f}, 32)
	if err := repository.SettleWebhook(context.Background(), tamperedToken, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("tampered token settled: %v", err)
	}
	if err := repository.SettleWebhook(context.Background(), reclaimed, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); err != nil {
		t.Fatal(err)
	}
	assertWebhookState(t, database, reclaimed.OutboxID, "DELIVERED", 204, "", true, false)
	if err := repository.SettleWebhook(context.Background(), reclaimed, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("terminal delivery was mutable: %v", err)
	}
}

func TestMySQLWebhookClaimUsesSkipLockedAcrossTenantHeads(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	var firstTenant, secondTenant string
	for index, id := range []byte{'g', 'h'} {
		tenantID := strings.Repeat(string(id), 26)
		if index == 0 {
			firstTenant = tenantID
		} else {
			secondTenant = tenantID
		}
		bundleID := strings.Repeat(string(id+2), 26)
		callbackID := strings.Repeat(string(id+4), 26)
		insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 4)
		job := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, fmt.Sprintf("webhook-skip-locked-%02d", index))
		if _, err := jobs.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
			t.Fatal(err)
		}
	}
	secondSameTenant := submitWebhookJob(t, jobs, firstTenant, strings.Repeat("i", 26), strings.Repeat("k", 26), "webhook-skip-same-tenant")
	if _, err := jobs.Cancel(context.Background(), firstTenant, secondSameTenant.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
UPDATE t_external_webhook_outbox AS outbox
JOIN t_external_job AS job ON job.id = outbox.job_id
SET outbox.next_attempt_at = CASE
    WHEN outbox.tenant_id = (SELECT id FROM t_external_tenant WHERE external_id = ?) AND job.external_id <> ?
        THEN CURRENT_TIMESTAMP(3) - INTERVAL 3 SECOND
    WHEN job.external_id = ? THEN CURRENT_TIMESTAMP(3) - INTERVAL 2 SECOND
    ELSE CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND END`, firstTenant, secondSameTenant.Job.ExternalID, secondSameTenant.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	var lockedID uint64
	locker, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if err := locker.QueryRow("SELECT id FROM t_external_webhook_outbox ORDER BY id LIMIT 1 FOR UPDATE").Scan(&lockedID); err != nil {
		_ = locker.Rollback()
		t.Fatal(err)
	}
	repository := newTestMySQLWebhookOutboxRepository(t, database, 4)
	claim, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-skip", time.Minute)
	if err != nil {
		_ = locker.Rollback()
		t.Fatal(err)
	}
	if claim.OutboxID == lockedID {
		_ = locker.Rollback()
		t.Fatal("claim did not skip the locked tenant head")
	}
	if claim.TenantID != secondTenant {
		_ = locker.Rollback()
		t.Fatalf("locked tenant head leaked later same-tenant row: claim=%s tenant=%s", claim, claim.TenantID)
	}
	if err := locker.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLWebhookRetryAndTerminalDeadRules(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID, callbackID := strings.Repeat("m", 26), strings.Repeat("n", 26), strings.Repeat("o", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 12)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	repository := newTestMySQLWebhookOutboxRepository(t, database, 2)

	retryJob := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, "webhook-retry-rules-1")
	if _, err := jobs.Cancel(context.Background(), tenantID, retryJob.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-retry", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC().Add(2 * time.Minute)
	if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{
		Disposition: WebhookRetry, HTTPStatus: 503, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: retryAt,
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	var scheduled time.Time
	if err := database.QueryRow("SELECT status, next_attempt_at FROM t_external_webhook_outbox WHERE id = ?", claim.OutboxID).Scan(&status, &scheduled); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || scheduled.Before(retryAt.Add(-time.Second)) || scheduled.After(retryAt.Add(time.Second)) {
		t.Fatalf("retry status=%s next=%v want=%v", status, scheduled, retryAt)
	}
	if _, err := database.Exec("UPDATE t_external_webhook_outbox SET next_attempt_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?", claim.OutboxID); err != nil {
		t.Fatal(err)
	}
	second, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-retry-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SettleWebhook(context.Background(), second, WebhookSettlement{
		Disposition: WebhookRetry, HTTPStatus: 429, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	assertWebhookState(t, database, second.OutboxID, "DEAD", 429, WebhookErrorHTTPRetryable, false, true)

	disabledJob := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, "webhook-disabled-rule")
	if _, err := jobs.Cancel(context.Background(), tenantID, disabledJob.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE t_external_callback SET disabled_at = CURRENT_TIMESTAMP(3) WHERE external_id = ?", callbackID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-disabled", time.Minute); !errors.Is(err, ErrWebhookNotAvailable) {
		t.Fatalf("disabled callback claim error=%v", err)
	}
	assertWebhookStateByJob(t, database, disabledJob.Job.ExternalID, "DEAD", "callback_disabled")

	if _, err := database.Exec("UPDATE t_external_callback SET disabled_at = NULL WHERE external_id = ?", callbackID); err != nil {
		t.Fatal(err)
	}
	expiredJob := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, "webhook-expired-rule-1")
	if _, err := jobs.Cancel(context.Background(), tenantID, expiredJob.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_webhook_outbox AS outbox JOIN t_external_job AS job ON job.id=outbox.job_id
SET outbox.expires_at=CURRENT_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE job.external_id=?`, expiredJob.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-expired", time.Minute); !errors.Is(err, ErrWebhookNotAvailable) {
		t.Fatalf("expired event claim error=%v", err)
	}
	assertWebhookStateByJob(t, database, expiredJob.Job.ExternalID, "DEAD", "delivery_expired")
}

func TestMySQLWebhookClaimKeepsFullLeaseAndAllowsSuccessAfterDeliveryExpiry(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID, callbackID := strings.Repeat("2", 26), strings.Repeat("3", 26), strings.Repeat("4", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 2)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, "webhook-short-window")
	if _, err := jobs.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_webhook_outbox
SET expires_at = CURRENT_TIMESTAMP(3) + INTERVAL 30 SECOND WHERE job_id = ?`, job.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLWebhookOutboxRepository(t, database, 4)
	claim, err := repository.ClaimNextWebhook(context.Background(), "short-window-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.LeaseUntil.After(claim.ExpiresAt) {
		t.Fatalf("lease=%v was truncated to delivery expiry=%v", claim.LeaseUntil, claim.ExpiresAt)
	}
	if _, err := database.Exec("UPDATE t_external_webhook_outbox SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE id = ?", claim.OutboxID); err != nil {
		t.Fatal(err)
	}
	if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); err != nil {
		t.Fatalf("successful in-flight delivery after event expiry: %v", err)
	}
	assertWebhookState(t, database, claim.OutboxID, "DELIVERED", 204, "", true, false)
}

func TestMySQLWebhookSettlementRejectsUnredactedOrInvalidAuditValues(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID, callbackID := strings.Repeat("p", 26), strings.Repeat("q", 26), strings.Repeat("r", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 4)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	job := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, "webhook-invalid-audit-1")
	if _, err := jobs.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	repository := newTestMySQLWebhookOutboxRepository(t, database, 4)
	claim, err := repository.ClaimNextWebhook(context.Background(), "webhook-worker-audit", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	invalid := []WebhookSettlement{
		{Disposition: WebhookDelivered, HTTPStatus: 199},
		{Disposition: WebhookRetry, HTTPStatus: 700, ErrorCode: WebhookErrorNetwork, RetryAt: time.Now().Add(time.Minute)},
		{Disposition: WebhookPermanentFailure, HTTPStatus: 400, ErrorCode: "password=secret"},
		{Disposition: WebhookRetry, HTTPStatus: 503, ErrorCode: WebhookErrorHTTPRetryable, RetryAt: time.Now().Add(-time.Minute)},
	}
	for _, settlement := range invalid {
		if err := repository.SettleWebhook(context.Background(), claim, settlement); !errors.Is(err, ErrWebhookSettlementInvalid) {
			t.Fatalf("invalid settlement %+v error=%v", settlement, err)
		}
	}
	if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{
		Disposition: WebhookPermanentFailure, HTTPStatus: 400, ErrorCode: WebhookErrorHTTPPermanent,
	}); err != nil {
		t.Fatal(err)
	}
	assertWebhookState(t, database, claim.OutboxID, "DEAD", 400, WebhookErrorHTTPPermanent, false, true)
}

func TestMySQLWebhookSweepTerminalUsesDatabaseClockAndSkipsActiveRows(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID, callbackID := strings.Repeat("s", 26), strings.Repeat("t", 26), strings.Repeat("u", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, callbackID, 8)
	jobs := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	repository := newTestMySQLWebhookOutboxRepository(t, database, 4)
	ids := make([]uint64, 0, 4)
	for index := 0; index < 4; index++ {
		job := submitWebhookJob(t, jobs, tenantID, bundleID, callbackID, fmt.Sprintf("webhook-retention-%04d", index))
		if _, err := jobs.Cancel(context.Background(), tenantID, job.Job.ExternalID); err != nil {
			t.Fatal(err)
		}
		claim, err := repository.ClaimNextWebhook(context.Background(), fmt.Sprintf("webhook-sweep-%d", index), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, claim.OutboxID)
		switch index {
		case 0:
			if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); err != nil {
				t.Fatal(err)
			}
			_, _ = database.Exec("UPDATE t_external_webhook_outbox SET delivered_at=CURRENT_TIMESTAMP(3)-INTERVAL 31 DAY WHERE id=?", claim.OutboxID)
		case 1:
			if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{Disposition: WebhookPermanentFailure, HTTPStatus: 400, ErrorCode: WebhookErrorHTTPPermanent}); err != nil {
				t.Fatal(err)
			}
			_, _ = database.Exec("UPDATE t_external_webhook_outbox SET dead_at=CURRENT_TIMESTAMP(3)-INTERVAL 31 DAY WHERE id=?", claim.OutboxID)
		case 2:
			if err := repository.SettleWebhook(context.Background(), claim, WebhookSettlement{Disposition: WebhookDelivered, HTTPStatus: 204}); err != nil {
				t.Fatal(err)
			}
		default:
			// Keep DELIVERING; retention must not remove active work.
		}
	}
	if _, err := repository.SweepTerminal(context.Background(), 30*24*time.Hour, 0); !errors.Is(err, ErrWebhookSettlementInvalid) {
		t.Fatalf("invalid limit error=%v", err)
	}
	deleted, err := repository.SweepTerminal(context.Background(), 30*24*time.Hour, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("first sweep deleted=%d error=%v", deleted, err)
	}
	deleted, err = repository.SweepTerminal(context.Background(), 30*24*time.Hour, 1000)
	if err != nil || deleted != 1 {
		t.Fatalf("second sweep deleted=%d error=%v", deleted, err)
	}
	if count := mustCount(t, database, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE id IN (?, ?)", ids[2], ids[3]); count != 2 {
		t.Fatalf("recent/active rows were swept: %d", count)
	}
}

func newTestMySQLWebhookOutboxRepository(t *testing.T, database *sql.DB, maximumAttempts uint) *MySQLWebhookOutboxRepository {
	t.Helper()
	repository, err := NewMySQLWebhookOutboxRepository(MySQLWebhookOutboxRepositoryConfig{
		Database: database, Random: rand.Reader, MaximumAttempts: maximumAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func assertWebhookState(t *testing.T, database *sql.DB, id uint64, status string, httpStatus int, errorCode string, delivered, dead bool) {
	t.Helper()
	var gotStatus string
	var gotHTTP sql.NullInt64
	var gotError sql.NullString
	var deliveredAt, deadAt sql.NullTime
	var worker sql.NullString
	var token []byte
	var lease sql.NullTime
	if err := database.QueryRow(`SELECT status,last_http_status,last_error_code,delivered_at,dead_at,worker_id,lease_token,lease_until
FROM t_external_webhook_outbox WHERE id=?`, id).Scan(&gotStatus, &gotHTTP, &gotError, &deliveredAt, &deadAt, &worker, &token, &lease); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || int(gotHTTP.Int64) != httpStatus || gotError.String != errorCode || deliveredAt.Valid != delivered || deadAt.Valid != dead || worker.Valid || token != nil || lease.Valid {
		t.Fatalf("state=%s http=%v error=%v delivered=%v dead=%v worker=%v token=%x lease=%v", gotStatus, gotHTTP, gotError, deliveredAt, deadAt, worker, token, lease)
	}
}

func assertWebhookStateByJob(t *testing.T, database *sql.DB, jobExternalID, status, errorCode string) {
	t.Helper()
	var gotStatus string
	var gotError sql.NullString
	var deadAt sql.NullTime
	if err := database.QueryRow(`SELECT outbox.status,outbox.last_error_code,outbox.dead_at
FROM t_external_webhook_outbox AS outbox JOIN t_external_job AS job ON job.id=outbox.job_id WHERE job.external_id=?`, jobExternalID).Scan(&gotStatus, &gotError, &deadAt); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotError.String != errorCode || !deadAt.Valid {
		t.Fatalf("job webhook status=%s error=%s dead=%v", gotStatus, gotError.String, deadAt)
	}
}
