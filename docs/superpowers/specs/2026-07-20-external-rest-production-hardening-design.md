# External REST Production Hardening Design

## Scope

This hardening pass closes the independent review findings on the asynchronous external judge without changing the external-only boundary, webhook SSRF/TLS controls, transactional outbox, or canonical compile-once execution pipeline.

## Canonical capabilities

One registry owns every public language identifier, display name, runtime identifier, and Sandbox identifier. Submission validates the public identifier before durable acceptance and stores the canonical Sandbox identifier. Capabilities and OpenAPI use the same lowercase checker identifiers accepted by bundle manifests. A canonical fake Sandbox integration test exercises capabilities, bundle publication, submission, claim, execution, and terminal polling without a mock result shortcut.

## Daily execution accounting

MySQL is authoritative. A daily tenant ledger is keyed by tenant and `CURRENT_DATE`, never application time. Claiming follows the fixed tenant → job → ledger/attempt lock order, recovers expired reservations, then atomically reserves the bundle time ceiling. Terminal success and cancellation with trusted case measurements settle to the overflow-safe measured total. Cancellation or compilation failure without trusted measurements consumes the full reservation to prevent deliberate quota evasion. Infrastructure and expired-lease failures refund the reservation before a replacement claim. Replays and process crashes are idempotent through attempt/job fencing.

## Fair scheduling

Tenant rows carry a scheduler cursor. A claim first locks one eligible tenant ordered by recovery priority and least-recent service, then locks one eligible job for that tenant. The lock order is tenant then job then ledger/attempt. `SKIP LOCKED` allows concurrent workers to select different tenants. Updating the cursor in the same transaction provides bounded round-robin fairness while retaining per-tenant running ceilings.

## Retention

Terminal jobs and their encrypted source objects are retained for a configurable period. A bounded worker first marks eligible jobs/source rows using database time and reference checks, records an audit event, and commits. It then deletes the object; a second fenced transaction records deletion and redacts source metadata. Transient object-store failures leave marked rows retryable. Jobs are deleted only after webhook terminal audit retention and idempotency references permit deletion.

Bundle uploads stage under the separately enumerable `external-staging/` prefix. Object-store operations happen outside MySQL transactions. A garbage collector applies a safety age longer than the bounded application upload deadline and checks authoritative bundle references both before and immediately before deletion. Operators additionally configure a longer object-store lifecycle rule for that prefix only.

## HTTP availability and contract

The external server receives explicit read-header, read, write, and idle timeouts. A bounded semaphore rejects excess concurrent bundle uploads before reading their bodies. Polling views include a stable `failureCode`. OpenAPI, README, operations guidance, CHANGELOG, migration Job, schema postconditions, and tests remain synchronized.

## Verification

Each behavior starts with a focused failing test. Final gates cover race tests, vet, static builds, MySQL 8.4 migration replay/integration, OpenAPI contract tests, shellcheck, and an independent final review. The branch is pushed to the existing draft PR and is not merged.
