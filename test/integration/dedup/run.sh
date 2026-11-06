#!/bin/sh
# Apply the chart's ClickHouse schema to a real server and verify that replay
# duplicates collapse on the signal tables while distinct rows survive.
#
# Requires: curl and the e2e schema renderer (sed/awk). Runs in the CI
# integration job against the ClickHouse service on 127.0.0.1:8123.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../../.." && pwd)
base="${CLICKHOUSE_URL:-http://127.0.0.1:8123}"
user="${CLICKHOUSE_USER:-default}"
password="${CLICKHOUSE_PASSWORD:-}"
database="skopos_dedup_$(date +%s)_$$"

"$root/test/e2e/obi/render-schema.sh" >/dev/null
schema="$root/test/e2e/obi/.render/schema.sql"

q() {
  curl -fsS -u "${user}:${password}" "${base}/" --data-binary "$1"
}

cleanup() {
  q "DROP DATABASE IF EXISTS \`${database}\` SYNC" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# The renderer produces the chart defaults under the `otel` database. Reuse the
# DDL under a unique database so concurrent tests cannot collide.
sed "s/\`otel\`/\`${database}\`/g" "$schema" > /tmp/skopos-dedup-schema.sql

q "CREATE DATABASE \`${database}\`"
# Full-line comments can contain semicolons; strip them before splitting the
# remaining DDL into one HTTP request per statement.
sed '/^[[:space:]]*--/d' /tmp/skopos-dedup-schema.sql > /tmp/skopos-dedup-schema-nocomments.sql
awk 'BEGIN { RS = ";" } {
  gsub(/\n/, " ")
  gsub(/^[ \t]+|[ \t]+$/, "")
  if (length($0) > 0) print
}' /tmp/skopos-dedup-schema-nocomments.sql |
  while IFS= read -r statement; do
    q "$statement"
  done

engines=$(q "SELECT count() FROM system.tables WHERE database = '${database}' AND engine = 'ReplacingMergeTree'")
[ "$engines" = "8" ] || {
  echo "FAIL: expected 8 ReplacingMergeTree tables, got ${engines}" >&2
  exit 1
}

# Logs: the same record with different attribute insertion order is a replay
# duplicate; a different body is a distinct record.
q "INSERT INTO \`${database}\`.\`otel_logs\` (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, EventName, LogAttributes) VALUES ('2026-11-01 00:00:00.123456789','t1','s1','INFO',9,'svc','same','evt',{'a':'1','b':'2'})"
q "INSERT INTO \`${database}\`.\`otel_logs\` (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, EventName, LogAttributes) VALUES ('2026-11-01 00:00:00.123456789','t1','s1','INFO',9,'svc','same','evt',{'b':'2','a':'1'})"
q "INSERT INTO \`${database}\`.\`otel_logs\` (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, EventName) VALUES ('2026-11-01 00:00:00.123456789','t1','s1','INFO',9,'svc','different','evt')"

logs_raw=$(q "SELECT count() FROM \`${database}\`.\`otel_logs\`")
logs_final=$(q "SELECT count() FROM \`${database}\`.\`otel_logs\` FINAL")
[ "$logs_raw" = "3" ] && [ "$logs_final" = "2" ] || {
  echo "FAIL: logs raw=${logs_raw} final=${logs_final}, want 3/2" >&2
  exit 1
}

# Traces: (TraceId, SpanId) is the replay identity.
q "INSERT INTO \`${database}\`.\`otel_traces\` (Timestamp, TraceId, SpanId, SpanName, ServiceName, Duration) VALUES ('2026-11-01 00:00:00.1','trace1','span1','op','svc',10)"
q "INSERT INTO \`${database}\`.\`otel_traces\` (Timestamp, TraceId, SpanId, SpanName, ServiceName, Duration) VALUES ('2026-11-01 00:00:00.1','trace1','span1','op','svc',10)"
q "INSERT INTO \`${database}\`.\`otel_traces\` (Timestamp, TraceId, SpanId, SpanName, ServiceName, Duration) VALUES ('2026-11-01 00:00:00.1','trace1','span2','op','svc',10)"

traces_raw=$(q "SELECT count() FROM \`${database}\`.\`otel_traces\`")
traces_final=$(q "SELECT count() FROM \`${database}\`.\`otel_traces\` FINAL")
[ "$traces_raw" = "3" ] && [ "$traces_final" = "2" ] || {
  echo "FAIL: traces raw=${traces_raw} final=${traces_final}, want 3/2" >&2
  exit 1
}

# Metrics: a replay duplicate keeps the same value; a different value is a
# distinct data point.
q "INSERT INTO \`${database}\`.\`otel_metrics_gauge\` (ServiceName, MetricName, TimeUnix, StartTimeUnix, Value, Attributes) VALUES ('svc','m','2026-11-01 00:00:00','2026-11-01 00:00:00',1.5,{'k':'v'})"
q "INSERT INTO \`${database}\`.\`otel_metrics_gauge\` (ServiceName, MetricName, TimeUnix, StartTimeUnix, Value, Attributes) VALUES ('svc','m','2026-11-01 00:00:00','2026-11-01 00:00:00',1.5,{'k':'v'})"
q "INSERT INTO \`${database}\`.\`otel_metrics_gauge\` (ServiceName, MetricName, TimeUnix, StartTimeUnix, Value, Attributes) VALUES ('svc','m','2026-11-01 00:00:00','2026-11-01 00:00:00',2.5,{'k':'v'})"

metrics_raw=$(q "SELECT count() FROM \`${database}\`.\`otel_metrics_gauge\`")
metrics_final=$(q "SELECT count() FROM \`${database}\`.\`otel_metrics_gauge\` FINAL")
[ "$metrics_raw" = "3" ] && [ "$metrics_final" = "2" ] || {
  echo "FAIL: gauge raw=${metrics_raw} final=${metrics_final}, want 3/2" >&2
  exit 1
}

# The trace lookup index is derived; verify the materialized view survived the
# schema application and still receives inserts.
q "INSERT INTO \`${database}\`.\`otel_traces\` (Timestamp, TraceId, SpanId, SpanName, ServiceName, Duration) VALUES ('2026-11-01 00:00:01.1','trace2','span3','op','svc',20)"
trace_index=$(q "SELECT count() FROM \`${database}\`.\`otel_traces_trace_id_ts\`")
[ "$trace_index" -ge 2 ] || {
  echo "FAIL: trace id index has ${trace_index} rows, want at least 2" >&2
  exit 1
}

echo "PASS: replay duplicates collapse on traces, logs, and metrics; distinct rows survive"
