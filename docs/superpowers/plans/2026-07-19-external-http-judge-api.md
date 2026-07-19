# External Async REST Judge API Implementation Plan

> Execute this plan test-first in `croj-judging-server`. Keep the existing RocketMQ contract compatible and keep the Sandbox gRPC API private.

**Goal:** Deliver a durable, tenant-isolated `/api/v1` asynchronous judge API that uploads immutable hidden-test bundles, submits idempotent jobs, survives replica/process failure, exposes redacted results, and signs retryable webhooks.

**Architecture:** HTTP handlers authenticate opaque tenant keys and persist commands in a judge-owned MySQL schema. Lease workers convert durable jobs to the same canonical execution request used by RocketMQ, discover Sandbox Pods through Kubernetes EndpointSlice, and persist terminal results with CAS. Redis is an availability-dependent quota adapter, S3/MinIO stores encrypted source and content-addressed bundles, and a transactional outbox drives registered HTTPS webhooks.

**Stack:** Go 1.26 standard `net/http`, MySQL 8.4/GORM driver, Redis, MinIO/S3, Kubernetes client-go, gRPC, embedded SQL migrations, OpenAPI 3.1, Docker/Kind CI.

---

## Task 1: HTTP foundation, RFC 9457 errors, API-key authentication, and capabilities

**Files:**

- Create: `internal/httpapi/problem.go`
- Create: `internal/httpapi/auth.go`
- Create: `internal/httpapi/capabilities.go`
- Create: `internal/httpapi/server.go`
- Test: `internal/httpapi/auth_test.go`
- Test: `internal/httpapi/server_test.go`

1. Write failing handler tests for missing/malformed/unknown/revoked keys, scope denial, request IDs, problem JSON, and authenticated capabilities.
2. Write failing verifier tests for strict opaque-key parsing and HMAC-SHA256 verification against a lookup-prefix repository.
3. Implement only enough middleware and routing to make the tests pass. Never log or return the bearer token.
4. Run `go test -race ./internal/httpapi`, `go vet ./internal/httpapi`, and `gofmt`.
5. Commit `feat(http-api): add authenticated capabilities endpoint`.

## Task 2: Judge-owned schema, migrations, tenant policy, and CLI key provisioning

**Files:**

- Create: `internal/external/migrations/*.sql`
- Create: `internal/external/migrate.go`
- Create: `internal/external/repository.go`
- Create: `internal/external/mysql_repository.go`
- Create: `cmd/judge-admin/main.go`
- Test: `internal/external/migrate_test.go`
- Test: `internal/external/mysql_repository_test.go`
- Test: `cmd/judge-admin/main_test.go`

1. Test clean install and forward migration on MySQL 8.4 for tenants, credentials, callback registrations, bundle ownership, source objects, jobs, attempts, idempotency, and webhook outbox.
2. Test credential creation prints a 256-bit secret once while MySQL stores only lookup prefix and peppered HMAC.
3. Test revocation/expiry/scope and tenant-policy reads fail closed for writes.
4. Implement embedded immutable migrations with a judge-specific history table and transactional repository methods.
5. Run unit/integration tests, migration replay, and `go vet`; commit `feat(http-api): add tenant repository and key provisioning`.

## Task 3: Streaming immutable bundle upload and tenant isolation

**Files:**

- Create: `internal/external/bundle_service.go`
- Create: `internal/external/object_store.go`
- Create: `internal/httpapi/bundles.go`
- Test: `internal/external/bundle_service_test.go`
- Test: `internal/httpapi/bundles_test.go`
- Test: `internal/integration/external_bundle_upload_test.go`

1. Test multipart streaming limits, SHA-256, ZIP traversal/link/count/ratio/uncompressed bounds, manifest/case pairing, and cancellation cleanup.
2. Test `(tenant, Idempotency-Key)` same-hash replay and different-hash 409; cross-tenant metadata is 404.
3. Publish atomically to `external/<tenant>/sha256/<digest>.zip`; callers never supply or receive object URLs/keys.
4. Test concurrent identical uploads create one logical ownership row and no incomplete visible object.
5. Run race/integration tests; commit `feat(http-api): add immutable bundle uploads`.

## Task 4: Idempotent asynchronous jobs, encrypted source, leases, cancellation, and recovery

**Files:**

- Create: `internal/external/job.go`
- Create: `internal/external/job_service.go`
- Create: `internal/external/source_crypto.go`
- Create: `internal/external/worker.go`
- Create: `internal/httpapi/jobs.go`
- Test: `internal/external/job_service_test.go`
- Test: `internal/external/worker_test.go`
- Test: `internal/httpapi/jobs_test.go`
- Test: `internal/integration/external_job_recovery_test.go`

1. Test strict request decoding, language IDs, source limits, bundle ownership, encrypted object persistence, canonical request hashing, 202 replay, and conflicting-key 409.
2. Test stable cursor pagination and cross-tenant 404 for get/cancel.
3. Test `QUEUED -> RUNNING -> SUCCEEDED|FAILED|CANCELLED`, idempotent cancellation, lease heartbeat, expiry reclaim, monotonic attempt number, and stale-worker CAS rejection.
4. Test source plaintext never reaches MySQL/log/result and AES-256-GCM key versions decrypt only their own ciphertext.
5. Implement worker retry/backoff and terminal transaction; run race/MySQL/MinIO restart tests; commit `feat(http-api): add durable asynchronous judge jobs`.

## Task 5: Canonical execution core shared by HTTP and RocketMQ

**Files:**

- Create: `internal/service/canonical_request.go`
- Create: `internal/service/canonical_judge.go`
- Modify: `internal/service/judge_service.go`
- Modify: `internal/consumer/rocketmq.go`
- Modify: `cmd/main.go`
- Test: `internal/service/canonical_judge_test.go`
- Test: `internal/integration/external_judge_sandbox_test.go`

1. Write contract tests proving HTTP and RocketMQ adapters build equivalent immutable execution requests and neither calls Sandbox directly.
2. Adapt the compile-once batch pipeline to return ordered redacted case results to external jobs while preserving the existing backend callback event.
3. Propagate cancellation between compile/case boundaries and keep Sandbox hard limits authoritative.
4. Test Accepted, Wrong Answer, Compile Error, infrastructure retry, EndpointSlice failover, and redaction using real TCP gRPC.
5. Run full race/vet/static build; commit `refactor(judge): share canonical execution core`.

## Task 6: Redis quotas and multi-replica admission

**Files:**

- Create: `internal/external/quota.go`
- Create: `internal/external/redis_quota.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/external/worker.go`
- Test: `internal/external/redis_quota_test.go`
- Test: `internal/integration/external_quota_test.go`

1. Test per-tenant request/upload token buckets and MySQL queued/running/daily/retained limits across concurrent replicas.
2. Test Redis uncertainty returns 503 for new writes, established exhaustion returns 429, and authorized reads remain available.
3. Test workers cannot over-claim global or tenant concurrency under races.
4. Run race and Redis integration tests; commit `feat(http-api): enforce distributed tenant quotas`.

## Task 7: Registered signed webhooks and SSRF-safe delivery

**Files:**

- Create: `internal/external/webhook.go`
- Create: `internal/external/webhook_worker.go`
- Create: `internal/external/safe_transport.go`
- Test: `internal/external/webhook_test.go`
- Test: `internal/external/safe_transport_test.go`
- Test: `internal/integration/external_webhook_test.go`

1. Test terminal result and outbox insertion are atomic and each logical event ID is stable.
2. Test signature input `<timestamp>.<raw-body>`, headers, retry matrix, jitter/backoff, and at-least-once delivery.
3. Test HTTPS/allowlist/explicit-port enforcement, public IP resolution and per-delivery revalidation, redirect denial, DNS rebinding, and IPv4/IPv6 private ranges.
4. Ensure errors/logs never expose secrets, source, cases, resolved Pod addresses, or response bodies.
5. Run race/integration tests; commit `feat(http-api): deliver signed registered webhooks`.

## Task 8: Lifecycle, metrics, retention, OpenAPI, platform manifests, and E2E

**Files:**

- Create: `api/openapi.yaml`
- Create: `internal/external/retention.go`
- Modify: `cmd/main.go`
- Modify: `pkg/config/config.go`
- Modify: `configs/config.yaml`
- Modify: `Dockerfile`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Test: `internal/integration/external_oj_e2e_test.go`
- Modify platform Helm/Gateway/NetworkPolicy/CI in `croj-platform` after the server contract is green.

1. Test readiness ordering, write drain, claim stop, graceful deadline, lease release, and restart recovery.
2. Add bounded-cardinality metrics and redacted structured correlation fields.
3. Test mark-before-delete retention for idempotency, source, terminal jobs, webhook rows, and unreferenced bundles.
4. Publish/lint OpenAPI 3.1 with compatibility checks and executable examples.
5. Add private-by-default HTTP Service port, opt-in TLS Gateway, Secrets, NetworkPolicy, MySQL/Redis/MinIO wiring, and rollback notes.
6. Run the external-OJ E2E: provision key, upload, Accepted/WA, idempotent replay/conflict, restart/reclaim, webhook signature, queued cancel, and cross-tenant denial.
7. Run `go test -race ./...`, `go vet ./...`, static build, image inspection, SBOM, Trivy, Helm lint/render, Kind E2E, and independent Critical/Important review.
8. Commit `feat(http-api): complete external OJ delivery gates` and keep the PR Draft until every dependency and check is green.
