#!/bin/sh
# Run the OBI -> Collector -> ClickHouse -> gateway end-to-end test on a Linux
# Docker host. Requires a kernel with BTF and tracefs (see README.md).
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$here/docker-compose.yml"
project=skopos-obi-e2e
gateway_url=${SKOPOS_E2E_GATEWAY_URL:-http://127.0.0.1:${SKOPOS_E2E_GATEWAY_PORT:-18081}}
app_url=${SKOPOS_E2E_APP_URL:-http://127.0.0.1:${SKOPOS_E2E_APP_PORT:-18080}}

compose() {
  docker compose -p "$project" -f "$compose_file" "$@"
}

cleanup() {
  status=$?
  set +e
  if [ "$status" -ne 0 ]; then
    echo "run: harness failed (exit $status); keeping containers for inspection" >&2
    compose ps >&2
    for svc in obi collector clickhouse gateway app; do
      echo "----- $svc logs (tail) -----" >&2
      compose logs --no-color --tail=40 "$svc" >&2
    done
    echo "run: inspect with: docker compose -p $project -f $compose_file logs" >&2
    echo "run: remove with:   docker compose -p $project -f $compose_file down -v" >&2
  elif [ "${SKOPOS_E2E_KEEP:-0}" = "1" ]; then
    echo "run: keeping stack because SKOPOS_E2E_KEEP=1"
    echo "run: gateway: $gateway_url  app: $app_url"
  else
    compose down -v --remove-orphans >/dev/null 2>&1
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "run: missing required command: $1" >&2
    exit 1
  }
}
need docker
need curl
need jq
docker compose version >/dev/null 2>&1 || {
  echo "run: docker compose plugin is required" >&2
  exit 1
}

tracefs=${SKOPOS_E2E_TRACEFS:-/sys/kernel/tracing}
cgroupfs=${SKOPOS_E2E_CGROUPFS:-/sys/fs/cgroup}
[ -d "$tracefs" ] || { echo "run: $tracefs is missing; enable CONFIG_TRACEFS and mount tracefs" >&2; exit 1; }
[ -d "$cgroupfs" ] || { echo "run: $cgroupfs is missing; cgroup v2 is required" >&2; exit 1; }

echo "run: rendering chart schema"
"$here/render-schema.sh"

echo "run: starting a clean stack"
compose down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d --build

i=0
until curl -fsS "$gateway_url/readyz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 90 ]; then
    echo "run: gateway did not become ready at $gateway_url/readyz" >&2
    exit 1
  fi
  sleep 2
done
echo "run: gateway ready"

i=0
until curl -fsS "$app_url/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "run: target app did not become ready at $app_url/healthz" >&2
    exit 1
  fi
  sleep 1
done

# OBI v1.0.0 occasionally misses the target's listening socket when the stack
# is recreated in quick succession on the same host (the first attach logs the
# process but never reports traffic). Wait for the tracer to be fully launched
# before sending load; a fixed sleep races with OBI's ~5-8s startup.
before=$(compose logs --no-color obi 2>/dev/null | wc -l | tr -d ' ')
compose restart obi >/dev/null
i=0
while :; do
  logs=$(compose logs --no-color obi 2>/dev/null || true)
  total=$(printf '%s\n' "$logs" | wc -l | tr -d ' ')
  if [ "$total" -gt "$before" ] && printf '%s\n' "$logs" | tail -n "+$((before + 1))" | grep -q 'Launching p.Tracer'; then
    break
  fi
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "run: OBI did not launch its tracer within 60s" >&2
    exit 1
  fi
  sleep 1
done
sleep 2
echo "run: target app ready; OBI tracer launched"
echo "run: OBI process discovery:"
compose logs --no-color obi 2>&1 | grep -E 'instrumenting process|Unsupported Go program|configuration loaded' || true

"$here/load.sh"

echo "run: waiting for OBI metrics to arrive in ClickHouse"
i=0
until curl -fsSG --data-urlencode 'query=sum(http_server_request_duration_count)' "$gateway_url/api/v1/query" \
  | jq -e '.data.result | length > 0' >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "run: OBI metrics did not arrive within 120s" >&2
    exit 1
  fi
  sleep 2
done

echo "run: waiting for OBI traces to arrive in ClickHouse"
now=$(date +%s)
start=$((now - 1800))
i=0
until curl -fsSG --data-urlencode 'q={resource.service.name="skopos-e2e-server"}' \
  --data-urlencode "start=$start" --data-urlencode "end=$now" \
  "$gateway_url/api/search" | jq -e '.traces | length > 0' >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "run: OBI traces did not arrive within 120s" >&2
    exit 1
  fi
  sleep 2
  now=$(date +%s)
  start=$((now - 1800))
done

SKOPOS_E2E_GATEWAY_URL="$gateway_url" "$here/verify.sh"
