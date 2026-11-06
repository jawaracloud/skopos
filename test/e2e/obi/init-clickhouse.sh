#!/bin/sh
# Apply the chart DDL and create the read-only gateway user. This is the
# Compose equivalent of the chart's clickhouse-init Job.
set -eu

export HOME=/tmp

admin_user=${CLICKHOUSE_ADMIN_USER:-default}
admin_password=${CLICKHOUSE_ADMIN_PASSWORD:-}
gateway_user=${SKOPOS_GATEWAY_USER:-skopos_gateway}
gateway_password=${SKOPOS_GATEWAY_PASSWORD:-skopos-e2e-password}
database=${SKOPOS_DATABASE:-otel}

cli() {
  clickhouse-client --host clickhouse --port 9000 \
    --user "$admin_user" --password "$admin_password" "$@"
}

i=0
until cli --query "SELECT 1" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 150 ]; then
    echo "clickhouse-init: ClickHouse did not become ready" >&2
    exit 1
  fi
  echo "clickhouse-init: waiting for ClickHouse"
  sleep 2
done

cli --multiquery < /schema/schema.sql
CLICKHOUSE_HOST=clickhouse \
CLICKHOUSE_ADMIN_USER="$admin_user" \
CLICKHOUSE_ADMIN_PASSWORD="$admin_password" \
  sh /schema/migrate.sh

cli --param_password="$gateway_password" --query "CREATE USER IF NOT EXISTS ${gateway_user} IDENTIFIED WITH sha256_password BY {password:String} SETTINGS readonly = 2, max_execution_time = 30, max_memory_usage = 1073741824, max_rows_to_read = 10000000, max_result_rows = 100000"
cli --param_password="$gateway_password" --query "ALTER USER ${gateway_user} IDENTIFIED WITH sha256_password BY {password:String} SETTINGS readonly = 2, max_execution_time = 30, max_memory_usage = 1073741824, max_rows_to_read = 10000000, max_result_rows = 100000"
cli --query "GRANT SELECT ON \`${database}\`.* TO ${gateway_user}"
echo "clickhouse-init: schema and gateway user ready"
