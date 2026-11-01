#!/usr/bin/env bash
# Real ClickHouse + RustFS test for tiered storage and BACKUP/RESTORE.
#
# Requires: docker with compose, helm, curl.
#   PATH=/tmp/opencode/linux-amd64:$PATH ./test/integration/s3/run.sh
#
# Set SKOPOS_S3_IT_KEEP=1 to keep containers and volumes for inspection.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../../.." && pwd)"
PROJECT="skopos-s3-it"
HELM_BIN="${HELM_BIN:-helm}"
CH_PASSWORD="test-password"
AWS_ARGS=(--endpoint-url http://rustfs:9000)

if ! command -v "${HELM_BIN}" >/dev/null 2>&1; then
  echo "helm not found; set HELM_BIN to the helm binary" >&2
  exit 1
fi

compose() {
  docker compose -p "${PROJECT}" -f "${HERE}/docker-compose.yml" "$@"
}

cleanup() {
  status=$?
  if [ "${SKOPOS_S3_IT_KEEP:-0}" = "1" ]; then
    echo "kept ${PROJECT} containers and volumes (SKOPOS_S3_IT_KEEP=1)"
    return
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  exit "${status}"
}
trap cleanup EXIT

ch() {
  compose exec -T clickhouse clickhouse-client --password "${CH_PASSWORD}" "$@"
}

aws_s3() {
  compose run --rm --no-deps --entrypoint aws buckets "${AWS_ARGS[@]}" "$@"
}

fail() {
  echo "FAIL: $*" >&2
  compose ps >&2 || true
  compose logs --no-color --tail=120 >&2 || true
  exit 1
}

helm_values=(
  template skopos "${ROOT}/deploy/helm/skopos" --namespace observability
  --set clickhouse.storage.enabled=true
  --set clickhouse.storage.hotDays=1
  --set clickhouse.storage.totalDays=30
  --set clickhouse.storage.cold.endpoint=http://rustfs:9000
  --set clickhouse.storage.cold.bucket=skopos-cold
  --set clickhouse.storage.cold.prefix=skopos/cold
  --set clickhouse.storage.cold.accessKeyId=unused
  --set clickhouse.storage.cold.secretAccessKey=unused
)

echo "==> rendering the chart storage configuration"
"${HELM_BIN}" "${helm_values[@]}" \
  --show-only templates/clickhouse-storage-configmap.yaml \
  | sed -n '/^  10-storage.xml: |$/,$p' \
  | tail -n +2 \
  | sed 's/^    //' \
  > /tmp/skopos-s3-storage.xml

grep -q '<storage_configuration>' /tmp/skopos-s3-storage.xml \
  || fail "rendered storage configuration is empty"
export SKOPOS_S3_STORAGE_XML=/tmp/skopos-s3-storage.xml

echo "==> starting RustFS, buckets, and ClickHouse"
compose up -d

for _ in $(seq 1 120); do
  if curl -fsS http://127.0.0.1:18123/ping >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS http://127.0.0.1:18123/ping >/dev/null 2>&1 || fail "ClickHouse did not become ready"
echo "ok: ClickHouse is ready"

echo "==> rendering the chart schema with tiered storage enabled"
"${HELM_BIN}" "${helm_values[@]}" \
  --show-only templates/clickhouse-schema-configmap.yaml \
  | sed -n '/^  schema.sql: |$/,$p' \
  | tail -n +2 \
  | sed 's/^    //' \
  > /tmp/skopos-s3-schema.sql

grep -q "storage_policy = 'tiered'" /tmp/skopos-s3-schema.sql \
  || fail "rendered schema does not use the tiered policy"
grep -q "TO VOLUME 'cold'" /tmp/skopos-s3-schema.sql \
  || fail "rendered schema does not move parts to the cold volume"
grep -q 'INDEX idx_body Body TYPE text' /tmp/skopos-s3-schema.sql \
  || fail "rendered schema is missing idx_body"

echo "==> applying the full rendered schema"
ch --query "DROP DATABASE IF EXISTS otel"
ch --query "DROP DATABASE IF EXISTS otel_restore"
ch --multiquery < /tmp/skopos-s3-schema.sql

echo "==> moving an old log part to the cold volume"
ch --query "SYSTEM STOP MERGES otel.otel_logs"
ch --query "INSERT INTO otel.otel_logs (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, EventName) VALUES (now64(9) - INTERVAL 10 DAY, 'trace-old', 'span-old', 'INFO', 9, 's3-it', 'CaseSensitiveToken tiered-storage-test', 's3-it')"
ch --query "INSERT INTO otel.otel_logs (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, EventName) VALUES (now64(9), 'trace-new', 'span-new', 'INFO', 9, 's3-it', 'HotOnlyToken', 's3-it')"
ch --query "ALTER TABLE otel.otel_logs MATERIALIZE TTL"
ch --query "SYSTEM START MERGES otel.otel_logs"

cold_parts="$(ch --query "SELECT count() FROM system.parts WHERE database = 'otel' AND table = 'otel_logs' AND active AND disk_name = 'cold'")"
hot_parts="$(ch --query "SELECT count() FROM system.parts WHERE database = 'otel' AND table = 'otel_logs' AND active AND disk_name = 'hot'")"
[ "${cold_parts}" -ge 1 ] || fail "no active otel_logs part on the cold disk (cold=${cold_parts} hot=${hot_parts})"
[ "${hot_parts}" -ge 1 ] || fail "no active otel_logs part on the hot disk (cold=${cold_parts} hot=${hot_parts})"
echo "ok: otel_logs has ${cold_parts} cold part(s) and ${hot_parts} hot part(s)"

total="$(ch --query "SELECT count() FROM otel.otel_logs")"
cold_row="$(ch --query "SELECT count() FROM otel.otel_logs WHERE Body = 'CaseSensitiveToken tiered-storage-test'")"
[ "${total}" = "2" ] || fail "expected 2 log rows after the TTL move, got ${total}"
[ "${cold_row}" = "1" ] || fail "cold row was not queryable after the TTL move"
echo "ok: cold part is queryable through ClickHouse (rows=${total})"

echo "==> verifying the case-sensitive idx_body index is usable"
plan="$(ch --query "EXPLAIN indexes = 1 SELECT count() FROM otel.otel_logs WHERE match(Body, 'CaseSensitiveToken tiered-storage-test')")"
printf '%s\n' "${plan}" | grep -q 'idx_body' \
  || fail "query planner did not use idx_body for match(Body, ...)"
echo "ok: EXPLAIN indexes = 1 uses idx_body"

echo "==> backing up the database to the separate backup bucket/prefix"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
destination="http://rustfs:9000/skopos-backup/skopos/backups/otel/${timestamp}"
ch --query "BACKUP DATABASE otel TO S3('${destination}', 'rustfsadmin', 'rustfsadmin')"

object_count="$(aws_s3 s3 ls "s3://skopos-backup/skopos/backups/otel/${timestamp}/" --recursive | wc -l)"
[ "${object_count}" -ge 1 ] || fail "backup object prefix is empty"
echo "ok: backup wrote ${object_count} object(s) to skopos-backup/skopos/backups/otel/${timestamp}/"

echo "==> restoring the backup under a new database name"
ch --query "RESTORE DATABASE otel AS otel_restore FROM S3('${destination}', 'rustfsadmin', 'rustfsadmin')"
restored="$(ch --query "SELECT count() FROM otel_restore.otel_logs")"
[ "${restored}" = "${total}" ] || fail "restored row count ${restored} does not match source ${total}"
echo "ok: restored otel_restore.otel_logs with ${restored} row(s)"

echo "PASS: tiered storage, cold-part query, idx_body, BACKUP, and RESTORE work against RustFS"
