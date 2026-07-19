#!/usr/bin/env bash
set -euo pipefail

REPOSITORY_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPOSITORY_ROOT
readonly WORKFLOW="${REPOSITORY_ROOT}/.github/workflows/ci.yml"
readonly MYSQL_DIGEST='mysql@sha256:c592c15aaf4a1961e15d82eb31ea5987dda862d1c4b1e93424438c0e91dc1f8d'
readonly WEBHOOK_STEP='Run durable webhook contract on MySQL 8.4'
readonly WEBHOOK_DSN='root:@tcp(127.0.0.1:3306)/croj_bundle_ci?parseTime=true&loc=UTC&charset=utf8mb4'

fail() {
  printf 'webhook MySQL 8.4 CI contract: %s\n' "$1" >&2
  exit 1
}

extract_job() {
  awk '
    /^  mysql84-bundle-integration:[[:space:]]*$/ {
      if (found) {
        exit 2
      }
      found = 1
    }
    found && /^  [[:alnum:]_.-]+:[[:space:]]*$/ && !/^  mysql84-bundle-integration:[[:space:]]*$/ {
      exit
    }
    found { print }
    END {
      if (!found) {
        exit 1
      }
    }
  ' "${WORKFLOW}"
}

extract_webhook_step() {
  awk -v step="${WEBHOOK_STEP}" '
    $0 == "      - name: " step {
      if (found) {
        exit 2
      }
      found = 1
    }
    found && /^      - / && $0 != "      - name: " step {
      exit
    }
    found { print }
    END {
      if (!found) {
        exit 1
      }
    }
  '
}

extract_mysql_service() {
  awk '
    $0 == "      mysql:" {
      if (found) {
        exit 2
      }
      found = 1
    }
    found && (/^    [^ ]/ || /^      [[:alnum:]_.-]+:[[:space:]]*$/) && $0 != "      mysql:" {
      exit
    }
    found { print }
    END {
      if (!found) {
        exit 1
      }
    }
  '
}

job_block="$(extract_job)" || fail 'missing or duplicate mysql84-bundle-integration job'
step_block="$(printf '%s\n' "${job_block}" | extract_webhook_step)" || fail "missing or duplicate ${WEBHOOK_STEP} step"
mysql_service_block="$(printf '%s\n' "${job_block}" | extract_mysql_service)" || fail 'missing or duplicate mysql service'

grep -Eq -- '^    timeout-minutes: 15[[:space:]]*$' <<<"${job_block}" || fail 'job must have timeout-minutes: 15'
grep -Fqx -- "        image: ${MYSQL_DIGEST}" <<<"${mysql_service_block}" || fail 'mysql service must pin the MySQL 8.4 digest'
grep -Fqx -- "          JUDGE_TEST_MYSQL_DSN: ${WEBHOOK_DSN}" <<<"${step_block}" || fail 'webhook step must use the disposable localhost integration DSN'
grep -Fqx -- "        run: go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'" <<<"${step_block}" || fail 'webhook step must run the exact race-enabled MySQL selector'

printf 'webhook MySQL 8.4 CI contract: ok\n'
