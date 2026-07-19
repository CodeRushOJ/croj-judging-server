# MySQL 8.4 Judge Schema CI Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a digest-pinned, container-only MySQL 8.4 CI gate for the judge-owned schema.

**Architecture:** A strict Bash harness creates an isolated MySQL and Go toolchain network, applies migrations twice through `judge-admin`, and proves tenant-safe foreign keys with real SQL. Separate GitHub Actions jobs run the schema gate and digest-pinned ShellCheck/actionlint checks in parallel with the existing Go job.

**Tech Stack:** Bash, Docker, MySQL 8.4.10, Go 1.26.3, GitHub Actions, ShellCheck 0.11.0, actionlint 1.7.12.

---

### Task 1: Add a failing repository contract test

**Files:**
- Create: `scripts/ci/test-mysql84-schema-gate-contract.sh`
- Test: `scripts/ci/test-mysql84-schema-gate-contract.sh`

- [ ] **Step 1: Write the failing contract**

Create a strict Bash test that requires:

```bash
#!/usr/bin/env bash
set -euo pipefail

test -x scripts/ci/mysql84-schema-gate.sh
grep -Fq 'mysql84-schema:' .github/workflows/ci.yml
grep -Fq 'timeout-minutes:' .github/workflows/ci.yml
grep -Fq 'scripts/ci/mysql84-schema-gate.sh' .github/workflows/ci.yml
grep -Fq 'shellcheck@sha256:' .github/workflows/ci.yml
grep -Fq 'actionlint@sha256:' .github/workflows/ci.yml
```

- [ ] **Step 2: Run it and verify RED**

Run:

```bash
bash scripts/ci/test-mysql84-schema-gate-contract.sh
```

Expected: FAIL because `scripts/ci/mysql84-schema-gate.sh` and the workflow jobs do not exist.

- [ ] **Step 3: Commit the red test**

```bash
git add scripts/ci/test-mysql84-schema-gate-contract.sh
git commit -m "test(ci): require MySQL 8.4 schema gate"
```

### Task 2: Implement the real MySQL gate

**Files:**
- Create: `scripts/ci/mysql84-schema-gate.sh`
- Create: `scripts/ci/mysql84/same-tenant-fixture.sql`
- Create: `scripts/ci/mysql84/cross-tenant-job.sql`
- Create: `scripts/ci/mysql84/cross-tenant-outbox.sql`
- Test: `scripts/ci/mysql84-schema-gate.sh`

- [ ] **Step 1: Add immutable container inputs and lifecycle**

The harness must define literal digest references for MySQL 8.4.10 and Go
1.26.3, require `bash`, `docker`, `git`, create unique container/network
names, and register cleanup before starting containers:

```bash
readonly MYSQL84_IMAGE='mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d'
readonly GO_IMAGE='golang@sha256:2d6c80227255c3112a4d08e67ba98e58efd3846daf15d9d7d4c389565d881b1a'
readonly RUN_TOKEN="${GITHUB_RUN_ID:-local}-$$"
readonly NETWORK_NAME="croj-mysql84-${RUN_TOKEN}"
readonly MYSQL_NAME="croj-mysql84-db-${RUN_TOKEN}"
trap cleanup EXIT
```

- [ ] **Step 2: Add bounded readiness and production migration replay**

Start MySQL without a published port, wait at most 60 seconds using
`mysqladmin ping` inside the container, and invoke:

```bash
run_admin tenant create --name 'Schema Gate Tenant One' \
  --max-queued 100 --max-running 4 --max-source-bytes 1048576 \
  --max-bundles 200 --daily-execution-ms 3600000 --max-infra-tries 3
run_admin tenant create --name 'Schema Gate Tenant Two' \
  --max-queued 100 --max-running 4 --max-source-bytes 1048576 \
  --max-bundles 200 --daily-execution-ms 3600000 --max-infra-tries 3
```

`run_admin` runs the digest-pinned Go container on the same Docker network,
mounts the repository read-only, and mounts explicit module/build cache
directories supplied by `MYSQL84_GO_MOD_CACHE` and
`MYSQL84_GO_BUILD_CACHE`.

- [ ] **Step 3: Add same-tenant and cross-tenant SQL proofs**

`same-tenant-fixture.sql` creates fixed tenant-owned bundle, encrypted source,
callback, job, and outbox rows and ends with:

```sql
SELECT 'same-tenant-ok', COUNT(*)
FROM t_external_job
WHERE external_id = 'eeeeeeeeeeeeeeeeeeeeeeeeee';
```

The two cross-tenant files attempt one forbidden job and one forbidden outbox
insert. The harness must require both commands to fail and require their
captured stderr to contain `ERROR 1452` plus the expected composite constraint
name.

- [ ] **Step 4: Assert migration history**

Execute through the container client:

```sql
SELECT COUNT(*), MIN(version), MAX(version),
       MIN(CHAR_LENGTH(checksum)), MAX(CHAR_LENGTH(checksum))
FROM t_judge_schema_history;
```

Require exact output `1 1 1 64 64`.

- [ ] **Step 5: Run the harness and verify GREEN behavior**

Run:

```bash
MYSQL84_GO_MOD_CACHE="$PWD/.ci-cache/go-mod" \
MYSQL84_GO_BUILD_CACHE="$PWD/.ci-cache/go-build" \
bash scripts/ci/mysql84-schema-gate.sh
```

Expected: PASS with clean install, replay, same-tenant success, both cross-tenant
rejections, and history checksum confirmation.

- [ ] **Step 6: Commit the harness**

```bash
git add scripts/ci/mysql84-schema-gate.sh scripts/ci/mysql84
git commit -m "test(ci): gate judge schema on MySQL 8.4"
```

### Task 3: Wire pinned parallel CI and documentation

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Test: `scripts/ci/test-mysql84-schema-gate-contract.sh`

- [ ] **Step 1: Add the parallel schema job**

Add `mysql84-schema` with `runs-on: ubuntu-24.04`,
`timeout-minutes: 15`, commit-pinned checkout/cache actions, cache keys based
on `go.sum` and the schema script, and one command:

```yaml
- name: Run real MySQL 8.4 schema gate
  env:
    MYSQL84_GO_MOD_CACHE: ${{ runner.temp }}/croj-mysql84-go-mod
    MYSQL84_GO_BUILD_CACHE: ${{ runner.temp }}/croj-mysql84-go-build
  run: bash scripts/ci/mysql84-schema-gate.sh
```

Do not add `needs: test`; the jobs must run in parallel.

- [ ] **Step 2: Add containerized workflow/script lint**

Add a `ci-lint` job with `timeout-minutes: 5`, pinned checkout, and Docker
commands using literal digest references:

```yaml
docker run --rm -v "$PWD:/repo:ro" \
  koalaman/shellcheck@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d \
  /repo/scripts/ci/mysql84-schema-gate.sh \
  /repo/scripts/ci/test-mysql84-schema-gate-contract.sh
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  rhysd/actionlint@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
```

- [ ] **Step 3: Document local reproduction**

Document the single local command, Docker requirement, immutable MySQL version,
cache locations, verified invariants, and cleanup behavior in `README.md`.
Record the new CI gate in `CHANGELOG.md`.

- [ ] **Step 4: Run the contract and verify GREEN**

```bash
bash scripts/ci/test-mysql84-schema-gate-contract.sh
```

Expected: PASS.

- [ ] **Step 5: Commit CI wiring and docs**

```bash
git add .github/workflows/ci.yml README.md CHANGELOG.md \
  scripts/ci/test-mysql84-schema-gate-contract.sh
git commit -m "ci: enforce real MySQL 8.4 schema compatibility"
```

### Task 4: Validate, independently review, and publish

**Files:**
- Verify only: all base-to-head changes

- [ ] **Step 1: Run focused and repository gates**

```bash
bash scripts/ci/test-mysql84-schema-gate-contract.sh
bash scripts/ci/mysql84-schema-gate.sh
docker run --rm -v "$PWD:/repo:ro" \
  koalaman/shellcheck@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d \
  /repo/scripts/ci/mysql84-schema-gate.sh \
  /repo/scripts/ci/test-mysql84-schema-gate-contract.sh
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  rhysd/actionlint@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/judging-server ./cmd
CGO_ENABLED=0 go build -trimpath -o /tmp/judge-admin ./cmd/judge-admin
git diff --check origin/codex/http-judge-api...HEAD
```

Expected: every command passes.

- [ ] **Step 2: Request independent review**

Provide the reviewer with the base/head SHAs and ask for Critical/Important
findings focused on shell safety, digest/action pinning, cleanup, timeout/cache,
real migration replay, tenant FK assertions, and workflow concurrency.

- [ ] **Step 3: Rebase on the latest stacked base**

```bash
git fetch origin codex/http-judge-api
git rebase origin/codex/http-judge-api
```

Rerun the focused contract, real MySQL gate, ShellCheck, and actionlint.

- [ ] **Step 4: Push and open the stacked Draft PR**

```bash
git push -u origin codex/mysql84-schema-ci-gate
gh pr create --draft --base codex/http-judge-api \
  --head codex/mysql84-schema-ci-gate \
  --title "ci: gate judge schema on MySQL 8.4" \
  --body $'## Summary\n- run judge migrations on real MySQL 8.4\n- reject cross-tenant job and outbox rows\n- lint workflow and shell harness\n\n## Validation\n- real MySQL clean/replay gate\n- ShellCheck and actionlint\n- Go race, vet, and static builds'
```
