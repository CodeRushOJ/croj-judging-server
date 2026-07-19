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
