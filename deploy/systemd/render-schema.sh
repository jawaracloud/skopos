#!/bin/sh
# Render the chart's ClickHouse schema with local-only storage for a
# non-Kubernetes/systemd install. Writes SQL to stdout unless an output path is
# given. This is the default schema; tiered storage requires the Helm/S3
# configuration described in docs/storage.md.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
src="$root/deploy/helm/skopos/files/schema.sql"
out=${1:-}

render() {
  sed \
    -e "s|{{- if .Values.clickhouse.storage.enabled }}, storage_policy = 'tiered'{{- end }}||g" \
    "$src" \
    | awk '
        /{{- if \.Values\.clickhouse\.storage\.enabled }}/ { in_block = 1; next }
        in_block && /{{- else }}/ { in_block = 0; keep_else = 1; next }
        in_block { next }
        keep_else && /{{- end }}/ { keep_else = 0; next }
        { print }
      ' \
    | sed \
        -e 's|{{ .Values.clickhouse.database }}|otel|g' \
        -e 's|{{ .Values.clickhouse.tables.traces }}|otel_traces|g' \
        -e 's|{{ .Values.clickhouse.tables.logs }}|otel_logs|g' \
        -e 's|{{ .Values.clickhouse.tables.metricsGauge }}|otel_metrics_gauge|g' \
        -e 's|{{ .Values.clickhouse.tables.metricsSum }}|otel_metrics_sum|g' \
        -e 's|{{ .Values.clickhouse.tables.metricsHistogram }}|otel_metrics_histogram|g' \
        -e 's|{{ .Values.clickhouse.tables.metricsExponentialHistogram }}|otel_metrics_exponential_histogram|g' \
        -e 's|{{ .Values.clickhouse.tables.metricsSummary }}|otel_metrics_summary|g' \
        -e 's|{{ .Values.clickhouse.ttl.tracesDays }}|7|g' \
        -e 's|{{ .Values.clickhouse.ttl.logsDays }}|7|g' \
        -e 's|{{ .Values.clickhouse.ttl.metricsDays }}|30|g'
}

if [ -n "$out" ]; then
  render >"$out"
  echo "rendered $out"
else
  render
fi
