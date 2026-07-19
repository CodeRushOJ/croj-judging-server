# External asynchronous REST operations

## Rollout order

1. Publish one immutable judging-server image digest containing both `/app/judge-admin` and `/app/judging-server`.
2. Set that digest in `deploy/judge-schema-migration-job.yaml` and run the schema v6 Job against the Judge-owned MySQL 8.4 database.
3. Confirm the Job completed and `judge-admin schema migrate` validated all migration checksums and postconditions.
4. Deploy Sandbox pods behind the private headless Service; the public REST deployment uses the `dns:///...` gRPC target and Kubernetes `round_robin` balancing.
5. Deploy Redis and S3/MinIO credentials, key rings, API peppers, and the external runtime. Keep `LEGACY_JUDGE_ENABLED=false` for an external-only deployment.
6. Wait for `/readyz` before exposing the Service through an HTTPS Gateway.

Never run application pods with DDL privileges. Migrations use the dedicated Job and the exact application image digest.

## Canonical client contract

Call `GET /api/v1/capabilities` before submitting. The v1 language identifiers are `go`, `cpp`, `python`, `java`, and `javascript`; bundle checker identifiers are `exact` and `token`. Unsupported identifiers fail before source encryption, object creation, quota charging, or job persistence. A FAILED polling response includes a stable `failureCode`; internal worker IDs, leases, object keys, source, and hidden cases remain private.

## Required runtime controls

| Variable | Default | Purpose |
| --- | ---: | --- |
| `EXTERNAL_API_READ_HEADER_TIMEOUT` | `5s` | Bounds request headers. |
| `EXTERNAL_API_READ_TIMEOUT` | `30s` | Bounds request bodies, including multipart upload. |
| `EXTERNAL_API_WRITE_TIMEOUT` | `30s` | Bounds response writes. |
| `EXTERNAL_API_IDLE_TIMEOUT` | `60s` | Bounds keep-alive idle connections. |
| `EXTERNAL_BUNDLE_UPLOAD_CONCURRENCY` | `4` | Per-pod non-blocking upload slots; saturation returns retryable `503`. |
| `EXTERNAL_SOURCE_RETENTION` | `720h` | Minimum terminal job/source retention. |
| `EXTERNAL_RETENTION_IDLE_DELAY` | `1m` | Delay when no eligible retention work exists or object deletion is retryable. |
| `EXTERNAL_RETENTION_DELETE_TIMEOUT` | `30s` | Per-object deletion deadline. |

The existing DSN, pepper, key-ring, Redis, bundle store, worker lease, and webhook variables remain mandatory when the external API is enabled.

## Daily accounting and scheduling

`dailyExecutionMillis` is enforced in MySQL, not Redis. Claiming uses the database date and reserves the bundle time ceiling for the attempt. Completion converts the reservation to actual execution milliseconds; cancellation, infrastructure failure, and expired attempts release it. Recovery is fenced by job/attempt/worker/token and cannot double reserve. Operators can inspect `t_external_execution_daily` for `reserved_millis` and `consumed_millis` without reading contestant data.

Tenant scheduling persists `last_claimed_at`. The lock order is tenant, job, then daily ledger/attempt. Concurrent workers use `FOR UPDATE SKIP LOCKED`, so an older backlog from one tenant cannot monopolize every replica.

## Retention and recovery

Retention selects only terminal jobs older than the configured period after their idempotency record and webhook outbox row are gone. Phase one marks the source with a random delete token and writes a `MARKED` audit event. Object deletion occurs outside MySQL. A failure records `OBJECT_DELETE_FAILED` plus `DELETE_RETRY`; a later claim rotates the token and retries. Phase two rechecks all references under the token, writes `DELETED`, and deletes attempts, job metadata, and source metadata atomically.

Do not manually delete marked rows. To diagnose a backlog, inspect `t_external_source_object.delete_marked_at`, `delete_attempt_count`, `delete_last_error_code`, and `t_external_retention_audit`, then verify S3/MinIO delete permissions and connectivity.

## Verification

Use a disposable MySQL 8.4 database:

```bash
docker run --rm -d --name croj-judge-mysql84 \
  -e MYSQL_ROOT_PASSWORD=test-root \
  -e MYSQL_DATABASE=judge_test mysql:8.4.10

docker run --rm --link croj-judge-mysql84:mysql \
  -e JUDGE_TEST_MYSQL_DSN='root:test-root@tcp(mysql:3306)/judge_test?parseTime=true&loc=UTC' \
  -v "$PWD:/workspace" -w /workspace golang:1.26.3 \
  go test -race ./internal/external ./internal/httpapi ./internal/integration

docker stop croj-judge-mysql84
```

Also run `go vet ./...`, static builds for both binaries, OpenAPI contract tests, ShellCheck, and the image inspection/SBOM/security gates before rollout.
