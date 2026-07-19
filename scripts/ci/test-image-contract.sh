#!/usr/bin/env bash
set -euo pipefail

image="${1:-coderushoj/judging-server:ci}"
container_id=""
archive="$(mktemp)"

cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null 2>&1 || true
  fi
  rm -f "$archive"
}
trap cleanup EXIT

container_id="$(docker create "$image")"
docker export "$container_id" >"$archive"

for required_path in app/judging-server app/judge-admin app/configs/config.yaml; do
  if ! tar -tf "$archive" "$required_path" >/dev/null; then
    echo "container image is missing /$required_path" >&2
    exit 1
  fi
done

if ! docker image inspect "$image" --format '{{json .Config.User}} {{json .Config.Entrypoint}}' |
  grep -Fxq '"65532:65532" ["/app/judging-server"]'; then
  echo "container image must run /app/judging-server as non-root 65532:65532" >&2
  exit 1
fi
