#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPOSITORY_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly REPOSITORY_ROOT
readonly GATE_SCRIPT="${SCRIPT_DIR}/mysql84-schema-gate.sh"
readonly WORKFLOW="${REPOSITORY_ROOT}/.github/workflows/ci.yml"

fail() {
  printf 'mysql84 schema gate contract: %s\n' "$1" >&2
  exit 1
}

require_literal() {
  local file="$1"
  local literal="$2"
  grep -Fq -- "${literal}" "${file}" ||
    fail "${file#"${REPOSITORY_ROOT}/"} is missing ${literal}"
}

[[ -x "${GATE_SCRIPT}" ]] ||
  fail "scripts/ci/mysql84-schema-gate.sh must exist and be executable"

require_literal "${GATE_SCRIPT}" 'mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d'
require_literal "${GATE_SCRIPT}" 'golang@sha256:2d6c80227255c3112a4d08e67ba98e58efd3846daf15d9d7d4c389565d881b1a'
require_literal "${GATE_SCRIPT}" 'ERROR 1452'
require_literal "${GATE_SCRIPT}" 'fk_external_job_bundle_tenant'
require_literal "${GATE_SCRIPT}" 'fk_external_webhook_job_tenant'
require_literal "${GATE_SCRIPT}" $'readonly MIGRATION_DIRECTORY="${REPOSITORY_ROOT}/internal/external/migrations"'
require_literal "${GATE_SCRIPT}" $'migration_files=("${MIGRATION_DIRECTORY}"/[0-9][0-9][0-9]_*.sql)'
require_literal "${GATE_SCRIPT}" $'expected_history="${migration_count}|1|${migration_count}|64|64"'
if grep -Fq -- "'1|1|1|64|64'" "${GATE_SCRIPT}"; then
  fail 'scripts/ci/mysql84-schema-gate.sh must not hard-code a single migration'
fi

require_literal "${WORKFLOW}" 'mysql84-schema:'
require_literal "${WORKFLOW}" 'ci-lint:'
require_literal "${WORKFLOW}" 'timeout-minutes: 20'
require_literal "${WORKFLOW}" 'timeout-minutes: 15'
require_literal "${WORKFLOW}" 'timeout-minutes: 5'
require_literal "${WORKFLOW}" 'services:'
require_literal "${WORKFLOW}" 'redis:8.4-alpine@sha256:61ea466e4022f3803b9f019e01f7738aac694f01989aee35a013724921d8e742'
require_literal "${WORKFLOW}" 'REDIS_TEST_ADDR: 127.0.0.1:6379'
require_literal "${WORKFLOW}" 'bash scripts/ci/mysql84-schema-gate.sh'
require_literal "${WORKFLOW}" 'actions/cache@0057852bfaa89a56745cba8c7296529d2fc39830'
require_literal "${WORKFLOW}" 'koalaman/shellcheck@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d'
require_literal "${WORKFLOW}" 'rhysd/actionlint@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667'

printf 'mysql84 schema gate contract: ok\n'
