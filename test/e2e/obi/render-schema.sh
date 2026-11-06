#!/bin/sh
# Render the Helm chart's ClickHouse DDL with the chart defaults used by the
# Docker Compose harness. The harness intentionally reuses the chart schema so
# a schema regression breaks the end-to-end test instead of a private copy.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../../.." && pwd)
src="$root/deploy/helm/skopos/files/schema.sql"
migrate_src="$root/deploy/helm/skopos/files/migrate.sh"
out="$here/.render/schema.sql"
migrate_out="$here/.render/migrate.sh"

mkdir -p "$here/.render"

# The e2e harness uses local-only storage. Drop the storage-enabled branch and
# the inline storage_policy fragment, then substitute the chart defaults.
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
      -e 's|{{ .Values.clickhouse.ttl.metricsDays }}|30|g' \
      >"$out"

sed \
  -e 's|{{ .Values.clickhouse.database }}|otel|g' \
  -e 's|{{ .Values.clickhouse.tables.traces }}|otel_traces|g' \
  -e 's|{{ .Values.clickhouse.tables.logs }}|otel_logs|g' \
  -e 's|{{ .Values.clickhouse.tables.metricsGauge }}|otel_metrics_gauge|g' \
  -e 's|{{ .Values.clickhouse.tables.metricsSum }}|otel_metrics_sum|g' \
  -e 's|{{ .Values.clickhouse.tables.metricsHistogram }}|otel_metrics_histogram|g' \
  -e 's|{{ .Values.clickhouse.tables.metricsExponentialHistogram }}|otel_metrics_exponential_histogram|g' \
  -e 's|{{ .Values.clickhouse.tables.metricsSummary }}|otel_metrics_summary|g' \
  "$migrate_src" >"$migrate_out"

if grep -q '{{' "$out" "$migrate_out"; then
  echo "render-schema: unreplaced Helm placeholder in $out or $migrate_out" >&2
  grep -n '{{' "$out" "$migrate_out" >&2
  exit 1
fi

sh -n "$migrate_out"
echo "rendered $out and $migrate_out"
