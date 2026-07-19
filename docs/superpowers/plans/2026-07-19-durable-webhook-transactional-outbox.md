# Durable Webhook Transactional Outbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist every callback-enabled terminal judge event atomically with its job, then provision and deliver it securely with leased, fenced, at-least-once HTTP workers.

**Architecture:** MySQL stores immutable exact payload bytes and the delivery state machine. Job completion/failure/cancellation calls a transaction-scoped event writer; delivery workers claim with `SKIP LOCKED`, decrypt a callback-specific AES-GCM secret, use the existing SSRF-safe HMAC transport, and CAS-settle with the MySQL clock. Callback creation remains operator-only in `judge-admin`.

**Tech Stack:** Go 1.26, MySQL 8.4/InnoDB, `database/sql`, AES-256-GCM, HMAC-SHA256 webhook v1, `net/http`, GitHub Actions.

---

## File map

- `internal/external/migrations/005_durable_webhook_outbox.sql`: replay-safe schema upgrade and legacy callback disablement (renumbered after canonical execution ceilings became v4).
- `internal/external/callback_crypto.go`: canonical destination, callback secret material, AES-GCM key ring, environment decoder.
- `internal/external/provision.go`: callback provisioning and encrypted persistence.
- `internal/admincli/run.go`, `cmd/judge-admin/main.go`: operator-only `callback create` command and key-ring injection.
- `internal/external/webhook_event.go`: versioned redacted payload and transaction-scoped insert.
- `internal/external/mysql_webhook_outbox.go`: claim/reclaim, fenced settlement, retention.
- `internal/external/webhook_worker.go`: decrypt, deliver, retry, and run loop.
- `internal/external/webhook.go`, `internal/external/safe_transport.go`: structured outcome, bounded `Retry-After`, unsafe-DNS classification.
- Existing job repository files: atomic event creation on every terminal path and complete callback metadata admission.
- Tests adjacent to each unit; real MySQL cases remain in `internal/external/*_test.go`.

### Task 1: Replay-safe durable outbox migration

**Files:**
- Create: `internal/external/migrations/005_durable_webhook_outbox.sql`
- Modify: `internal/external/migrate_test.go`
- Modify: `internal/external/mysql_integration_test.go`
- Create: `docs/testing/durable-webhook-outbox-red-green.md`

- [ ] **Step 1: Write migration contract tests first**

Add a test that expects migration version 4 and checks the exact invariants:

```go
func TestDurableWebhookMigrationDefinesFencedOutboxAndDisablesLegacyCallbacks(t *testing.T) {
    migrations, err := Migrations()
    if err != nil { t.Fatal(err) }
    if len(migrations) != 4 || migrations[3].Version != 4 || migrations[3].Name != "durable_webhook_outbox" {
        t.Fatalf("migrations=%+v", migrations)
    }
    sql := strings.ToLower(migrations[3].SQL)
    for _, contract := range []string{
        "add column secret_nonce binary(12)",
        "set disabled_at = coalesce(disabled_at, current_timestamp(3))",
        "add column payload_body mediumblob",
        "add column worker_id varchar(128)",
        "add column lease_token binary(32)",
        "add column lease_until datetime(3)",
        "add column dead_at datetime(3)",
        "unique key uk_external_webhook_job (job_id)",
        "check (status in ('pending','delivering','delivered','dead'))",
    } {
        if !strings.Contains(sql, contract) { t.Errorf("missing %q", contract) }
    }
}
```

- [ ] **Step 2: Run RED and record it**

Run:

```bash
go test ./internal/external -run 'TestDurableWebhookMigrationDefinesFencedOutboxAndDisablesLegacyCallbacks|TestEmbeddedMigrations' -count=1
```

Expected: FAIL because migration 4 and its schema contracts do not exist. Record command, failure excerpt, and timestamp under Task 1 RED in the RED/GREEN document.

- [ ] **Step 3: Add the migration minimally**

Use split replay-safe statements to add nullable columns, atomically disable incomplete legacy callbacks, translate `FAILED` to `DEAD`, backfill `payload_body`, reject duplicate legacy `job_id` rows with `SIGNAL SQLSTATE '45000'`, add `UNIQUE(job_id)`, replace the delivery status check, and add active-lease/callback-cipher checks. Preserve `uk_external_webhook_event`.

The enabled-callback check must be equivalent to:

```sql
CHECK (
  disabled_at IS NOT NULL OR
  (secret_nonce IS NOT NULL AND OCTET_LENGTH(secret_nonce) = 12 AND
   OCTET_LENGTH(secret_ciphertext) > 16 AND secret_key_version > 0)
)
```

- [ ] **Step 4: Add real MySQL migration/replay tests**

Prepare schema through migration 3, insert one active legacy callback without nonce and one legacy outbox `FAILED` row, apply migration 4 twice, then assert the callback is disabled, status is `DEAD`, payload bytes equal the stored JSON representation, unique job constraint exists, and history contains version 4. Add a separate duplicate-job legacy fixture and assert migration 4 fails without deleting either event.

- [ ] **Step 5: Run GREEN**

Run:

```bash
go test ./internal/external -run 'Migration|Migrations' -count=1
```

Expected: PASS (MySQL tests SKIP without `JUDGE_TEST_MYSQL_DSN`). Record GREEN.

- [ ] **Step 6: Commit Task 1**

```bash
git add internal/external/migrations/005_durable_webhook_outbox.sql internal/external/migrate_test.go internal/external/mysql_integration_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): migrate durable outbox state"
```

### Task 2: Canonical callback destination and AES-GCM secret ring

**Files:**
- Create: `internal/external/callback_crypto.go`
- Create: `internal/external/callback_crypto_test.go`
- Modify: `internal/external/safe_transport.go`
- Modify: `internal/external/safe_transport_test.go`

- [ ] **Step 1: Write destination/AAD/rotation RED tests**

Tests define the wished-for API:

```go
destination, err := CanonicalCallbackDestination("https://OJ.Example.com:443/a/../hooks?b=2&a=1")
if err != nil { t.Fatal(err) }
if destination.URL != "https://oj.example.com:443/hooks?a=1&b=2" || destination.Host != "oj.example.com" || destination.Port != 443 {
    t.Fatalf("destination=%+v", destination)
}
cipher, _ := NewCallbackCipher(2, map[uint16][]byte{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32)}, deterministicReader)
encrypted, err := cipher.Encrypt(tenantID, callbackID, destination.URL, []byte("croj_whsec_secret"))
if err != nil { t.Fatal(err) }
if _, err := cipher.Decrypt(tenantID, callbackID, "https://oj.example.com:443/other?a=1&b=2", encrypted); err == nil {
    t.Fatal("path transplant decrypted")
}
```

Also test query ordering, escaped path normalization, rejection of userinfo/fragments/IP literals/non-HTTPS/invalid ports, decryption with historical key version, unknown version failure, ciphertext tampering, and redacted `String`/`GoString`.

- [ ] **Step 2: Run RED and record it**

```bash
go test ./internal/external -run 'TestCanonicalCallback|TestCallbackCipher' -count=1
```

Expected: build FAIL because the new API is undefined.

- [ ] **Step 3: Implement minimal crypto and canonicalization**

Define focused types:

```go
type CallbackDestination struct { URL, Host string; Port uint16 }
type EncryptedCallbackSecret struct { Ciphertext, Nonce []byte; KeyVersion uint16 }
type CallbackMaterial struct { CallbackID, Secret string }
type CallbackCipher struct { activeVersion uint16; keys map[uint16][]byte; random io.Reader }
```

Canonicalize scheme/host/effective port/path/query before encryption. Build length-prefixed AAD from tenant ID, callback ID, canonical URL, and version. Generate nonces with `io.ReadFull`, use only 32-byte AES keys, copy key material on construction, and redact material formatting.

- [ ] **Step 4: Add strict key-ring decoder tests and implementation**

Expose:

```go
func DecodeCallbackKeyRing(active string, encodedJSON string, random io.Reader) (*CallbackCipher, error)
```

Reject zero/nondecimal versions, duplicate JSON keys, unknown fields/shapes, non-base64 data, non-32-byte keys, missing active key, and trailing JSON. Do not include key bytes in errors.

- [ ] **Step 5: Run GREEN**

```bash
go test ./internal/external -run 'TestCanonicalCallback|TestCallbackCipher|TestDecodeCallbackKeyRing' -count=1
```

Expected: PASS. Record GREEN.

- [ ] **Step 6: Commit Task 2**

```bash
git add internal/external/callback_crypto.go internal/external/callback_crypto_test.go internal/external/safe_transport.go internal/external/safe_transport_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): encrypt callback-specific secrets"
```

### Task 3: Operator-only callback provisioning

**Files:**
- Modify: `internal/external/provision.go`
- Modify: `internal/external/provision_test.go`
- Modify: `internal/admincli/run.go`
- Modify: `internal/admincli/run_test.go`
- Modify: `cmd/judge-admin/main.go`
- Create: `cmd/judge-admin/main_test.go`

- [ ] **Step 1: Write Provisioner RED tests**

Add a resolver fake returning public addresses and assert:

```go
material, err := provisioner.CreateCallback(ctx, tenantID, "https://oj.example.com/hooks?version=1")
if err != nil { t.Fatal(err) }
if material.CallbackID == "" || !strings.HasPrefix(material.Secret, "croj_whsec_") { t.Fatalf("material=%+v", material) }
```

Capture the insert and assert it stores canonical URL, exact host/port, non-plaintext ciphertext, 12-byte nonce, and active key version. Add cases for missing/disabled tenant, private/mixed/empty DNS answers, invalid URL, RNG error, and DB failure; assert no error contains the secret.

- [ ] **Step 2: Run RED and record it**

```bash
go test ./internal/external -run 'TestProvisioner.*Callback' -count=1
```

Expected: build FAIL because `CreateCallback` is missing.

- [ ] **Step 3: Implement `CreateCallback`**

Extend `Provisioner` with callback cipher and resolver through backward-compatible options:

```go
func WithCallbackCipher(cipher *CallbackCipher) ProvisionerOption
func WithCallbackResolver(resolver callbackResolver) ProvisionerOption
func (p *Provisioner) CreateCallback(ctx context.Context, tenantID, rawURL string) (CallbackMaterial, error)
```

Canonicalize and publicly resolve before generating material; generate callback ID and 32 random secret bytes, encode `croj_whsec_` plus raw base64url, encrypt, clear temporary bytes, and perform one `INSERT ... SELECT` from an active tenant.

- [ ] **Step 4: Write admin CLI RED tests**

Extend the stub and call:

```go
err := Run(ctx, []string{"callback", "create", "--tenant", tenantID, "--url", "https://oj.example.com/hook"}, stub, pepper, &output)
```

Assert one provisioner call, exact URL, one callback ID, and exactly one occurrence of the secret plus “shown once”. Reject positional arguments and missing flags before provisioning.

- [ ] **Step 5: Implement CLI and conditional key loading**

Add `callback create` routing. In `cmd/judge-admin`, require/decode callback keys only when the first two arguments are `callback create`; tenant/API-key commands remain usable without callback keys. Pass the cipher through `WithCallbackCipher` and default public resolver.

- [ ] **Step 6: Run GREEN and commit**

```bash
go test ./internal/admincli ./cmd/judge-admin ./internal/external -run 'Callback|callback' -count=1
git add internal/external/provision.go internal/external/provision_test.go internal/admincli/run.go internal/admincli/run_test.go cmd/judge-admin/main.go cmd/judge-admin/main_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(admin): provision encrypted callbacks"
```

### Task 4: Stable redacted terminal event encoding

**Files:**
- Create: `internal/external/webhook_event.go`
- Create: `internal/external/webhook_event_test.go`

- [ ] **Step 1: Write exact-byte RED tests**

Create SUCCEEDED, FAILED, and CANCELLED inputs and assert byte-for-byte JSON. The completed case must match:

```go
want := `{"schemaVersion":1,"eventId":"ceirceirceirceirceirceirce","eventType":"judge.job.completed","occurredAt":"2026-07-19T12:34:56.789Z","tenantId":"tenanttenanttenanttenantte","jobId":"jobjobjobjobjobjobjobjobjobjo","clientReference":"submission-1","status":"SUCCEEDED","result":{"verdict":"ACCEPTED","compileStatus":"SUCCEEDED","timeMillis":8,"memoryBytes":1024,"cases":[]}}`
```

Assert FAILED omits result and includes only `failureCode`; CANCELLED omits both. Reject nonterminal status, invalid IDs, missing required result, oversized client reference, and invalid result.

- [ ] **Step 2: Run RED**

```bash
go test ./internal/external -run 'TestEncodeTerminalWebhook' -count=1
```

Expected: build FAIL because encoder types are missing.

- [ ] **Step 3: Implement a versioned concrete struct**

Expose only a transaction helper input type, not `map[string]any`:

```go
type TerminalWebhookEvent struct { EventID string; OccurredAt time.Time; Job ExternalJobRecord }
func EncodeTerminalWebhookEvent(event TerminalWebhookEvent) (eventType string, semanticJSON, exactBody []byte, err error)
```

Use UTC millisecond timestamps and standard struct field order; copy raw bytes before returning.

- [ ] **Step 4: Run GREEN and commit**

```bash
go test ./internal/external -run 'TestEncodeTerminalWebhook' -count=1
git add internal/external/webhook_event.go internal/external/webhook_event_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): encode stable terminal events"
```

### Task 5: Atomic terminal job/outbox writes and admission fail-closed

**Files:**
- Modify: `internal/external/mysql_job_repository.go`
- Modify: `internal/external/mysql_job_repository_test.go`
- Modify: `internal/external/mysql_job_worker.go`
- Modify: `internal/external/mysql_job_worker_test.go`
- Create: `internal/external/mysql_webhook_event.go`
- Create: `internal/external/mysql_webhook_event_test.go`

- [ ] **Step 1: Write MySQL RED integration cases**

For callback-enabled jobs, assert exactly one outbox row in the same observed terminal transaction for: queued cancel, Complete success, exhausted failure, Complete observing running cancellation, Fail observing cancellation, and expired-running cancellation recovery. For callback-less jobs assert zero. Force event insert failure with an invalid fixture and assert the job remains nonterminal.

- [ ] **Step 2: Add admission metadata RED case**

Insert an enabled callback with missing nonce/incomplete cipher metadata (temporarily disabling the migration check in the fixture if required) and assert `Submit` returns unavailable and inserts no source/job/idempotency record. Assert the callback lookup includes:

```sql
callback.disabled_at IS NULL
AND callback.secret_nonce IS NOT NULL
AND OCTET_LENGTH(callback.secret_nonce) = 12
AND OCTET_LENGTH(callback.secret_ciphertext) > 16
AND callback.secret_key_version > 0
```

- [ ] **Step 3: Run RED and record it**

```bash
go test ./internal/external -run 'TestMySQL.*(Outbox|CallbackMetadata|Terminal)' -count=1
```

Expected with MySQL configured: FAIL because terminal paths do not insert outbox rows. Without MySQL: SKIP; also run the SQL-shape unit test and observe FAIL.

- [ ] **Step 4: Implement transaction-scoped insertion**

Add:

```go
func (r *MySQLJobRepository) insertTerminalWebhook(ctx context.Context, tx *sql.Tx, now time.Time, job ExternalJobRecord) (string, error)
```

If callback ID is absent return empty. Generate event ID, encode once, insert `payload_json` and exact `payload_body`, and set `expires_at` from configured delivery window. On MySQL duplicate key for `uk_external_webhook_job`, lock/read the existing event and verify tenant/job/callback/type and semantic body before returning its event ID. Never swallow a mismatched duplicate.

- [ ] **Step 5: Call helper before every terminal commit**

Replace application-clock use in queued cancellation with `mysqlCurrentTime`. Invoke insertion after the terminal job update and before commit in all listed paths; do not invoke for requeue or running cancel intent. Preserve worker lease/attempt fencing and existing lock order.

- [ ] **Step 6: Run GREEN and commit**

```bash
go test ./internal/external -run 'TestMySQL.*(Outbox|CallbackMetadata|Terminal|Cancel|Complete|Failure)' -count=1
git add internal/external/mysql_job_repository.go internal/external/mysql_job_repository_test.go internal/external/mysql_job_worker.go internal/external/mysql_job_worker_test.go internal/external/mysql_webhook_event.go internal/external/mysql_webhook_event_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): commit terminal events atomically"
```

### Task 6: Structured HTTP outcome and bounded Retry-After

**Files:**
- Modify: `internal/external/webhook.go`
- Modify: `internal/external/webhook_test.go`
- Modify: `internal/external/safe_transport.go`
- Modify: `internal/external/safe_transport_test.go`

- [ ] **Step 1: Write classification RED tests**

Define:

```go
type WebhookOutcome struct { Disposition WebhookDisposition; HTTPStatus int; RetryAfter time.Duration; ErrorCode string }
func (d *WebhookDeliverer) DeliverOutcome(context.Context, WebhookDelivery, time.Duration) WebhookOutcome
```

Test all 2xx, 3xx, retryable 408/425/429/5xx, permanent remaining 4xx, retryable DNS/network failure, permanent private/mixed DNS and authority mismatch, valid delta/date Retry-After, invalid values, and clamping to 15 minutes. Assert response bodies never appear in outcome/error.

- [ ] **Step 2: Run RED**

```bash
go test ./internal/external -run 'TestWebhook.*(Outcome|RetryAfter|Unsafe)' -count=1
```

Expected: build FAIL because structured outcome is missing.

- [ ] **Step 3: Implement while keeping `Deliver` compatibility**

Have existing `Deliver` delegate to `DeliverOutcome`. Wrap prohibited DNS answers with `ErrUnsafeCallbackDestination`, but retain resolver outages as retryable network failures. Parse `Retry-After` only for retryable responses relative to injected response time and cap it at the supplied hard maximum (validated `<= 15m`). Keep exact HMAC v1 and `RoundTrip` redirect behavior unchanged.

- [ ] **Step 4: Run GREEN and commit**

```bash
go test ./internal/external -run 'TestWebhook|TestSafeCallback' -count=1
git add internal/external/webhook.go internal/external/webhook_test.go internal/external/safe_transport.go internal/external/safe_transport_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): classify bounded delivery outcomes"
```

### Task 7: Fenced MySQL outbox repository

**Files:**
- Create: `internal/external/mysql_webhook_outbox.go`
- Create: `internal/external/mysql_webhook_outbox_test.go`

- [ ] **Step 1: Write model and real MySQL RED tests**

Tests require:

```go
claim, err := repository.ClaimNextWebhook(ctx, "webhook-worker-1", time.Minute)
err = repository.SettleWebhook(ctx, claim, WebhookSettlement{Disposition: WebhookDelivered})
```

Cover FIFO due ordering across tenant heads, `SKIP LOCKED` concurrent distinct claims, PENDING→DELIVERING attempt increment, expired DELIVERING reclaim with new token and same event/body, no reclaim before expiry, stale token/attempt/worker rejection, DELIVERED/DEAD terminality, disabled callback DEAD settlement, retry schedule, and database clock immunity to a skewed application clock.

- [ ] **Step 2: Run RED**

```bash
go test ./internal/external -run 'TestMySQLWebhook' -count=1
```

Expected: build FAIL because repository API is absent.

- [ ] **Step 3: Implement claim and authoritative input**

Define redaction-safe claim/input types. In a `READ COMMITTED` transaction read `CURRENT_TIMESTAMP(3)`, select the oldest eligible tenant head then one row with `FOR UPDATE SKIP LOCKED`, expire/reclaim old DELIVERING ownership, generate a 32-byte token, increment attempt, and persist the lease. Return exact payload plus callback ciphertext/nonce/version and canonical destination.

- [ ] **Step 4: Implement fenced settlement**

Use a transaction and active-lease CAS. Delivered clears lease and sets DB `delivered_at`; retry clears lease and sets bounded `next_attempt_at`; permanent/exhausted/expired clears lease and sets `DEAD`/`dead_at`. Only store a validated HTTP status and an enum-like redacted error code.

- [ ] **Step 5: Implement retention**

Add:

```go
func (r *MySQLWebhookOutboxRepository) SweepTerminal(ctx context.Context, retention time.Duration, limit int) (int64, error)
```

Use DB time, terminal timestamps, bounded `1..1000` batches, and `FOR UPDATE SKIP LOCKED`; never delete PENDING/DELIVERING.

- [ ] **Step 6: Run GREEN and commit**

```bash
go test ./internal/external -run 'TestMySQLWebhook' -count=1
git add internal/external/mysql_webhook_outbox.go internal/external/mysql_webhook_outbox_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): lease durable outbox deliveries"
```

### Task 8: Delivery worker, jitter, and lifecycle

**Files:**
- Create: `internal/external/webhook_worker.go`
- Create: `internal/external/webhook_worker_test.go`

- [ ] **Step 1: Write retry arithmetic RED tests**

Use injected bytes to prove attempt 1 base delay, exponential growth, `[0.5,1.5]` jitter, saturation, 15-minute cap, max with Retry-After, 24-hour delivery deadline, 12-attempt exhaustion, and no negative duration.

- [ ] **Step 2: Write orchestration RED tests**

With fakes, prove claim→decrypt→deliver→settle, secret clearing after delivery, unsafe/decrypt failure→DEAD, network→PENDING, stale settlement ignored as ownership loss, cancellation interrupts HTTP, idle timer is context-aware, repository error exits, and expired claim is never delivered.

- [ ] **Step 3: Run RED**

```bash
go test ./internal/external -run 'TestWebhookWorker|TestWebhookBackoff' -count=1
```

Expected: build FAIL because worker is absent.

- [ ] **Step 4: Implement minimal worker**

Define validated config with lease > HTTP timeout, base 5s, max/hard cap 15m, maximum 12 attempts, idle delay, and retention. Reuse safe transports via a bounded authority cache; close idle connections on eviction and `Close`. Decrypt with full canonical destination AAD immediately before delivery and clear plaintext with `defer clear(secret)`.

- [ ] **Step 5: Run GREEN and commit**

```bash
go test ./internal/external -run 'TestWebhookWorker|TestWebhookBackoff' -count=1
git add internal/external/webhook_worker.go internal/external/webhook_worker_test.go docs/testing/durable-webhook-outbox-red-green.md
git commit -m "feat(webhook): deliver fenced outbox events"
```

### Task 9: MySQL 8.4 end-to-end webhook contract

**Files:**
- Modify: `internal/external/mysql_webhook_outbox_test.go`
- Create: `scripts/ci/test-webhook-mysql84-contract.sh`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Add RED integration scenario**

Provision tenant/callback, submit callback-enabled job, claim/complete it, claim webhook, deliver through a capturing transport, verify exact body and HMAC headers, settle delivered, then simulate a second job whose first remote call succeeds but DB settlement is skipped; advance lease, reclaim, and verify the same event ID/body is delivered again and stale settlement cannot win.

- [ ] **Step 2: Run RED on disposable MySQL 8.4**

Use an explicitly named temporary container and trap cleanup:

```bash
docker run --name croj-webhook-mysql84-test -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -e MYSQL_DATABASE=croj_webhook_test -p 13316:3306 -d mysql:8.4
JUDGE_TEST_MYSQL_DSN='root:@tcp(127.0.0.1:13316)/croj_webhook_test?parseTime=true&loc=UTC&charset=utf8mb4' go test -race -count=1 ./internal/external -run 'TestMySQLWebhook'
docker rm -f croj-webhook-mysql84-test
```

Expected initial failure: missing/incorrect end-to-end behavior, not container setup.

- [ ] **Step 3: Fix only integration-exposed behavior and rerun GREEN**

Repeat the exact command until PASS, then verify `docker ps -a --filter name=croj-webhook-mysql84-test` returns no container.

- [ ] **Step 4: Add CI contract**

Add a pinned MySQL 8.4 step using `JUDGE_TEST_MYSQL_DSN` and focused `TestMySQLWebhook` tests. The shell contract checks pinned image, DSN, race flag, focused selector, and timeout; validate it with `shellcheck` and execute it.

- [ ] **Step 5: Commit Task 9**

```bash
git add internal/external/mysql_webhook_outbox_test.go scripts/ci/test-webhook-mysql84-contract.sh .github/workflows/ci.yml docs/testing/durable-webhook-outbox-red-green.md
git commit -m "test(webhook): gate MySQL 8.4 outbox recovery"
```

### Task 10: Documentation and final gates

**Files:**
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `docs/testing/durable-webhook-outbox-red-green.md`

- [ ] **Step 1: Document deployment and receiver contract**

README must include `judge-admin callback create`, callback key-ring environment examples with non-secret placeholders, add-before-switch rotation, one-time secret custody, exact v1 signature input, signed event fields, deduplication by event ID, status/retry matrix, 15-minute Retry-After cap, 24-hour delivery window, DEAD audit, 30-day retention, MySQL migration, local disposable-container test commands, and the absence of a public callback-management API.

- [ ] **Step 2: Record truthful changelog and complete RED/GREEN evidence**

Add Unreleased entries for atomic terminal events, encrypted callback provisioning, fenced delivery, retry/DEAD semantics, migration compatibility, MySQL gate, and known at-least-once duplicate behavior. Ensure every task in the RED/GREEN document has a failing and passing command plus reason.

- [ ] **Step 3: Run focused and full gates**

```bash
gofmt -w internal/external/*.go internal/admincli/*.go cmd/judge-admin/*.go
go test ./internal/external ./internal/admincli ./cmd/judge-admin -count=1
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/croj-judging-server ./cmd
CGO_ENABLED=0 go build -trimpath -o /tmp/croj-judge-admin ./cmd/judge-admin
shellcheck scripts/ci/*.sh
docker build -t coderushoj/judging-server:webhook-ci .
```

Expected: every command PASS with no warnings. Remove `/tmp` binaries and the local image only if cleanup is explicitly safe; never remove unrelated images or containers.

- [ ] **Step 4: Run migration replay with disposable MySQL and clean it**

Run the Task 9 container command with all `internal/external` integration tests, then remove only `croj-webhook-mysql84-test`.

- [ ] **Step 5: Request independent review and fix all Critical/Important findings**

Review the range from `970a080` to HEAD for transaction atomicity, stale-worker fencing, retry classification, SSRF/DNS rebinding, secret handling, migration replay, cleanup, race safety, and test false greens. Any fix starts with a failing regression test.

- [ ] **Step 6: Commit final documentation/gate evidence**

```bash
git add README.md CHANGELOG.md docs/testing/durable-webhook-outbox-red-green.md
git commit -m "docs(webhook): document durable callback delivery"
```
