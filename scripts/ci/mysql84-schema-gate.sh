#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPOSITORY_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly REPOSITORY_ROOT
readonly SQL_DIRECTORY="${SCRIPT_DIR}/mysql84"
readonly MIGRATION_DIRECTORY="${REPOSITORY_ROOT}/internal/external/migrations"

# mysql:8.4.10 and golang:1.26.3 multi-platform indexes.
readonly MYSQL84_IMAGE='mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d'
readonly GO_IMAGE='golang@sha256:2d6c80227255c3112a4d08e67ba98e58efd3846daf15d9d7d4c389565d881b1a'

readonly DATABASE_NAME='coderushoj_judge'
readonly DATABASE_USER='judge_admin'
readonly DATABASE_PASSWORD='schema-gate-admin-password'
readonly DATABASE_ROOT_PASSWORD='schema-gate-root-password'
readonly API_KEY_PEPPER_B64='ERERERERERERERERERERERERERERERERERERERERERE='

readonly RAW_RUN_TOKEN="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_TOKEN="$(printf '%s' "${RAW_RUN_TOKEN}" | tr -c '[:alnum:]_.-' '-')"
readonly RUN_TOKEN
readonly NETWORK_NAME="croj-mysql84-net-${RUN_TOKEN:0:48}"
readonly MYSQL_CONTAINER="croj-mysql84-db-${RUN_TOKEN:0:48}"

readonly GO_MOD_CACHE="${MYSQL84_GO_MOD_CACHE:-${REPOSITORY_ROOT}/.ci-cache/mysql84-go-mod}"
readonly GO_BUILD_CACHE="${MYSQL84_GO_BUILD_CACHE:-${REPOSITORY_ROOT}/.ci-cache/mysql84-go-build}"

log() {
  printf '[mysql84-schema] %s\n' "$*"
}

fail() {
  log "ERROR: $*" >&2
  return 1
}

container_exists() {
  docker container inspect "${MYSQL_CONTAINER}" >/dev/null 2>&1
}

cleanup() {
  local exit_code=$?
  if ((exit_code != 0)) && container_exists; then
    log 'MySQL log tail follows because the gate failed' >&2
    docker logs --tail 120 "${MYSQL_CONTAINER}" >&2 || true
  fi
  if container_exists; then
    docker stop --time 5 "${MYSQL_CONTAINER}" >/dev/null 2>&1 || true
    docker rm "${MYSQL_CONTAINER}" >/dev/null 2>&1 || true
  fi
  docker network rm "${NETWORK_NAME}" >/dev/null 2>&1 || true
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

mysql_client() {
  docker exec -i \
    -e "MYSQL_PWD=${DATABASE_PASSWORD}" \
    "${MYSQL_CONTAINER}" \
    mysql --protocol=TCP --host=127.0.0.1 --user="${DATABASE_USER}" \
    --batch --raw --skip-column-names "${DATABASE_NAME}" "$@"
}

mysql_file() {
  local sql_file="$1"
  mysql_client <"${sql_file}"
}

run_admin() {
  docker run --rm \
    --network "${NETWORK_NAME}" \
    --user "$(id -u):$(id -g)" \
    -e 'GOCACHE=/cache/go-build' \
    -e 'GOMODCACHE=/cache/go-mod' \
    -e "GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}" \
    -e "JUDGE_DATABASE_DSN=${DATABASE_USER}:${DATABASE_PASSWORD}@tcp(${MYSQL_CONTAINER}:3306)/${DATABASE_NAME}?parseTime=true&charset=utf8mb4" \
    -e "JUDGE_API_KEY_PEPPER_B64=${API_KEY_PEPPER_B64}" \
    -v "${REPOSITORY_ROOT}:/workspace:ro" \
    -v "${GO_MOD_CACHE}:/cache/go-mod" \
    -v "${GO_BUILD_CACHE}:/cache/go-build" \
    -w /workspace \
    "${GO_IMAGE}" \
    go run -mod=readonly ./cmd/judge-admin "$@"
}

wait_for_mysql() {
  local deadline=$((SECONDS + 90))
  while ((SECONDS < deadline)); do
    if ! container_exists ||
      [[ "$(docker inspect --format '{{.State.Running}}' "${MYSQL_CONTAINER}")" != 'true' ]]; then
      fail 'MySQL container exited during startup'
      return
    fi
    if docker exec \
      -e "MYSQL_PWD=${DATABASE_PASSWORD}" \
      "${MYSQL_CONTAINER}" \
      mysqladmin --protocol=TCP --host=127.0.0.1 --user="${DATABASE_USER}" \
      ping --silent >/dev/null 2>&1; then
      return
    fi
    sleep 1
  done
  fail 'MySQL readiness timed out after 90 seconds'
}

expect_fk_rejection() {
  local sql_file="$1"
  local constraint="$2"
  local output
  if output="$(mysql_file "${sql_file}" 2>&1)"; then
    fail "cross-tenant SQL unexpectedly succeeded: ${sql_file#"${REPOSITORY_ROOT}/"}"
    return
  fi
  if [[ "${output}" != *'ERROR 1452'* || "${output}" != *"${constraint}"* ]]; then
    printf '%s\n' "${output}" >&2
    fail "expected ERROR 1452 from ${constraint}"
    return
  fi
  log "rejected cross-tenant insert with ${constraint}"
}

main() {
  require_command docker
  docker info >/dev/null
  for sql_file in \
    "${SQL_DIRECTORY}/same-tenant-fixture.sql" \
    "${SQL_DIRECTORY}/cross-tenant-job.sql" \
    "${SQL_DIRECTORY}/cross-tenant-outbox.sql"; do
    [[ -r "${sql_file}" ]] || fail "missing SQL fixture: ${sql_file}"
  done
  local -a migration_files
  shopt -s nullglob
  migration_files=("${MIGRATION_DIRECTORY}"/[0-9][0-9][0-9]_*.sql)
  shopt -u nullglob
  local migration_count="${#migration_files[@]}"
  ((migration_count > 0)) || fail "no embedded migrations found in ${MIGRATION_DIRECTORY}"
  mkdir -p "${GO_MOD_CACHE}" "${GO_BUILD_CACHE}"

  log "starting isolated MySQL 8.4.10 container ${MYSQL_CONTAINER}"
  docker network create "${NETWORK_NAME}" >/dev/null
  docker run --detach \
    --name "${MYSQL_CONTAINER}" \
    --network "${NETWORK_NAME}" \
    --tmpfs /var/lib/mysql:rw,nosuid,nodev,size=1073741824 \
    -e "MYSQL_ROOT_PASSWORD=${DATABASE_ROOT_PASSWORD}" \
    -e "MYSQL_DATABASE=${DATABASE_NAME}" \
    -e "MYSQL_USER=${DATABASE_USER}" \
    -e "MYSQL_PASSWORD=${DATABASE_PASSWORD}" \
    "${MYSQL84_IMAGE}" >/dev/null
  wait_for_mysql

  log 'applying clean embedded migration through judge-admin'
  run_admin tenant create \
    --name 'Schema Gate Tenant One' \
    --max-queued 100 \
    --max-running 4 \
    --max-source-bytes 1048576 \
    --max-bundles 200 \
    --daily-execution-ms 3600000 \
    --max-infra-tries 3 >/dev/null

  log 'replaying embedded migration through judge-admin'
  run_admin tenant create \
    --name 'Schema Gate Tenant Two' \
    --max-queued 100 \
    --max-running 4 \
    --max-source-bytes 1048576 \
    --max-bundles 200 \
    --daily-execution-ms 3600000 \
    --max-infra-tries 3 >/dev/null

  local history
  history="$(mysql_client -e \
    "SELECT CONCAT_WS('|', COUNT(*), MIN(version), MAX(version), MIN(CHAR_LENGTH(checksum)), MAX(CHAR_LENGTH(checksum))) FROM t_judge_schema_history;")"
  local expected_history="${migration_count}|1|${migration_count}|64|64"
  [[ "${history}" == "${expected_history}" ]] ||
    fail "unexpected migration history: ${history}; expected ${expected_history}"
  log "migration replay retained ${migration_count} immutable 64-character checksums"

  local same_tenant
  same_tenant="$(mysql_file "${SQL_DIRECTORY}/same-tenant-fixture.sql")"
  [[ "${same_tenant}" == 'same-tenant-ok|1|1' ]] ||
    fail "same-tenant fixture assertion failed: ${same_tenant}"
  log 'same-tenant job and outbox inserts succeeded'

  expect_fk_rejection \
    "${SQL_DIRECTORY}/cross-tenant-job.sql" \
    'fk_external_job_bundle_tenant'
  expect_fk_rejection \
    "${SQL_DIRECTORY}/cross-tenant-outbox.sql" \
    'fk_external_webhook_job_tenant'

  log 'PASS'
}

main "$@"
