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
