# Durable Webhook Outbox RED/GREEN Evidence

## Task 1: Replay-safe migration

### RED

Command (Go 1.26.3 container):

```bash
docker run --rm -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace golang:1.26.3 go test ./internal/external -run 'TestDurableWebhookMigrationDefinesFencedOutboxAndDisablesLegacyCallbacks|TestEmbeddedMigrations' -count=1
```

Observed failure: `TestDurableWebhookMigrationDefinesFencedOutboxAndDisablesLegacyCallbacks` received only migrations 1–3. This is the expected feature-missing failure before migration 4 exists.

### GREEN

Focused embedded migration contracts passed in Go 1.26.3:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 1.214s
```

Real MySQL 8.4.10 replay, legacy-row upgrade, and duplicate-job rejection passed over the isolated `croj-webhook-test-net` network:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 8.525s
```

The first real-MySQL assertion incorrectly expected pre-MySQL JSON whitespace to survive MySQL's binary JSON representation. It was corrected to validate the migrated legacy payload semantically; exact bytes are guaranteed only for new rows after `payload_body` exists.

## Task 2: Canonical callback destination and callback cipher

### RED

```bash
docker run --rm -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace golang:1.26.3 go test ./internal/external -run 'TestCanonicalCallback|TestCallbackCipher|TestCallbackSecret|TestDecodeCallbackKeyRing' -count=1
```

Observed compile failures for the undefined `CanonicalCallbackDestination`, `NewCallbackCipher`, `CallbackMaterial`, `EncryptedCallbackSecret`, and `DecodeCallbackKeyRing` APIs. The tests existed before implementation and specifically covered full path/query AAD transplant rejection and historical key rotation.

### GREEN

The identical focused command passed:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.012s
```

## Task 3: Operator-only callback provisioning

### RED

```bash
docker run --rm -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace golang:1.26.3 go test ./internal/external ./internal/admincli ./cmd/judge-admin -run 'TestProvisioner.*Callback|TestRun.*Callback|TestCallbackProvisionerOptions' -count=1
```

Observed failures: `Provisioner` had no callback cipher/resolver or `CreateCallback`; admin CLI rejected `callback create`; judge-admin lacked conditional callback key-ring loading.

### GREEN

The identical focused command passed all three packages:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.011s
ok github.com/CodeRushOJ/croj-judging-server/internal/admincli 0.009s
ok github.com/CodeRushOJ/croj-judging-server/cmd/judge-admin 0.008s
```

## Task 4: Stable redacted terminal event encoding

### RED

```bash
docker run --rm -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace golang:1.26.3 go test ./internal/external -run 'TestEncodeTerminalWebhook' -count=1
```

Observed compile failures because `TerminalWebhookEvent` and `EncodeTerminalWebhookEvent` were undefined. Exact payload, state-specific omission, UTC millisecond time, and independent byte storage were specified before implementation.

### GREEN

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.022s
```

## Task 5: Atomic terminal job and outbox writes

### RED

The MySQL 8.4.10 integration tests were added before the repository changes. Completion and running-cancellation finalization both failed with `sql: no rows in result set` because no outbox event existed. A separate legacy-fixture test failed because submission accepted an enabled callback with a missing nonce and undersized ciphertext.

```bash
docker run --rm --network croj-webhook-task5-net \
  -e 'JUDGE_TEST_MYSQL_DSN=root:@tcp(croj-webhook-mysql84-task5:3306)/croj_webhook_test?parseTime=true&loc=UTC&charset=utf8mb4' \
  -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod \
  -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace \
  golang:1.26.3 go test ./internal/external \
  -run 'TestMySQL(TerminalTransitions|RunningCancellation|SubmitRejectsEnabledCallback)' -count=1 -v
```

### GREEN

The terminal transition suite passed against real MySQL 8.4.10. It covers successful completion, exhausted infrastructure failure, queued cancellation, running cancellation finalized by a worker, cancelled and exhausted expired-lease recovery, callback-free jobs, one authoritative event per job, and fail-closed incomplete callback metadata. A forced outbox constraint violation also proves the job and attempt terminal updates roll back with the event insert.

```text
PASS
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.312s
```

The complete external package, including every MySQL integration test, passed:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 4.572s
```

Independent review found two important race/collision edges. Regression tests first reproduced an event-ID uniqueness collision rolling back a terminal transition and a concurrent callback disable winning after admission. The fixes now retry independent event-ID collisions with a bounded budget, recover only an authoritative row for the same job, and hold a callback shared lock through submit commit. Callback-enabled cancellation finalized through both `Complete` and `FailInfrastructure` is covered. The expanded MySQL 8.4.10 suite passed:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 1.332s
```

## Task 6: Structured HTTP outcome and bounded Retry-After

### RED

The structured-outcome tests were added before implementation and failed to compile because `WebhookDeliverer.DeliverOutcome` and the redaction-safe error codes did not exist:

```bash
docker run --rm -v "$PWD:/workspace" -v coderushoj-go-cache:/go/pkg/mod \
  -v coderushoj-go-build-cache:/root/.cache/go-build -w /workspace \
  golang:1.26.3 go test ./internal/external \
  -run 'TestWebhook.*(Outcome|RetryAfter|Unsafe)' -count=1
```

```text
internal/external/webhook_test.go:189:25: deliverer.DeliverOutcome undefined
internal/external/webhook_test.go:207:109: undefined: WebhookErrorNetwork
FAIL github.com/CodeRushOJ/croj-judging-server/internal/external [build failed]
```

### GREEN

The focused classification suite passed after implementing the structured outcome, full 2xx/3xx/4xx/5xx matrix, bounded delta/date `Retry-After`, and unsafe-versus-transient transport classification:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.013s
```

The broader webhook and safe-callback compatibility suite also passed, preserving exact HMAC v1 signing and no-redirect `RoundTrip` behavior:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.025s
```

Independent review added two regression tests. They failed first because HTTP-date delay used the pre-request signing time (`12m0s` instead of `2m0s`) and a duration-overflowing delta was clamped (`15m0s`) instead of ignored. After sampling the injected clock after `RoundTrip` and rejecting delta values beyond `time.Duration`, the full focused suite passed:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.013s
```

The complete external package passed under the race detector, and `go vet ./internal/external` completed cleanly:

```text
ok github.com/CodeRushOJ/croj-judging-server/internal/external 1.357s
```
