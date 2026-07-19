# MySQL 8.4 Judge Schema CI Gate Design

Date: 2026-07-19

## Goal

Add a repeatable CI gate that proves the embedded judge-owned schema works on
real MySQL 8.4 before the external asynchronous REST branch can merge. The gate
must remain independent of the ordinary Go test job and must not require a
host-installed MySQL client or server.

## Architecture

A repository-owned Bash harness starts a digest-pinned MySQL 8.4 container on a
private, uniquely named Docker network. The harness applies the embedded schema
through the production `judge-admin` entry point, runs the entry point again to
prove checksum-stable replay, and executes all SQL assertions through the
container's own MySQL client. A trap always removes the temporary container and
network.

GitHub Actions runs the harness in a dedicated `mysql84-schema` job in parallel
with the existing `test` job. The job has an explicit timeout, uses commit-pinned
checkout/cache actions and a digest-pinned Go container, and enables the Go
module/build cache. A separate
workflow-lint job runs digest-pinned ShellCheck and actionlint containers so
neither tool must be installed on the runner.

## Gate Contract

The harness proves all of the following:

1. A clean database accepts every embedded migration on MySQL 8.4.
2. A second `judge-admin` invocation replays without schema changes or checksum
   drift.
3. `t_judge_schema_history` contains exactly one version with a 64-character
   SHA-256 checksum.
4. Same-tenant job and webhook-outbox rows referencing the tenant's bundle,
   encrypted source, callback, and job are accepted.
5. A cross-tenant job is rejected by a composite tenant foreign key.
6. A cross-tenant webhook-outbox row is rejected by a composite tenant foreign
   key.

Expected foreign-key failures are checked by command status and by MySQL error
code 1452. Any unexpected success, different error, readiness timeout, migration
failure, or assertion mismatch fails the gate and prints bounded container logs.

## Isolation and Security

The database, user, and passwords are fixed disposable CI values and never
represent production credentials. The MySQL image is referenced by immutable
digest. Container and network names include the process and CI run identity to
avoid collisions. The schema source is mounted read-only where practical, no
host port is published, and the database uses an ephemeral tmpfs.

The script uses strict Bash mode, validates required commands, applies bounded
readiness polling, and performs idempotent cleanup. It never logs generated API
keys or production-style DSNs.

## Testing Strategy

TDD starts with a repository contract test that fails while the harness and CI
jobs are absent. The minimal implementation then makes that contract green.
The completed harness is run against real MySQL 8.4 locally, followed by
ShellCheck, actionlint, the existing Go race tests, vet, static builds, and
`git diff --check`. An independent reviewer inspects the final base-to-head
diff before publication.
