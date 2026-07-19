# Durable External Judge Jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist tenant-isolated asynchronous judge jobs and provide fenced MySQL worker lease primitives that survive replica and process failure.

**Architecture:** A focused job service coordinates canonical hashing, AES-GCM source encryption, object lifecycle, and a transaction-oriented MySQL repository. Worker mutations use attempt-number and lease-token fencing; public list cursors are authenticated and tenant-bound.

**Tech Stack:** Go 1.26, `database/sql`, MySQL 8.4/InnoDB, AES-256-GCM, HMAC-SHA256, disposable Docker integration tests.

---

### Task 1: Schema fencing and contracts

**Files:**
- Create: `internal/external/migrations/003_durable_job_fencing.sql`
- Create: `internal/external/job_repository.go`
- Test: `internal/external/job_repository_test.go`

- [x] Write tests requiring source object lifecycle interfaces, job admission/read types, opaque cursors, lease tokens, and retry outcomes.
- [x] Run `go test ./internal/external` and confirm missing contracts fail compilation.
- [x] Add the smallest contracts and replay-safe schema migration for lease tokens and attempt tenant fencing.
- [x] Run the focused tests and commit the schema/contracts slice.

### Task 2: MySQL admission and stable reads

**Files:**
- Create: `internal/external/mysql_job_repository.go`
- Create: `internal/external/mysql_job_repository_test.go`

- [x] Write failing real-MySQL tests for same-key replay, conflicting-key rejection, concurrent duplicate submission, bundle/callback ownership, queued quota fail-closed, no source plaintext, tenant 404, and stable filtered cursor pagination.
- [x] Run the tests against disposable MySQL 8.4 and verify each fails for the missing repository.
- [x] Implement transactional admission, authenticated cursors, encrypted source metadata, object compensation, get/list/cancel.
- [x] Re-run the focused tests with race detection and commit.

### Task 3: Worker leases and restart recovery

**Files:**
- Modify: `internal/external/mysql_job_repository.go`
- Test: `internal/external/mysql_job_worker_test.go`

- [x] Write failing MySQL tests for `FOR UPDATE SKIP LOCKED`, monotonic attempts, heartbeat, stale CAS, success, infrastructure retry/exhaustion, expired reclaim, and cancel recovery.
- [x] Run focused tests and confirm the missing lease methods are the cause.
- [x] Implement transaction-scoped claim and fenced mutations.
- [x] Re-run tests, including concurrent claimers and repository reconstruction, then commit.

### Task 4: HTTP adapter and delivery gates

**Files:**
- Create: `internal/external/http_job_service.go`
- Test: `internal/external/http_job_service_test.go`
- Modify: `README.md`
- Modify: `CHANGELOG.md`

- [x] Write failing adapter tests mapping repository records to redacted HTTP views and repository errors to the public error contract.
- [x] Implement the adapter without pretending an execution worker is wired.
- [x] Document lifecycle, configuration, migrations, and recovery semantics.
- [x] Run `go test -race ./...`, `go vet ./...`, and `CGO_ENABLED=0 go build ./cmd ./cmd/judge-admin`.
- [x] Run independent Critical/Important reviews and resolve all reported findings before delivery.
