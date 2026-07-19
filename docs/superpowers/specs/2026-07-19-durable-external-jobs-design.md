# Durable External Judge Jobs Design

## Scope

This slice makes asynchronous external judge jobs durable. It owns MySQL job admission, tenant isolation, encrypted-source object metadata, stable reads, cancellation, worker leases, attempts, retry, and restart recovery. It does not execute Sandbox work, deliver webhooks, or implement Redis request-rate limiting.

## Admission transaction

`MySQLJobRepository.Submit` validates the canonical request before side effects. It computes `HMAC-SHA256(idempotency-pepper, Idempotency-Key)` and the canonical request hash, then starts an InnoDB transaction. The transaction locks the active tenant row, decodes and validates its policy, looks up a non-deleted bundle and enabled callback through the same tenant, and counts queued jobs. An unavailable or invalid policy/quota state fails closed.

An existing `(tenant, operation_scope, key_digest)` row with the same request hash returns the original job; a different hash returns an idempotency conflict. For a new job, the service generates source/job IDs, encrypts source with tenant and source ID as AES-GCM associated data, durably reserves the private object key before upload, and persists only object key, digest, length, key version, and nonce. Database commit publishes the job and idempotency response. Definite rollback compensates immediately. Outcome-ambiguous writes keep their reservation so a retention sweeper can compare it with authoritative source metadata and delete only unreferenced ciphertext.

## Reads and cancellation

Job lookups always predicate on tenant external ID and job external ID; foreign-tenant and missing IDs are indistinguishable. Lists use descending `(created_at, id)` keyset pagination. The cursor is an authenticated, versioned payload bound to tenant and status filter, so it cannot be altered or replayed across tenants.

Cancellation locks the job. `QUEUED` becomes terminal `CANCELLED`; `RUNNING` records intent; terminal states are idempotently unchanged. A cancelled expired lease is claimed only by recovery logic and immediately finalized without executing contestant code.

## Worker fencing

Claim uses one transaction and `SELECT ... FOR UPDATE SKIP LOCKED`, ordered by `next_attempt_at, created_at, id`. It selects due `QUEUED` jobs or expired `RUNNING` jobs, retries tenant selection after concurrent contention, expires the old attempt, increments `attempt_no`, generates an unguessable lease token, and inserts an attempt row. Lease issuance and expiry predicates use the MySQL clock. The lease token, attempt number, worker ID, and active lease form the fencing predicate for heartbeat, completion, and retry.

Heartbeat extends both job and attempt leases with CAS. Completion validates the redacted result, updates job and attempt atomically, and rejects stale workers. Infrastructure failure either requeues with bounded delay or marks terminal `FAILED` after the tenant policy retry budget. Expired running jobs are reclaimed after process restart; old workers cannot write through a newer lease.

## Verification

Unit tests cover request/cursor cryptography and object lifecycle compensation. Disposable MySQL 8.4 integration tests prove concurrent idempotency, tenant-owned lookups, fail-closed quota, keyset pagination, tenant 404, cancellation, `SKIP LOCKED` claims, heartbeat CAS, stale completion rejection, infrastructure retry, and restart/reclaim. Required gates are `go test -race ./...`, `go vet ./...`, static build, real MySQL replay, and an independent Critical/Important review.
