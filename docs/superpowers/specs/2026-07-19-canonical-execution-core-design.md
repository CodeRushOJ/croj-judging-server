# Canonical Execution Core Design

## Scope

This slice connects the durable asynchronous REST job lifecycle to real Sandbox execution without creating a second judging implementation. The existing RocketMQ submission consumer and the MySQL-leased HTTP job worker both adapt their input into one canonical compile-once batch execution core. Sandbox remains an internal gRPC service; the only external protocol is the authenticated asynchronous REST API.

The slice stacks the four linear `codex/compile-once-batch` commits (`c604b1d`, `b0cc41d`, `324521a`, `0beb0f4`) on `codex/external-durable-jobs` at `970a080`. Their merge base is `092222a`; cherry-picking the four commits preserves a small, reviewable history and avoids an unrelated merge commit.

## Canonical execution boundary

The core receives an immutable execution request containing language, plaintext source, time and memory limits, stop-on-failure policy, and a verified bundle artifact. It creates the existing `ExecuteBatchV1` request, compiles once, executes manifest cases in order, validates the complete server stream, checks exact or token output, and returns a canonical result with one of `AC`, `WA`, `CE`, `TLE`, `MLE`, `RE`, or `SE`, aggregate resource metrics, and ordered per-case results.

The core never loads a database row, claims work, publishes a callback, or changes a lease. Those responsibilities remain in entry adapters. This keeps judging semantics identical across RocketMQ and REST and lets the adapters translate the canonical result into their existing public persistence or callback contracts.

## Sandbox discovery and load balancing

The default client target is configurable and uses the form `dns:///sandbox-workers.<namespace>.svc.cluster.local:<port>`. `sandbox-workers` must be a headless Kubernetes Service so its DNS answer contains the Pod addresses maintained through native EndpointSlices. The gRPC connection uses the `round_robin` service config, allowing each batch RPC to be assigned across ready DNS endpoints.

A normal ClusterIP Service resolves to one virtual IP and therefore must not be described as gRPC client-side endpoint balancing. Existing direct EndpointSlice discovery remains only as an explicitly configured deprecated fallback for compatibility; neither entry point uses it by default. The client keeps one reusable gRPC channel to the DNS target, enforces bounded request/response sizes and deadlines, and discards partial streams before any retry.

## RocketMQ adapter

The RocketMQ path keeps its strict versioned event validation, immutable submission/problem-version loading, registry deduplication, and idempotent backend callback. `JudgeService` converts the immutable submission and bundle metadata into the canonical request, invokes the shared core, then maps the canonical result to the existing callback result. Infrastructure errors remain retryable RocketMQ failures; deterministic bundle or Sandbox protocol failures become `SE` rather than false acceptance.

## Durable REST worker

The REST worker claims a MySQL job through the existing `ClaimNext` transaction, then loads the encrypted source and tenant-owned ready bundle metadata through the exact active claim fence. It resolves the immutable bundle through the existing bounded cache and archive verifier, constructs the canonical request, and invokes the same core.

Each running claim owns a child context. A heartbeat loop extends the job and attempt leases with the MySQL clock. A control poll observes cancellation under the same attempt number, worker ID, and lease token. Cancellation, shutdown, execution timeout, heartbeat rejection, or lease loss cancels the child context, which propagates to the gRPC stream. The worker only calls `Complete` while its claim remains current; MySQL rechecks the full active-lease fence and converts a concurrent cancellation into terminal `CANCELLED`. A stale worker treats `ErrStaleJobClaim` as ownership loss and never overwrites the newer attempt.

Retryable storage, DNS, transport, and Sandbox availability failures call `FailInfrastructure`, which requeues or terminally fails according to the tenant retry budget. Deterministic execution outcomes, including `SE`, are persisted as successful execution results so clients can distinguish judge system errors from an exhausted worker infrastructure lifecycle.

## Durable result mapping

The persisted REST result uses the public verdict vocabulary already exposed by the API. Canonical statuses map as follows: `AC -> ACCEPTED`, `WA -> WRONG_ANSWER`, `CE -> COMPILE_ERROR`, `TLE -> TIME_LIMIT_EXCEEDED`, `MLE -> MEMORY_LIMIT_EXCEEDED`, `RE -> RUNTIME_ERROR`, and `SE -> SYSTEM_ERROR`. Compile status is `SUCCEEDED` except for `CE`, where it is `FAILED`; case results include only cases actually produced by the validated Sandbox stream. Time is stored in milliseconds and memory is converted from Sandbox KiB to bytes with overflow checks.

## Configuration and lifecycle

Configuration adds the gRPC target, load-balancing policy, worker identity, concurrency, lease duration, heartbeat interval, cancellation poll interval, idle-claim backoff, and an execution grace bound. Validation requires heartbeat and poll intervals to be shorter than the lease and requires a DNS target unless the deprecated EndpointSlice fallback is explicitly enabled.

Process startup constructs one bundle provider and one canonical core, injects it into both adapters, starts the DNS-backed gRPC channel, REST HTTP server, durable worker pool, and RocketMQ consumer, and stops them from a shared signal context. Shutdown stops new claims first, cancels active execution, closes consumers and gRPC connections, and never starts a persistent service during tests.

## Verification

Every behavior is introduced test-first with the failing command and expected failure recorded before implementation. Unit tests use real in-process gRPC servers and real filesystem ZIP artifacts rather than mocked Sandbox responses. Disposable MySQL 8.4 integration tests prove active-claim bundle/source loading, completion fencing, cancellation propagation, heartbeat lease loss, retry disposition, and restart reclaim. DNS target construction and gRPC round-robin selection are tested with a real custom resolver feeding multiple in-process servers; the production default remains the native DNS resolver.

Required gates are focused RED/GREEN tests, `go test -race ./...`, `go vet ./...`, static builds for both commands, disposable MySQL integration, container build, CI contract checks, and an independent Critical/Important code review. README, CHANGELOG, configuration examples, Kubernetes headless Service/RBAC notes, design documentation, and CI are updated in the same branch.
