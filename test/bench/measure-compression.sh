#!/bin/sh
# Print ClickHouse on-disk and per-column compression for the benchmark tables.
set -eu

compose() {
  docker compose -p skopos-bench -f "$(dirname "$0")/docker-compose.yml" "$@"
}

compose exec -T clickhouse clickhouse-client --query "
SELECT
  p.table AS table,
  p.rows AS rows,
  p.bytes_on_disk AS on_disk_bytes,
  round(p.bytes_on_disk / p.rows, 2) AS bytes_per_row_on_disk,
  round(c.data_compressed_bytes / p.rows, 2) AS data_bytes_per_row,
  round(c.data_uncompressed_bytes / c.data_compressed_bytes, 2) AS compression_ratio
FROM
(
  SELECT table, sum(rows) AS rows, sum(bytes_on_disk) AS bytes_on_disk
  FROM system.parts
  WHERE database='otel' AND active
  GROUP BY table
) AS p
LEFT JOIN
(
  SELECT table,
         sum(data_compressed_bytes) AS data_compressed_bytes,
         sum(data_uncompressed_bytes) AS data_uncompressed_bytes
  FROM system.columns
  WHERE database='otel'
  GROUP BY table
) AS c USING table
ORDER BY p.table
FORMAT Markdown"

echo
echo "Per-signal totals (main tables only):"
compose exec -T clickhouse clickhouse-client --query "
SELECT
  multiIf(table LIKE 'otel_metrics_%', 'metrics', table = 'otel_logs', 'logs', table = 'otel_traces', 'traces', 'other') AS signal,
  sum(row_count) AS rows,
  sum(bytes_on_disk) AS on_disk_bytes,
  round(sum(bytes_on_disk) / sum(row_count), 2) AS bytes_per_row_on_disk,
  round(sum(data_compressed_bytes) / sum(row_count), 2) AS data_bytes_per_row
FROM
(
  SELECT p.table AS table, p.rows AS row_count, p.bytes_on_disk AS bytes_on_disk,
         c.data_compressed_bytes AS data_compressed_bytes
  FROM
  (
    SELECT table, sum(rows) AS rows, sum(bytes_on_disk) AS bytes_on_disk
    FROM system.parts WHERE database='otel' AND active GROUP BY table
  ) AS p
  LEFT JOIN
  (
    SELECT table, sum(data_compressed_bytes) AS data_compressed_bytes
    FROM system.columns WHERE database='otel' GROUP BY table
  ) AS c USING table
  WHERE p.table LIKE 'otel_metrics_%' OR p.table IN ('otel_logs', 'otel_traces')
)
GROUP BY signal
ORDER BY signal
FORMAT Markdown"
