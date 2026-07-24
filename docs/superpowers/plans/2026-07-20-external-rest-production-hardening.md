# External REST Production Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the external asynchronous REST judge capability-compatible, quota-safe, tenant-fair, retention-bounded, and operationally complete.

**Architecture:** Keep the canonical bundle execution pipeline and external-only runtime. Add one immutable language registry at the HTTP-to-worker boundary and MySQL-authoritative daily accounting, tenant scheduling, and two-phase retention workers using database time and existing fences.

**Tech Stack:** Go 1.26, MySQL 8.4/InnoDB, Kubernetes, OpenAPI 3.1, MinIO/S3-compatible object storage, gRPC Sandbox.

---

### Task 1: Canonical language and checker contract

**Files:** `internal/external/language_registry.go`, `internal/external/job_request.go`, `internal/httpapi/capabilities.go`, `cmd/external_runtime.go`, `api/openapi.yaml`, focused tests and canonical fake-Sandbox integration tests.

- [ ] Write tests proving capabilities advertise only accepted public IDs/checkers, unsupported submission fails before persistence, and `cpp` reaches Sandbox as `cpp`.
- [ ] Run focused tests and record the expected mismatch failures.
- [ ] Add the immutable registry and submission validator; wire capabilities from it.
- [ ] Run focused and integration tests until green; commit.

### Task 2: MySQL daily accounting and fair claim

**Files:** `internal/external/migrations/006_external_execution_accounting_retention.sql`, `internal/external/migrate.go`, `internal/external/mysql_job_worker.go`, worker/repository/MySQL integration tests.

- [ ] Add failing migration, daily-limit, crash-recovery, lock-order, and multi-worker fairness tests.
- [ ] Add a DB-date ledger, attempt reservations, tenant scheduler cursor, constraints, indexes, and postconditions.
- [ ] Reserve under tenant/ledger/job locks; settle terminal attempts and recover expired reservations idempotently.
- [ ] Select eligible tenants by least-recent service with `SKIP LOCKED`; prove concurrent workers choose different tenants.
- [ ] Run MySQL 8.4 replay/integration tests; commit.

### Task 3: Two-phase terminal/source retention

**Files:** migration 006, `internal/external/source_retention.go`, `internal/external/source_retention_worker.go`, `cmd/external_runtime.go`, configuration and focused/MySQL tests.

- [ ] Add failing tests for mark/reference fencing, successful delete/finalize, retry after object failure, audit creation, and worker configuration.
- [ ] Implement bounded mark/claim/delete/finalize operations using DB time and source references.
- [ ] Wire the worker with validated retention/interval/batch settings.
- [ ] Run focused and MySQL recovery tests; commit.

### Task 4: HTTP deadlines, upload concurrency, and failure code

**Files:** `internal/app/runtime.go`, `internal/httpapi/server.go`, `internal/httpapi/bundles.go`, `internal/httpapi/jobs.go`, `internal/httpapi/mysql_job_service.go`, `api/openapi.yaml`, focused tests.

- [ ] Add failing runtime timeout, upload saturation, and FAILED polling response tests.
- [ ] Add explicit timeout config/default validation and a non-blocking upload semaphore.
- [ ] Expose stable `failureCode` only when present and document it.
- [ ] Run focused OpenAPI/runtime tests; commit.

### Task 5: Operations, release evidence, and final gates

**Files:** `README.md`, `docs/operations/external-rest.md`, `CHANGELOG.md`, `deploy/judge-schema-migration-job.yaml`, CI/test evidence docs.

- [ ] Document canonical IDs, daily accounting, fair scheduling, retention lifecycle/configuration, HTTP limits, and rollout order.
- [ ] Update schema version/postcondition and immutable migration Job guidance.
- [ ] Run `go test -race ./...`, `go vet ./...`, static server/admin builds, MySQL 8.4 integration, OpenAPI tests, and shellcheck.
- [ ] Request independent review, fix all Critical/Important findings test-first, commit, and push the draft PR without merging.
