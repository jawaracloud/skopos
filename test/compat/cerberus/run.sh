#!/bin/sh
# Three-way differential gate: real Prometheus + cerberus + Skopos.
#
#   test/compat/cerberus/run.sh [--strict] [--verbose]
#
# The default gate fails when Skopos diverges from the reference, when a
# cerberus divergence is not in known-cerberus-gaps.json, or when the baseline
# is stale. Known cerberus omissions are printed as CERBERUS_GAP and summarised;
# pass --strict to make those fail too. Set CERBERUS_COMPAT_KEEP=1 to keep the
# stack running for inspection.
set -eu

cd "$(dirname "$0")/../../.."

COMPOSE_FILE=test/compat/cerberus/docker-compose.yml
COMPOSE_PROJECT=skopos-cerberus-compat
RENDER=test/compat/cerberus/.render
COMPOSE="docker compose -p $COMPOSE_PROJECT -f $COMPOSE_FILE"

CH_HTTP_PORT=${CERBERUS_COMPAT_CH_HTTP_PORT:-18131}
CH_NATIVE_PORT=${CERBERUS_COMPAT_CH_NATIVE_PORT:-19031}
CH_USER=${CERBERUS_COMPAT_CH_USER:-default}
CH_PASSWORD=${CERBERUS_COMPAT_CH_PASSWORD:-skopos-cerberus}
CERBERUS_PORT=${CERBERUS_COMPAT_CERBERUS_PORT:-18092}
PROMETHEUS_PORT=${CERBERUS_COMPAT_PROMETHEUS_PORT:-18093}
SKOPOS_PORT=${CERBERUS_COMPAT_SKOPOS_PORT:-18088}

CH_URL="http://127.0.0.1:$CH_HTTP_PORT"
CERBERUS_URL="http://127.0.0.1:$CERBERUS_PORT"
PROMETHEUS_URL="http://127.0.0.1:$PROMETHEUS_PORT"
SKOPOS_URL="http://127.0.0.1:$SKOPOS_PORT"

export CERBERUS_COMPAT_CH_HTTP_PORT=$CH_HTTP_PORT
export CERBERUS_COMPAT_CH_NATIVE_PORT=$CH_NATIVE_PORT
export CERBERUS_COMPAT_CH_USER=$CH_USER
export CERBERUS_COMPAT_CH_PASSWORD=$CH_PASSWORD
export CERBERUS_COMPAT_CERBERUS_PORT=$CERBERUS_PORT
export CERBERUS_COMPAT_PROMETHEUS_PORT=$PROMETHEUS_PORT

SKOPOS_PID=
KEEP=${CERBERUS_COMPAT_KEEP:-0}

cleanup() {
    status=$?
    if [ -n "$SKOPOS_PID" ]; then
        kill "$SKOPOS_PID" 2>/dev/null || true
        wait "$SKOPOS_PID" 2>/dev/null || true
    fi
    if [ "$KEEP" != "1" ]; then
        $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
        rm -rf "$RENDER"
    else
        echo "kept: $COMPOSE"
        echo "  clickhouse: $CH_URL"
        echo "  cerberus:   $CERBERUS_URL"
        echo "  prometheus: $PROMETHEUS_URL"
        echo "  skopos:     $SKOPOS_URL"
        echo "  logs:       $RENDER/skopos.log"
    fi
    exit "$status"
}
trap cleanup EXIT INT TERM

wait_http() {
    name=$1
    url=$2
    attempts=${3:-90}
    i=0
    while [ "$i" -lt "$attempts" ]; do
        if curl -fsS -o /dev/null "$url" 2>/dev/null; then
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done
    echo "timed out waiting for $name at $url" >&2
    return 1
}

rm -rf "$RENDER"
mkdir -p "$RENDER"

echo "==> start clickhouse"
$COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
$COMPOSE up -d clickhouse
i=0
while [ "$i" -lt 120 ]; do
    if curl -fsS -u "$CH_USER:$CH_PASSWORD" -o /dev/null "$CH_URL/?query=SELECT%201" 2>/dev/null; then
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$i" -ge 120 ]; then
    echo "clickhouse did not accept authenticated queries" >&2
    $COMPOSE logs clickhouse >&2 || true
    exit 1
fi

echo "==> seed the shared corpus"
go run ./test/compat/cerberus/seed \
    --clickhouse-url "$CH_URL" \
    --clickhouse-user "$CH_USER" \
    --clickhouse-password "$CH_PASSWORD" \
    --prometheus-openmetrics "$RENDER/metrics.om"

echo "==> build the Prometheus reference TSDB"
$COMPOSE run --rm --no-deps --user 0 \
    -v "$PWD/$RENDER:/render:ro" \
    --entrypoint promtool prometheus \
    tsdb create-blocks-from openmetrics /render/metrics.om /prometheus

echo "==> start cerberus and prometheus"
$COMPOSE up -d cerberus prometheus
wait_http "cerberus" "$CERBERUS_URL/readyz" 90
wait_http "prometheus" "$PROMETHEUS_URL/-/ready" 90

echo "==> build and start skopos"
go build -mod=vendor -o "$RENDER/skopos" ./cmd/gateway
SKOPOS_LISTEN_ADDR="127.0.0.1:$SKOPOS_PORT" \
SKOPOS_CLICKHOUSE_URL="$CH_URL" \
SKOPOS_CLICKHOUSE_DATABASE=otel \
SKOPOS_CLICKHOUSE_USER="$CH_USER" \
SKOPOS_CLICKHOUSE_PASSWORD="$CH_PASSWORD" \
SKOPOS_LOG_LEVEL=warn \
    "$RENDER/skopos" >"$RENDER/skopos.log" 2>&1 &
SKOPOS_PID=$!
wait_http "skopos" "$SKOPOS_URL/readyz" 90

echo "==> three-way comparison"
PROMETHEUS_URL="$PROMETHEUS_URL" \
SKOPOS_URL="$SKOPOS_URL" \
CERBERUS_URL="$CERBERUS_URL" \
    python3 test/compat/cerberus/harness.py "$@"
