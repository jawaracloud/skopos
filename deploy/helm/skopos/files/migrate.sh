#!/bin/sh
# Convert pre-dedup MergeTree signal tables to ReplacingMergeTree.
#
# The init Job applies schema.sql first and then runs this script on every
# install and upgrade. Fresh installs are already ReplacingMergeTree, so this
# is a no-op. On an upgrade each old table is renamed aside, schema.sql
# recreates it with the replay-dedup engine and key, and the old rows are
# copied back. The new table is live before the copy starts, so ingest keeps
# working; the Collector retries inserts during the short rename window.
#
# Re-running the Job is safe. A table whose engine is already Replacing is
# skipped, and a leftover __skopos_pre_dedup table means an earlier run was
# interrupted: the copy is repeated and the leftover is dropped. Repeated
# copies collapse on the ReplacingMergeTree key.
set -eu

database='{{ .Values.clickhouse.database }}'
traces='{{ .Values.clickhouse.tables.traces }}'
logs='{{ .Values.clickhouse.tables.logs }}'
metrics_gauge='{{ .Values.clickhouse.tables.metricsGauge }}'
metrics_sum='{{ .Values.clickhouse.tables.metricsSum }}'
metrics_histogram='{{ .Values.clickhouse.tables.metricsHistogram }}'
metrics_exponential_histogram='{{ .Values.clickhouse.tables.metricsExponentialHistogram }}'
metrics_summary='{{ .Values.clickhouse.tables.metricsSummary }}'
trace_id_ts="${traces}_trace_id_ts"
trace_id_ts_mv="${traces}_trace_id_ts_mv"

legacy_suffix='__skopos_pre_dedup'

cli() {
  clickhouse-client --host "$CLICKHOUSE_HOST" --port 9000 \
    --user "$CLICKHOUSE_ADMIN_USER" --password "$CLICKHOUSE_ADMIN_PASSWORD" "$@"
}

engine() {
  cli --query "SELECT engine FROM system.tables WHERE database = '${database}' AND name = '$1'"
}

exists() {
  [ -n "$(engine "$1")" ]
}

apply_schema() {
  cli --multiquery < /schema/schema.sql
}

backfill() {
  table="$1"
  legacy="${table}${legacy_suffix}"
  if ! exists "$legacy"; then
    return 0
  fi
  current=$(engine "$table")
  if [ "$current" != 'ReplacingMergeTree' ]; then
    echo "migrate: ${legacy} exists but ${table} is '${current}', refusing to continue" >&2
    exit 1
  fi
  echo "migrate: backfilling ${table} from ${legacy}"
  cli --query "INSERT INTO \`${database}\`.\`${table}\` SELECT * FROM \`${database}\`.\`${legacy}\`"
  cli --query "DROP TABLE \`${database}\`.\`${legacy}\`"
}

migrate() {
  table="$1"
  legacy="${table}${legacy_suffix}"
  current=$(engine "$table")
  case "$current" in
    ReplacingMergeTree)
      backfill "$table"
      ;;
    MergeTree)
      if exists "$legacy"; then
        echo "migrate: both ${table} (MergeTree) and ${legacy} exist" >&2
        exit 1
      fi
      echo "migrate: converting ${table} to ReplacingMergeTree"
      cli --query "RENAME TABLE \`${database}\`.\`${table}\` TO \`${database}\`.\`${legacy}\`"
      apply_schema
      backfill "$table"
      ;;
    '')
      if exists "$legacy"; then
        echo "migrate: recreating ${table} after an interrupted rename"
        apply_schema
        backfill "$table"
      else
        echo "migrate: ${table} does not exist" >&2
        exit 1
      fi
      ;;
    *)
      echo "migrate: unsupported engine '${current}' for ${table}" >&2
      exit 1
      ;;
  esac
}

# The trace index is a materialized-view target, not a query table. Its view is
# attached to the source table by UUID, so drop it before either trace table is
# renamed and let schema.sql recreate it against the new tables.
ts_legacy="${trace_id_ts}${legacy_suffix}"
traces_legacy="${traces}${legacy_suffix}"
traces_engine=$(engine "$traces")
ts_engine=$(engine "$trace_id_ts")

if [ "$traces_engine" = 'MergeTree' ] || [ "$ts_engine" = 'MergeTree' ] || \
  exists "$traces_legacy" || { [ -z "$ts_engine" ] && exists "$ts_legacy"; }; then
  echo "migrate: deduplicating trace tables"
  cli --query "DROP VIEW IF EXISTS \`${database}\`.\`${trace_id_ts_mv}\`"

  if [ "$ts_engine" = 'MergeTree' ]; then
    cli --query "RENAME TABLE \`${database}\`.\`${trace_id_ts}\` TO \`${database}\`.\`${ts_legacy}\`"
  fi

  migrate "$traces"
  apply_schema

  if exists "$ts_legacy"; then
    # The old index was derived from the old traces table. Drop it; the
    # rebuild below recreates the current projection.
    cli --query "DROP TABLE \`${database}\`.\`${ts_legacy}\`"
  fi

  # Make the trace index match the new traces table. This also repairs an
  # earlier run that stopped after truncating it. The materialized view is
  # already recreated and serves new inserts after this point.
  echo "migrate: rebuilding ${trace_id_ts} from ${traces}"
  cli --query "TRUNCATE TABLE \`${database}\`.\`${trace_id_ts}\`"
  cli --query "INSERT INTO \`${database}\`.\`${trace_id_ts}\` (TraceId, Start, End) SELECT TraceId, min(Timestamp) AS Start, max(Timestamp) AS End FROM \`${database}\`.\`${traces}\` WHERE TraceId != '' GROUP BY TraceId"
fi

for table in "$logs" "$metrics_gauge" "$metrics_sum" "$metrics_histogram" \
  "$metrics_exponential_histogram" "$metrics_summary"; do
  migrate "$table"
done

echo "migrate: replay deduplication schema is ready"
