#!/bin/sh
# Generate the synthetic dataset, measure ClickHouse compression, and run the
# sequential plus concurrent gateway latency benchmarks. Requires Docker with
# the Compose plugin, curl, and Python 3.10+.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project=skopos-bench
compose_file="$here/docker-compose.yml"
gateway_url=${SKOPOS_BENCH_GATEWAY_URL:-http://127.0.0.1:${SKOPOS_BENCH_GATEWAY_PORT:-28082}}
iterations=${SKOPOS_BENCH_ITERATIONS:-30}
concurrency=${SKOPOS_BENCH_CONCURRENCY:-30}
concurrency_seconds=${SKOPOS_BENCH_CONCURRENCY_SECONDS:-60}
out=${SKOPOS_BENCH_OUTPUT:-}

compose() {
  docker compose -p "$project" -f "$compose_file" "$@"
}

cleanup() {
  status=$?
  set +e
  if [ "$status" -ne 0 ]; then
    echo "bench: failed (exit $status); keeping containers for inspection" >&2
    compose ps >&2
    compose logs --no-color --tail=30 clickhouse gateway >&2
  elif [ "${SKOPOS_BENCH_KEEP:-0}" = "1" ]; then
    echo "bench: keeping stack because SKOPOS_BENCH_KEEP=1"
  else
    compose down -v --remove-orphans >/dev/null 2>&1
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "bench: missing required command: $1" >&2
    exit 1
  }
}
need docker
need curl
need python3
docker compose version >/dev/null 2>&1 || {
  echo "bench: docker compose plugin is required" >&2
  exit 1
}

case "$concurrency" in
  ''|*[!0-9]*)
    echo "bench: SKOPOS_BENCH_CONCURRENCY must be a non-negative integer" >&2
    exit 1
    ;;
esac
case "$concurrency_seconds" in
  ''|*[!0-9]*)
    echo "bench: SKOPOS_BENCH_CONCURRENCY_SECONDS must be a non-negative integer" >&2
    exit 1
    ;;
esac

echo "bench: rendering chart schema"
"$here/../e2e/obi/render-schema.sh"

echo "bench: starting a clean ClickHouse and gateway"
compose down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d --build

i=0
until curl -fsS "$gateway_url/readyz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 90 ]; then
    echo "bench: gateway did not become ready at $gateway_url/readyz" >&2
    exit 1
  fi
  sleep 2
done
echo "bench: gateway ready"

echo "bench: inserting synthetic dataset (this takes about 15-30s)"
compose exec -T clickhouse clickhouse-client --multiquery <"$here/generate.sql"

echo
echo "bench: compression results"
"$here/measure-compression.sh"

echo
echo "bench: gateway latency (iterations=$iterations)"
set -- "$here/bench.py" --base-url "$gateway_url" --iterations "$iterations"
if [ -n "$out" ]; then
  set -- "$@" --json-out "$out"
fi
python3 "$@"

if [ "$concurrency" -gt 0 ] && [ "$concurrency_seconds" -gt 0 ]; then
  echo
  echo "bench: concurrent mixed workload (clients=$concurrency duration=${concurrency_seconds}s)"
  set -- "$here/bench.py" --base-url "$gateway_url" \
    --concurrency "$concurrency" --duration "$concurrency_seconds" --warmup 1
  containers=$(compose ps -q clickhouse gateway)
  if [ -n "$containers" ]; then
    # Container IDs are whitespace-separated by design.
    # shellcheck disable=SC2086
    set -- "$@" --docker-stats --docker-containers $containers
  fi
  if [ -n "$out" ]; then
    set -- "$@" --json-out "${out%.json}-concurrent.json"
  fi
  python3 "$@"
else
  echo
  echo "bench: concurrent workload skipped (SKOPOS_BENCH_CONCURRENCY=$concurrency SKOPOS_BENCH_CONCURRENCY_SECONDS=$concurrency_seconds)"
fi
