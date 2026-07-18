# Hidden Test Bundles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Safely load immutable hidden test bundles and produce correct ACM exact/token verdicts.

**Architecture:** Read immutable bundle metadata from MySQL, fetch verified ZIP bytes through a checksum-keyed cache, parse both manifest copies through one strict decoder, and run ordered cases through bounded sandbox failover. Storage, artifact validation, cache, and execution are isolated behind small interfaces for deterministic tests.

**Tech Stack:** Go 1.26, GORM/MySQL read-only queries, MinIO Go S3 client, archive/zip, SHA-256, gRPC, Docker.

---

### Task 1: Immutable metadata and strict manifest

**Files:** create `pkg/model/test_bundle.go`, `internal/bundle/manifest.go`, `internal/bundle/manifest_test.go`; modify `pkg/model/task.go`, `internal/database/db.go`.

- [ ] Write failing tests for valid v1, unknown fields, duplicate IDs/paths, traversal, invalid weights, SPJ/OI, and normalized DB/artifact equality.
- [ ] Run `go test ./internal/bundle` and confirm missing decoder failures.
- [ ] Implement strict decoder and read-only metadata model/query; add nullable `ProblemVersionID` to Task.
- [ ] Run focused tests and commit.

### Task 2: Safe ZIP reader

**Files:** create `internal/bundle/archive.go`, `internal/bundle/archive_test.go`.

- [ ] Write deterministic ZIP fixtures and failing tests for mandatory manifest, traversal, duplicate entries, symlink, file-count, ratio, per-file and total-size limits.
- [ ] Run focused tests and confirm missing archive loader failures.
- [ ] Implement central-directory validation and bounded in-place reads without extraction.
- [ ] Run focused tests and commit.

### Task 3: Streaming object download and disk cache

**Files:** create `internal/bundle/store.go`, `internal/bundle/cache.go`, `internal/bundle/store_test.go`, `internal/bundle/cache_test.go`; modify `go.mod`.

- [ ] Write failing tests for exact size/SHA-256, oversize stream cleanup, corrupt-hit refetch, concurrent single download, TTL and LRU eviction.
- [ ] Run focused tests and confirm failures.
- [ ] Implement an object reader interface, MinIO adapter, bounded hash download, atomic rename, keyed flight, and bounded cache.
- [ ] Run focused and race tests; commit.

### Task 4: Multi-case ACM runner and endpoint failover

**Files:** create `internal/service/bundle_pipeline.go`, `internal/service/bundle_pipeline_test.go`; modify `internal/service/execution_pipeline.go`.

- [ ] Write failing tests for ordered exact/token cases, first contestant-error stop, max metric aggregation, hidden-safe summaries, and retryable endpoint/status failover limits.
- [ ] Run focused tests and confirm missing runner failures.
- [ ] Implement checker rules, per-case execution, aggregation, and infrastructure-only bounded failover.
- [ ] Run focused and race tests; commit.

### Task 5: Judge flow, configuration, and publishable system errors

**Files:** modify `internal/service/judge_service.go`, tests, `cmd/main.go`, `pkg/config/config.go`, `configs/config.yaml`; create or modify bundle configuration tests.

- [ ] Write failing tests for null/missing/bad bundle as `SYSTEM_ERROR`, transient storage errors as retryable, and immutable version lookup.
- [ ] Run tests and confirm failures.
- [ ] Wire database metadata, cache, MinIO client, loader, pipeline, endpoint failover, and secret-only environment overrides.
- [ ] Run focused tests and commit.

### Task 6: Documentation, full verification, and release workflow

**Files:** modify `README.md`, `CHANGELOG.md`.

- [ ] Document manifest v1, deterministic packaging, MinIO/Secret/cache configuration, security limits, checker normalization, unsupported SPJ/OI, and Issue #11 compile-once debt.
- [ ] Run `go test -race ./...`, `go vet ./...`, static build, and container build.
- [ ] Confirm `git diff --check`, no secret/hidden payload logging, and no backend migration changes.
- [ ] Commit, push `codex/hidden-test-bundles`, and open a Draft PR based on `codex/judge-message-callback` closing #10.
