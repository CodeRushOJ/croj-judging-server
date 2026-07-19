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
| `EXTERNAL_API_READ_TIMEOUT` | `15m` | Bounds the complete request body; sized for a 512 MiB bundle at the supported minimum rate. |
| `EXTERNAL_API_WRITE_TIMEOUT` | `20m` | Covers upload reading plus at least five minutes for object publication and the final response. |
| `EXTERNAL_API_IDLE_TIMEOUT` | `60s` | Bounds keep-alive idle connections. |
| `EXTERNAL_BUNDLE_MIN_UPLOAD_BYTES_PER_SECOND` | `1048576` | Declares the minimum supported bundle upload rate (1 MiB/s) used to validate the read deadline. |
| `EXTERNAL_BUNDLE_UPLOAD_CONCURRENCY` | `4` | Per-pod non-blocking upload slots; saturation returns retryable `503`. |
| `EXTERNAL_SOURCE_RETENTION` | `720h` | Minimum terminal job/source retention. |
| `EXTERNAL_RETENTION_IDLE_DELAY` | `1m` | Delay when no eligible retention work exists or object deletion is retryable. |
| `EXTERNAL_RETENTION_DELETE_TIMEOUT` | `30s` | Per-object deletion deadline. |

The existing DSN, pepper, key-ring, Redis, bundle store, worker lease, and webhook variables remain mandatory when the external API is enabled.

The configured `TEST_BUNDLE_MAX_OBJECT_BYTES` must fit into `EXTERNAL_API_READ_TIMEOUT` at `EXTERNAL_BUNDLE_MIN_UPLOAD_BYTES_PER_SECOND`, with another two minutes reserved for headers and multipart framing; startup fails otherwise. `EXTERNAL_API_WRITE_TIMEOUT` must be at least five minutes longer than the read timeout. This keeps a near-limit upload from reaching the response deadline while MinIO/S3 publication is completing, which would otherwise turn a successful idempotent commit into a client-visible EOF and a needless retry storm. Current hard bounds are 30 minutes for reads and 40 minutes for writes.

## Daily accounting and scheduling

`dailyExecutionMillis` is enforced in MySQL, not Redis. Claiming uses the database date and reserves the bundle time ceiling for the attempt. Completion uses an overflow-safe sum of the executed case times, capped by the reservation; compile failures with no executed cases consume zero. Cancellation, infrastructure failure, and expired attempts release the reservation. A job whose reservation can never fit the current policy becomes `FAILED` with `DAILY_EXECUTION_LIMIT_TOO_LOW` instead of remaining queued forever. Recovery is fenced by job/attempt/worker/token and cannot double reserve. Operators can inspect `t_external_execution_daily` for `reserved_millis` and `consumed_millis` without reading contestant data.

Tenant scheduling persists `last_claimed_at`. The lock order is tenant, job, then daily ledger/attempt. Concurrent workers use `FOR UPDATE SKIP LOCKED`, so an older backlog from one tenant cannot monopolize every replica.

## Retention and recovery

An independent worker removes expired idempotency rows in transactions of at most 1,000 rows. Retention selects only terminal jobs older than the configured period after their idempotency record and webhook outbox row are gone. Phase one locks tenant → job → source, marks the source with a random delete token plus a persisted lease/next-attempt time, and writes a `MARKED` audit event. Other pods cannot rotate the token until the lease and retry delay expire. Object deletion occurs outside MySQL. A failure records `OBJECT_DELETE_FAILED` plus `DELETE_RETRY`; a later claim rotates the token and retries. Phase two repeats the same lock order, rechecks all references and the unexpired token, writes `DELETED`, and deletes attempts, job metadata, and source metadata atomically.

Do not manually delete marked rows. To diagnose a backlog, inspect `t_external_source_object.delete_marked_at`, `delete_lease_until`, `delete_next_attempt_at`, `delete_attempt_count`, `delete_last_error_code`, and `t_external_retention_audit`, then verify S3/MinIO delete permissions and connectivity.

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

The production-path asynchronous REST integration intentionally rebuilds its
schema, so point it at a dedicated disposable database:

```bash
mysql --host=127.0.0.1 --user=root --password=test-root \
  --execute='CREATE DATABASE judge_rest_e2e CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci'
ASYNC_REST_E2E_MYSQL_DSN='root:test-root@tcp(127.0.0.1:3306)/judge_rest_e2e?parseTime=true&loc=UTC&multiStatements=true' \
  go test -race -count=1 -run '^TestExternalAsyncRESTEndToEnd' ./internal/integration
```

Also run `go vet ./...`, static builds for both binaries, OpenAPI contract tests, ShellCheck, and the image inspection/SBOM/security gates before rollout.
