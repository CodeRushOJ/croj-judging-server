# Durable Webhook Transactional Outbox Design

Date: 2026-07-19  
Status: approved

## 1. Scope and completion definition

This slice completes webhook delivery for asynchronous external judge jobs. A job terminal transition and, when the job has an enabled callback, exactly one immutable webhook event are committed in the same MySQL transaction. Separate workers deliver committed events at least once through the existing HTTPS-only, DNS-revalidating, redirect-free transport and HMAC v1 signer.

The slice includes callback provisioning through `judge-admin callback create`, encrypted callback-specific secrets, replay-safe schema upgrade, claim/reclaim and retry persistence, a runnable delivery worker, MySQL 8.4 integration coverage, CI, README, and CHANGELOG updates. It does not add a public callback-management API, alter the external job HTTP resource contract, or start a persistent local service.

## 2. Considered approaches

### 2.1 Deliver inside the job transaction

Calling the remote callback before committing the job would appear atomic, but it would hold InnoDB locks across DNS, TLS, and arbitrary remote latency. A timeout cannot distinguish “remote accepted, response lost” from “remote did not accept,” and rollback after a successful remote call cannot retract the webhook. This approach is rejected.

### 2.2 Publish to an external broker after commit

Publishing to RocketMQ or Redis after the job commit has a dual-write gap; publishing before commit exposes uncommitted state. A broker transaction or CDC platform would solve that gap but adds an operational dependency and does not fit the service-owned MySQL durability model. This approach is deferred.

### 2.3 MySQL transactional outbox with leased HTTP workers

The selected approach writes the terminal job state and immutable event in one InnoDB transaction. Workers claim committed rows with `FOR UPDATE SKIP LOCKED`, deliver outside the transaction, then settle using lease-token and attempt fencing. A crash after remote acceptance but before settlement causes the same event to be delivered again after lease expiry; consumers deduplicate the signed stable `eventId`. This is the intended at-least-once contract.

## 3. Data model and replay-safe migration

Migration `005_durable_webhook_outbox.sql` follows the canonical tenant-policy v4 migration and upgrades the existing early outbox table rather than replacing it. MySQL DDL can commit statement by statement, so every additive or constraint operation is a separate `-- migrate:split` statement with the existing replay-error annotations for duplicate columns, indexes, or constraints.

The upgraded row contains:

- immutable identity and ownership: `event_id`, `tenant_id`, `job_id`, `callback_id`, and `event_type`;
- immutable content: semantic `payload_json` plus `payload_body` containing the exact UTF-8 bytes signed and retransmitted on every attempt;
- state: `PENDING`, `DELIVERING`, `DELIVERED`, or `DEAD`;
- fencing: `worker_id`, 256-bit `lease_token`, `lease_until`, and monotonically increasing `attempt_count`;
- schedule and audit: `next_attempt_at`, `last_http_status`, stable redacted `last_error_code`, `created_at`, `delivered_at`, `dead_at`, and `expires_at`. Here `expires_at` is the delivery deadline, not the retention deadline.

The database retains the existing unique constraint on `event_id` and adds `UNIQUE(job_id)`, enforcing at most one terminal event for each job independently of application retries. Migration first detects duplicate legacy job rows and fails with an actionable error rather than deleting or silently choosing an event; only a duplicate-free table receives the unique constraint. New event insertion treats a duplicate job key as evidence of an authoritative existing event and reads that row under the same transaction.

Legacy `FAILED` rows become `DEAD`. Existing JSON payloads are copied once into `payload_body` before the new non-null constraint is applied. Non-delivering rows have no worker or lease fields; `DELIVERING` rows require all three. The claim index includes status, due/lease time, tenant, and ID. The migration preserves existing event IDs and payloads and is verified by applying all migrations twice against disposable MySQL 8.4.

`t_external_callback` gains a nullable 12-byte AES-GCM nonce so legacy rows remain representable. In the same replay-safe migration sequence, every legacy row with a missing nonce or incomplete cipher metadata is atomically assigned `disabled_at = COALESCE(disabled_at, CURRENT_TIMESTAMP(3))`; the migration never invents a key or plaintext secret. A check constraint requires every enabled row to have a 12-byte nonce, non-empty ciphertext, and positive key version. Job submission repeats that complete-metadata predicate, so no new job can reference a callback the worker cannot decrypt. No secret-bearing column is exposed by the HTTP API.

## 4. Terminal event transaction

All terminal paths use the MySQL clock obtained inside their current transaction:

- successful `Complete`, including contestant verdicts;
- terminal `FailInfrastructure` after retry exhaustion;
- cancellation observed while completing or failing a running attempt;
- immediate cancellation of a queued job;
- cancellation finalized while reclaiming an expired running attempt.

If `callback_id` is null, the transaction writes only the job terminal state. Otherwise it inserts one terminal event before commit. Job-row locking serializes terminal transitions, terminal idempotence prevents a second event, and `UNIQUE(job_id)` is the final database invariant. The insert helper treats a duplicate-job-key conflict as recovery, reads the existing row, verifies matching tenant/callback/event type and payload semantics, and returns that authoritative event rather than generating a second logical notification.

The event ID is a cryptographically random 26-character external ID generated before insertion. Its body is encoded once from a versioned concrete struct and stored as exact bytes. Timestamps come from the transaction's MySQL clock and are rendered as UTC RFC3339 with millisecond precision. The v1 body contains only:

- `schemaVersion: 1`, `eventId`, `eventType`, and `occurredAt`;
- `tenantId`, `jobId`, and optional `clientReference`;
- terminal job `status`;
- the same redacted durable result returned by job GET for `SUCCEEDED`, or the stable infrastructure failure code for `FAILED`;
- no result or failure detail for `CANCELLED`.

Event types are `judge.job.completed`, `judge.job.failed`, and `judge.job.cancelled`. Source, hidden cases, expected output, object keys, worker IDs, lease tokens, callback destination, and secrets are never serialized. A transaction error rolls back both terminal state and outbox insert; an outcome-ambiguous commit is resolved by reading authoritative job/outbox state, never by sending before commit.

A completed payload has this shape; `result` is omitted for failed/cancelled events and `failureCode` is present only for failed events:

```json
{
  "schemaVersion": 1,
  "eventId": "abcdefghijklmnopqrstuvwxyz",
  "eventType": "judge.job.completed",
  "occurredAt": "2026-07-19T12:34:56.789Z",
  "tenantId": "abcdefghijklmnopqrstuvwxyz",
  "jobId": "abcdefghijklmnopqrstuvwxyz",
  "clientReference": "submission-123",
  "status": "SUCCEEDED",
  "result": {
    "verdict": "ACCEPTED",
    "compileStatus": "SUCCEEDED",
    "timeMillis": 8,
    "memoryBytes": 1048576,
    "cases": [{"caseId":"sample-1","verdict":"ACCEPTED","timeMillis":8,"memoryBytes":1048576}]
  }
}
```

## 5. Callback provisioning and secret encryption

Operators provision callbacks only through:

```text
judge-admin callback create --tenant <tenant-id> --url https://oj.example.com[:port]/path
```

The URL must be absolute HTTPS, contain no userinfo or fragment, use an ASCII lowercase DNS name rather than an IP literal, and have a numeric port in `1..65535` (default 443). The normalized hostname and effective port are persisted as the immutable allow-authority used by the safe transport. Provisioning resolves the hostname and rejects empty, mixed public/private, private, loopback, link-local, documentation, multicast, and metadata-class results. Delivery repeats this resolution on every connection, so later DNS rebinding fails closed. Redirect responses are never followed and become terminal delivery failures.

Provisioning generates a callback-specific 256-bit secret rendered as `croj_whsec_<base64url>`. The exact rendered bytes are encrypted with AES-256-GCM under a callback key ring and a versioned active key. Associated data binds tenant ID, callback ID, key version, and the complete canonical destination URL: lowercase `https` scheme and DNS host, explicit effective port, normalized escaped path, and canonical encoded query. This prevents ciphertext from being transplanted to another path or query on the same authority. The database stores only ciphertext, nonce, and key version. The CLI prints callback ID and plaintext secret once; secret-holding types redact `String`/`GoString`, temporary byte slices are cleared after encryption/decryption, and errors and logs never contain plaintext.

The admin and runtime binaries read `JUDGE_CALLBACK_KEY_VERSION` plus `JUDGE_CALLBACK_KEYS_JSON`, a JSON object mapping decimal versions to base64-encoded 32-byte AES keys, from environment/Kubernetes Secret configuration. These values are independent of the API-key pepper and source-encryption key. Encryption always uses the configured active version; decryption selects the row's stored version. Rotation is add-before-switch: deploy a ring containing old and new keys, switch the active version, recreate callbacks or run a future rewrap operation, and remove an old key only after no callback row references it. This slice deliberately does not perform automatic re-encryption. Missing historical keys fail closed to `DEAD`; key material is never stored in MySQL, output, or logs.

## 6. Claim, delivery, and recovery

Claiming is a short `READ COMMITTED` transaction. It reads the database clock, selects an eligible `PENDING` row whose `next_attempt_at` is due or an expired `DELIVERING` row, and uses `FOR UPDATE SKIP LOCKED` so a slow or crashed worker does not block peers. Ordering by due time and ID provides deterministic FIFO fairness; selection considers the oldest eligible head for each tenant before choosing the global oldest head, preventing one tenant's locked row from starving other tenants. Disabled tenants and disabled callbacks are settled `DEAD` rather than repeatedly selected.

The claim increments `attempt_count`, writes worker ID, a fresh 256-bit token, and a bounded lease based on the MySQL clock, then commits. Callback destination and encrypted secret metadata are loaded only for that fenced claim. Delivery occurs outside the transaction. The request timeout must be shorter than the lease.

Settlement uses `(outbox_id, attempt_count, worker_id, lease_token, DELIVERING, unexpired lease)` as the compare-and-set predicate and again obtains MySQL time in the transaction:

- any `2xx` becomes `DELIVERED`, with `delivered_at` and cleared lease fields;
- `408`, `425`, `429`, `5xx`, and network failures return to `PENDING` with a future schedule;
- other `4xx`, any `3xx`, invalid destination/authority, SSRF rejection, decryption/authentication failure, expiry, or exhausted attempts become `DEAD`, with `dead_at` and a redacted stable error code.

An expired `DELIVERING` lease is reclaimable. If the previous HTTP request succeeded but its settlement did not commit, the same event ID and exact body are resent. Stale workers cannot settle after a newer claim. This is at least once, not exactly once.

## 7. Retry timing

Retry delay is exponential from a configured positive base (default 5 seconds), capped at a configured maximum (default and hard upper bound 15 minutes), with injectable cryptographic jitter in the range `[0.5, 1.5]`. Arithmetic saturates before overflow. A valid HTTP `Retry-After` delta-seconds or IMF-fixdate is interpreted relative to the delivery response time, then clamped to 15 minutes; the schedule uses the later of jittered exponential delay and bounded `Retry-After`. Invalid, negative, or overflowing values are ignored.

New events have a configurable delivery window defaulting to 24 hours and a maximum of 7 days, and a configured maximum of 12 delivery attempts. The next attempt never exceeds event expiry. If no useful retry window remains, or `attempt_count` reaches the configured maximum, settlement is `DEAD`. HTTP response bodies are drained only to the existing small limit and are never stored as error text.

## 8. Component boundaries

- `CallbackCipher` owns callback AES-GCM and redaction-safe material types.
- `Provisioner.CreateCallback` owns URL/authority validation, public-DNS admission, secret creation, encryption, and the single callback insert.
- `MySQLWebhookOutboxRepository` owns transactional event insertion, claim/reclaim, authoritative input loading, and fenced settlement.
- `WebhookDeliverer` remains the HTTP/HMAC/security boundary and gains a structured outcome carrying disposition, status, and bounded `Retry-After` without exposing response bodies.
- `WebhookWorker` owns decrypt-deliver-settle orchestration and retry calculation. It never receives raw SQL handles. Safe transports are reused only within a bounded authority-keyed cache; eviction and worker shutdown close idle connections.
- Existing job repository methods call a small transaction-scoped event helper; they never call HTTP.

The worker loop returns repository failures to its supervisor rather than silently spinning, idles with a context-aware timer when no event is claimable, and clears decrypted secret bytes after every attempt.

## 9. Verification and delivery gates

Strict RED/GREEN records are retained in `docs/testing/durable-webhook-outbox-red-green.md`. Unit tests cover exact payload stability, HMAC headers, status matrix, `Retry-After`, retry overflow bounds, secret redaction/encryption/AAD/key rotation, URL normalization, CLI one-time output, cancellation event creation, and stale settlement rejection.

Real MySQL 8.4 integration tests prove:

- replay-safe migration from the legacy outbox shape;
- job terminal state and outbox insertion are atomic for completion, exhausted failure, queued cancellation, and recovered running cancellation;
- jobs without callbacks create no event;
- concurrent claim uses `SKIP LOCKED`, deterministic ordering, and does not duplicate a live claim;
- an expired delivery is reclaimed with a new token/attempt and the same event ID/body;
- stale delivery settlement cannot overwrite a newer attempt;
- delivered and dead rows are terminal;
- callback ciphertext decrypts only under the correct tenant/callback/complete canonical destination/key version.

Terminal `DELIVERED` and `DEAD` rows are retained for 30 days by default for audit and deduplication. A database-clock sweeper deletes only terminal rows whose `delivered_at`/`dead_at` is older than the configured retention, in bounded `SKIP LOCKED` batches. `PENDING` and `DELIVERING` rows are never deleted by retention; expired active rows are first settled `DEAD`. Retention is therefore independent of the 24-hour default delivery window.

Focused tests use `httptest` transports without starting a persistent server. Final gates are `go test -race ./...`, `go vet ./...`, static builds of the server and `judge-admin`, the MySQL 8.4 integration target, migration replay, and the existing container build. CI receives a dedicated MySQL webhook contract step. Temporary containers are named explicitly and removed after the gate.

## 10. Operational documentation and compatibility

README documents callback creation, secret custody, receiver signature verification, idempotent event handling, status/retry semantics, required runtime/admin secrets, migration, and local Docker/MySQL commands. CHANGELOG records the actual transactional and security behavior.

The public REST surface remains unchanged. The schema migration upgrades the pre-existing unused outbox table in place; it does not rewrite historical job results into synthetic callbacks. Existing callbacks lacking a nonce fail closed and must be recreated. The worker is safe to run in multiple replicas because all authority lives in MySQL leases and fencing tokens.
