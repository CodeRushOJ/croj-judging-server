#!/usr/bin/env bash
set -euo pipefail

REPOSITORY_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPOSITORY_ROOT
readonly CHECKER="${REPOSITORY_ROOT}/scripts/ci/test-webhook-mysql84-contract.sh"

test_root="$(mktemp -d)"
readonly test_root
trap 'rm -rf -- "${test_root}"' EXIT

mkdir -p -- "${test_root}/.github/workflows" "${test_root}/scripts/ci"
cp -- "${CHECKER}" "${test_root}/scripts/ci/test-webhook-mysql84-contract.sh"

assert_rejected() {
  local mutation="$1"

  if bash "${test_root}/scripts/ci/test-webhook-mysql84-contract.sh" >/dev/null 2>&1; then
    printf 'expected checker to reject %s mutation\n' "${mutation}" >&2
    exit 1
  fi
}

cat >"${test_root}/.github/workflows/ci.yml" <<'YAML'
name: Mutation fixture

jobs:
  mysql84-bundle-integration:
    runs-on: ubuntu-24.04
    steps:
      - name: Run durable webhook contract on MySQL 8.4
        run: go test ./internal/external

  decoy:
    runs-on: ubuntu-24.04
    timeout-minutes: 15
    services:
      mysql:
        image: mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d
    steps:
      - name: Unrelated command containing webhook tokens
        env:
          JUDGE_TEST_MYSQL_DSN: root:@tcp(127.0.0.1:3306)/decoy
        run: go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'
YAML

assert_rejected 'tokens split across jobs'

cat >"${test_root}/.github/workflows/ci.yml" <<'YAML'
name: Mutation fixture

jobs:
  mysql84-bundle-integration:
    runs-on: ubuntu-24.04
    timeout-minutes: 15
    services:
      mysql:
        image: mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d
    steps:
      - name: Run durable webhook contract on MySQL 8.4
        run: go test ./internal/external
      - env:
          JUDGE_TEST_MYSQL_DSN: root:@tcp(127.0.0.1:3306)/decoy
        run: go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'
YAML

assert_rejected 'tokens split into an unnamed sibling step'

cat >"${test_root}/.github/workflows/ci.yml" <<'YAML'
name: Mutation fixture

jobs:
  mysql84-bundle-integration:
    runs-on: ubuntu-24.04
    timeout-minutes: 15
    services:
      mysql:
        image: mysql:latest
      digest-decoy:
        image: mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d
    steps:
      - name: Run durable webhook contract on MySQL 8.4
        env:
          JUDGE_TEST_MYSQL_DSN: root:@tcp(127.0.0.1:3306)/croj_bundle_ci?parseTime=true&loc=UTC&charset=utf8mb4
        run: go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'
YAML

assert_rejected 'digest assigned to a decoy service'

cat >"${test_root}/.github/workflows/ci.yml" <<'YAML'
name: Mutation fixture

jobs:
  mysql84-bundle-integration:
    runs-on: ubuntu-24.04
    timeout-minutes: 15
    services:
      mysql:
        image: mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d
    steps:
      - name: Run durable webhook contract on MySQL 8.4
        env:
          JUDGE_TEST_MYSQL_DSN: ""
        run: go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'
YAML

assert_rejected 'empty webhook integration DSN'

printf 'webhook MySQL 8.4 CI contract mutation test: ok\n'
