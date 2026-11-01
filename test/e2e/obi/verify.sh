#!/bin/sh
# Verify that OBI-produced traces and metrics are readable through the Skopos
# gateway. Exits non-zero if any assertion fails.
set -eu

gateway=${SKOPOS_E2E_GATEWAY_URL:-http://127.0.0.1:18081}
app_service=${SKOPOS_E2E_SERVICE_NAME:-skopos-e2e-server}
now=$(date +%s)
start=$((now - 1800))

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "verify: missing required command: $1" >&2
    exit 1
  }
}
need curl
need jq

fail() {
  echo "verify: FAIL: $*" >&2
  exit 1
}

pass() {
  echo "verify: ok: $*"
}

# Wait for the gateway to report ClickHouse readiness.
i=0
until curl -fsS "$gateway/readyz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 90 ]; then
    fail "gateway did not become ready at $gateway/readyz"
  fi
  sleep 2
done
pass "gateway ready"

names=$(curl -fsS "$gateway/api/v1/label/__name__/values")
echo "$names" | jq -e '.status == "success"' >/dev/null || fail "label values response is not successful"
echo "$names" | jq -e --arg n 'http_server_request_duration_count' '.data | index($n) != null' >/dev/null \
  || fail "metric http_server_request_duration_count is missing"
echo "$names" | jq -e --arg n 'http_server_request_body_size_count' '.data | index($n) != null' >/dev/null \
  || fail "metric http_server_request_body_size_count is missing"
pass "OBI HTTP metric names are present via the Prometheus API"

instant=$(curl -fsSG --data-urlencode 'query=sum(http_server_request_duration_count)' "$gateway/api/v1/query")
echo "$instant" | jq -e '.status == "success" and (.data.result | length) > 0' >/dev/null \
  || fail "instant query returned no series: $instant"
value=$(echo "$instant" | jq -r '.data.result[0].value[1] // "0"' | cut -d. -f1)
[ "${value:-0}" -gt 0 ] || fail "instant request count is not positive: $instant"
pass "OBI request count is visible through /api/v1/query (value=$value)"

rng=$(curl -fsSG \
  --data-urlencode 'query=sum(rate(http_server_request_duration_count[1m]))' \
  --data-urlencode "start=$start" \
  --data-urlencode "end=$now" \
  --data-urlencode 'step=15s' \
  "$gateway/api/v1/query_range")
echo "$rng" | jq -e '.status == "success" and (.data.result | length) > 0' >/dev/null \
  || fail "range query returned no series: $rng"
nonzero=$(echo "$rng" | jq '[.data.result[0].values[]?[1] | tonumber | select(. > 0)] | length')
[ "$nonzero" -gt 0 ] || fail "range query has no positive rate samples: $rng"
pass "OBI request-rate range query returns $nonzero positive samples"

quantile=$(curl -fsSG \
  --data-urlencode 'query=histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_bucket[5m])))' \
  "$gateway/api/v1/query")
echo "$quantile" | jq -e '.status == "success" and (.data.result | length) > 0' >/dev/null \
  || fail "histogram_quantile query returned no series: $quantile"
pass "OBI duration histogram is queryable with histogram_quantile"

search=$(curl -fsSG \
  --data-urlencode "q={resource.service.name=\"$app_service\"}" \
  --data-urlencode "start=$start" \
  --data-urlencode "end=$now" \
  --data-urlencode 'limit=20' \
  "$gateway/api/search")
trace_count=$(echo "$search" | jq '.traces | length')
[ "$trace_count" -gt 0 ] || fail "Tempo search returned no traces: $search"
trace_id=$(echo "$search" | jq -r '[.traces[] | select(.rootTraceName == "GET /hello")][0].traceID // .traces[0].traceID')
[ -n "$trace_id" ] && [ "$trace_id" != "null" ] || fail "Tempo search returned no usable trace ID"
pass "Tempo search found $trace_count traces for service $app_service (sample=$trace_id)"

trace=$(curl -fsS "$gateway/api/traces/$trace_id")
span_count=$(echo "$trace" | jq '[.resourceSpans[]?.scopeSpans[]?.spans[]?] | length')
[ "$span_count" -gt 0 ] || fail "trace-by-ID returned no spans for $trace_id: $trace"
pass "Tempo trace-by-ID returned $span_count span(s)"

curl -fsS -o /dev/null -H 'Accept: application/protobuf' "$gateway/api/v2/traces/$trace_id" \
  || fail "Tempo v2 protobuf trace-by-ID failed for $trace_id"
pass "Tempo v2 protobuf trace-by-ID succeeded"

tags=$(curl -fsSG \
  --data-urlencode "start=$start" \
  --data-urlencode "end=$now" \
  "$gateway/api/v2/search/tags")
echo "$tags" | jq -e '.scopes[] | select(.name == "resource") | .tags | index("service.name") != null' >/dev/null \
  || fail "Tempo tag search does not expose resource service.name: $tags"
pass "Tempo tag search exposes resource service.name"

echo "verify: all OBI end-to-end assertions passed"
