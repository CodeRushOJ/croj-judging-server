# Canonical Execution Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute both RocketMQ submissions and durable asynchronous REST jobs through one compile-once Sandbox batch core with Kubernetes headless-Service DNS load balancing and strict MySQL lease fencing.

**Architecture:** Stack the existing compile-once protocol on the durable REST branch, refactor its batch pipeline into a canonical result-producing core, and add narrow RocketMQ and durable-worker adapters. A single gRPC channel targets a headless Service through `dns:///...` with `round_robin`; the durable worker owns heartbeat/cancellation control while the repository remains the final fencing authority.

**Tech Stack:** Go 1.26, gRPC/protobuf, Kubernetes headless Service DNS, MySQL 8.4/InnoDB, MinIO/S3, RocketMQ, real in-process gRPC integration tests.

---

### Task 1: Stack the compile-once protocol

**Files:**
- Modify: `proto/sandbox.proto`
- Modify: `proto/sandbox.pb.go`
- Modify: `proto/sandbox_grpc.pb.go`
- Create: `internal/service/bundle_batch_pipeline.go`
- Test: `internal/service/bundle_batch_pipeline_test.go`
- Modify: `internal/sandbox/client.go`
- Test: `internal/sandbox/client_test.go`

- [ ] Cherry-pick `c604b1d`, `b0cc41d`, `324521a`, and `0beb0f4` in order.
- [ ] Run `go test -race ./internal/sandbox ./internal/service ./internal/scheduler ./internal/integration` and require all inherited compile-once tests to pass.
- [ ] Record the four upstream commits and focused GREEN output in `docs/testing/canonical-execution-core-red-green.md`.

### Task 2: Make the batch pipeline the canonical execution core

**Files:**
- Create: `internal/execution/result.go`
- Create: `internal/execution/core.go`
- Test: `internal/execution/core_test.go`
- Modify: `internal/service/hidden_test_executor.go`
- Modify: `internal/service/judge_service_test.go`

- [ ] Write a real-artifact/real-gRPC test requiring ordered per-case canonical `AC/WA/CE/TLE/MLE/RE/SE` results, stop-on-failure propagation, bounded metrics, and redacted compile diagnostics.
- [ ] Run `go test ./internal/execution -run TestCore -count=1`, confirm RED because the package/core is missing, and append the command/failure to the RED/GREEN log.
- [ ] Implement `execution.Request`, `execution.Result`, `execution.CaseResult`, and `Core.Execute` by moving the compile-once request/stream validation and aggregation behind this boundary.
- [ ] Run the focused test and `go test -race ./internal/execution ./internal/service`, confirm GREEN, and append output to the log.
- [ ] Adapt `HiddenTestExecutor` to map the canonical result into `callback.Result` without changing RocketMQ identity/publishing behavior.
- [ ] Add a RocketMQ service test first, observe RED, implement the adapter, and record GREEN.
- [ ] Commit the canonical core slice.

### Task 3: Add one DNS-backed gRPC round-robin client

**Files:**
- Create: `internal/sandbox/batch_client.go`
- Test: `internal/sandbox/batch_client_test.go`
- Modify: `pkg/config/config.go`
- Test: `pkg/config/config_test.go`
- Modify: `configs/config.yaml`
- Create: `deploy/sandbox-headless-service.yaml`

- [ ] Write tests that use a manual gRPC resolver and two real in-process Sandbox servers to require one reusable channel, `round_robin` distribution, bounded streams, cancellation, and rejection of non-DNS defaults.
- [ ] Run the focused tests, confirm RED because the DNS batch client/config fields are missing, and record it.
- [ ] Implement a target-based batch client using `grpc.NewClient`, the `round_robin` service config, call size bounds, and per-request deadlines; keep the old direct-address client as deprecated fallback only.
- [ ] Add validated configuration/environment overrides and the headless Service manifest (`clusterIP: None`, gRPC port, sandbox selector).
- [ ] Run `go test -race ./internal/sandbox ./pkg/config`, confirm GREEN, record it, and commit.

### Task 4: Load a fenced durable execution input

**Files:**
- Modify: `internal/external/job_repository.go`
- Modify: `internal/external/mysql_job_worker.go`
- Test: `internal/external/mysql_job_worker_test.go`
- Create: `internal/external/bundle_provider.go`
- Test: `internal/external/bundle_provider_test.go`

- [ ] Add MySQL integration tests requiring `LoadClaimInput` to return source, language, stop-on-failure, and tenant-owned ready immutable bundle metadata only while the full attempt/worker/token/lease fence is active.
- [ ] Run the focused MySQL test against a disposable MySQL 8.4 instance, confirm RED for the missing loader, and record it.
- [ ] Implement one fenced query joining job, tenant, source, and ready bundle; decrypt source only after the fence succeeds and expose no lease/source secret through formatting.
- [ ] Implement an external-bundle adapter to the existing bounded cache/archive verifier.
- [ ] Re-run focused MySQL and artifact tests, confirm GREEN, record it, and commit.

### Task 5: Run durable jobs with heartbeat, cancellation, and strict ownership loss

**Files:**
- Create: `internal/worker/runner.go`
- Test: `internal/worker/runner_test.go`
- Modify: `internal/external/mysql_job_worker.go`
- Test: `internal/external/mysql_job_worker_test.go`

- [ ] Write tests using real gRPC execution plus real MySQL for successful completion, every canonical verdict mapping, heartbeat renewal, cancellation-to-context propagation, stale claim rejection, timeout propagation, infrastructure requeue/exhaustion, and restart reclaim.
- [ ] Run focused tests and confirm RED because the runner/control read is missing; record it.
- [ ] Add fenced `ClaimControl` and implement a worker runner whose child context is cancelled by shutdown, cancel intent, heartbeat failure, lease loss, or execution deadline.
- [ ] Convert canonical metrics safely to `DurableJobResult`, call `Complete` only for current ownership, and route retryable infrastructure errors through `FailInfrastructure`.
- [ ] Re-run focused race/MySQL tests, confirm GREEN, record it, and commit.

### Task 6: Wire process lifecycle and external REST without exposing gRPC

**Files:**
- Modify: `cmd/main.go`
- Create: `internal/app/runtime.go`
- Test: `internal/app/runtime_test.go`
- Modify: `internal/httpapi/server_test.go`

- [ ] Write construction/lifecycle tests that require one canonical core shared by both adapters, REST routes only, worker-first shutdown, and no public gRPC listener.
- [ ] Run focused tests, confirm RED for missing runtime assembly, and record it.
- [ ] Extract validated runtime assembly, create the HTTP server and worker pool, inject the same execution core into RocketMQ and REST paths, and preserve signal-driven cancellation.
- [ ] Run focused tests and static builds, confirm GREEN, record it, and commit.

### Task 7: Documentation, CI, and delivery gates

**Files:**
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `.github/workflows/ci.yml`
- Modify: `scripts/ci/test-bundle-mysql84-contract.sh`
- Create: `docs/testing/canonical-execution-core-red-green.md`

- [ ] Add CI contract tests first requiring MySQL worker integration, DNS round-robin integration, race tests, vet, static builds, and container build; run the contract test and record RED.
- [ ] Update CI, README, CHANGELOG, configuration and Kubernetes deployment documentation, then record contract GREEN.
- [ ] Run `gofmt`, `git diff --check`, `go test -race ./...`, `go vet ./...`, `CGO_ENABLED=0 go build -trimpath ./cmd ./cmd/judge-admin`, disposable MySQL 8.4 tests, and `docker build` without starting persistent services.
- [ ] Request independent Critical/Important review, fix all valid findings with RED/GREEN regression tests, and commit the final delivery.
