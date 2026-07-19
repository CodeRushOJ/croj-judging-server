#!/usr/bin/env bash
set -euo pipefail

REPOSITORY_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPOSITORY_ROOT
readonly WORKFLOW="${REPOSITORY_ROOT}/.github/workflows/ci.yml"
readonly MYSQL_DIGEST='mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d'

grep -Fq -- 'mysql84-bundle-integration:' "${WORKFLOW}"
grep -Fq -- "image: ${MYSQL_DIGEST}" "${WORKFLOW}"
grep -Fq -- 'EXTERNAL_JUDGE_MYSQL_TEST_DSN:' "${WORKFLOW}"
grep -Fq -- "-run '^TestExternalBundleSQLRepositoryIntegration$'" "${WORKFLOW}"
grep -Fq -- 'timeout-minutes:' "${WORKFLOW}"

printf 'bundle MySQL 8.4 CI contract: ok\n'
