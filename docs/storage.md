# Tiered storage and backups

Skopos stores telemetry in ClickHouse. Two independent, optional settings
control where older data lives and how it is protected:

- `clickhouse.storage.*` enables ClickHouse tiered storage: recent parts stay
  on the local PVC and older parts move to an S3-compatible bucket. Cold parts
  remain queryable through ClickHouse.
- `clickhouse.backup.*` creates a Kubernetes CronJob that copies the database
  to a separate S3-compatible destination with `BACKUP DATABASE`.

Both are disabled by default. A default install keeps the local-only PVC and
TTL/delete behavior and does not require any S3 credentials.

## What these features are not

- **TTL deletes are not backups.** When the delete rule fires, ClickHouse
  removes rows/parts. There is no copy elsewhere unless a backup exists.
- **The cold tier is not a backup.** Moving a part to S3 keeps it in the live
  table and queryable by the gateway. Deleting that part (TTL or a manual drop)
  deletes the live copy.
- **Backups are not queried by the gateway.** The gateway never reads the
  backup destination. To query backed-up data, restore it into a database
  first.
- **There is no Skopos/Thanos-style block compactor.** ClickHouse owns merges
  and compaction for both hot and cold volumes. The chart does not run a
  separate compactor and does not rewrite S3 objects behind ClickHouse's back.
  ClickHouse merges parts on the S3 disk using its own background merge
  scheduler.
- **The cold tier and backups are not the durability checkpoint.** The
  checkpoint described in [checkpoint.md](checkpoint.md) is a separate recent
  window of raw OTLP payloads for replay after ClickHouse loss; the gateway
  never queries it.

## Replay deduplication

The signal tables use `ReplacingMergeTree` so replaying the checkpoint is
idempotent in ClickHouse:

- `otel_traces` ends its sort key with the OTel span identity
  `(TraceId, SpanId)`.
- `otel_logs` and the metric tables add a 128-bit materialized
  `_skopos_row_hash` computed from the whole row. Attribute maps are hashed
  with `mapSort`, so attribute insertion order cannot split a duplicate.
  `SELECT *` does not include materialized columns, and the Collector's insert
  does not name them.
- The gateway reads every signal table with `FINAL`, which applies the
  `ReplacingMergeTree` key immediately instead of waiting for a background
  merge. A gateway that reaches a pre-migration `MergeTree` table retries
  without `FINAL`, so reads remain available until the init Job converts it.

There is no OTel record id for a log or metric point. Two byte-identical rows
are therefore the same row and collapse to one; distinct rows keep distinct
hashes.

The ClickHouse init Job runs `schema.sql` and then `migrate.sh`. On a fresh
install both are no-ops beyond creating the current tables. On an upgrade the
Job renames each old `MergeTree` table to `<table>__skopos_pre_dedup`, lets
`schema.sql` recreate it with the dedup engine and key, and copies the old rows
back. The new table is live before the copy starts. The Collector retries
inserts during the short rename window, and the Job is safe to re-run after an
interruption. For a large table, raise
`clickhouse.initJob.activeDeadlineSeconds` for the upgrade. Stale
`__skopos_pre_dedup` tables mean the copy did not finish; the next init Job
resumes it.

## Tiered storage

### Values

```yaml
clickhouse:
  storage:
    enabled: true
    hotVolume: hot
    coldVolume: cold
    hotDays: 3
    totalDays: 30
    cacheSize: 10Gi
    cold:
      endpoint: http://rustfs.observability.svc:9000
      bucket: skopos-cold
      prefix: skopos/cold
      region: ""
      existingSecret: skopos-s3-credentials
      accessKeyId: ""
      secretAccessKey: ""
```

| Value | Meaning |
| --- | --- |
| `enabled` | Mount the storage configuration and use the `tiered` policy in the schema. |
| `hotVolume` | Name of the local hot disk and volume. Data goes under `/var/lib/clickhouse/disks/<hotVolume>/` on the PVC. Must not be `default` or `cold_s3`. |
| `coldVolume` | Name of the S3-backed cold volume. The TTL rule uses `TO VOLUME '<coldVolume>'`. Must not be `default` or `cold_s3`. |
| `hotDays` | Age at which a part is eligible to move to the cold volume. |
| `totalDays` | Age at which a part is deleted. Must be greater than `hotDays`. |
| `cacheSize` | Local file-cache limit in front of the S3 disk, on the ClickHouse PVC. |
| `cold.endpoint` | S3 scheme and host only, for example `http://rustfs:9000`. Do not include the bucket. |
| `cold.bucket` | Existing bucket. The chart does not create buckets. |
| `cold.prefix` | Root prefix inside the bucket for live cold parts. |
| `cold.region` | Optional S3 region. |
| `cold.existingSecret` | Secret with `access-key-id` and `secret-access-key`. Preferred. |
| `cold.accessKeyId`, `cold.secretAccessKey` | Inline credentials. The chart writes them into a Secret, not the ConfigMap. Use only for local tests. |

When `storage.enabled` is true, `clickhouse.ttl.*` is ignored and the single
`hotDays`/`totalDays` pair applies to every signal table. The trace-lookup
table (`<traces>_trace_id_ts`) uses the same policy so trace-by-ID lookups keep
working for cold traces.

### Generated ClickHouse configuration

The chart renders a `10-storage.xml` ConfigMap and mounts it at
`/etc/clickhouse-server/config.d/10-storage.xml` only when storage is enabled.
It defines:

- a local disk named after `hotVolume`, rooted at
  `/var/lib/clickhouse/disks/<hotVolume>/`;
- an S3 disk `cold_s3` at
  `<endpoint>/<bucket>/<prefix>/`;
- a cache disk named after `coldVolume` that wraps `cold_s3` with
  `cacheSize` as `max_size`;
- a storage policy `tiered` with the hot volume first, the built-in `default`
  volume second, and the cold volume last. The `default` volume is included
  only so ClickHouse accepts
  `ALTER TABLE ... MODIFY SETTING storage_policy = 'tiered'` on an existing
  table; new tables write to the hot volume because it is first.

Credentials are read from the `CLICKHOUSE_S3_ACCESS_KEY_ID` and
`CLICKHOUSE_S3_SECRET_ACCESS_KEY` environment variables, which are injected
from the S3 Secret. They are not written into the ConfigMap.

With storage enabled, the generated DDL ends with:

```sql
TTL <time> + INTERVAL <hotDays> DAY TO VOLUME '<coldVolume>',
    <time> + INTERVAL <totalDays> DAY DELETE
SETTINGS ..., storage_policy = 'tiered'
```

ClickHouse moves a part only when every row in it satisfies the move rule.
Moves happen in the background with merges; they are not instantaneous at the
exact `hotDays` boundary.

### Prepare the bucket and credentials

Create the bucket before installing or upgrading:

```sh
aws --endpoint-url http://rustfs:9000 s3 mb s3://skopos-cold
```

The bucket for live cold parts must exist and must not be the same
bucket/prefix as the backup destination. If you use `existingSecret`, create
it with the two expected keys:

```sh
kubectl -n observability create secret generic skopos-s3-credentials \
  --from-literal=access-key-id=... \
  --from-literal=secret-access-key=...
```

The chart refuses to render when storage is enabled without an endpoint,
bucket, prefix, and credentials.

### Enable tiered storage on a new install

```sh
helm install skopos ./deploy/helm/skopos -n observability --create-namespace \
  --set clickhouse.storage.enabled=true \
  --set clickhouse.storage.cold.endpoint=http://rustfs.observability.svc:9000 \
  --set clickhouse.storage.cold.bucket=skopos-cold \
  --set clickhouse.storage.cold.existingSecret=skopos-s3-credentials
```

The init job creates every table with `storage_policy = 'tiered'` and the
hot/cold TTL.

### Enable tiered storage on an existing install

`CREATE TABLE IF NOT EXISTS` does not modify existing tables. The upgrade that
mounts the storage configuration does not by itself move or re-policy data.
Do this in order:

1. Upgrade the chart with `clickhouse.storage.enabled=true` and valid
   credentials so the new ConfigMap is mounted and ClickHouse has the
   `tiered` policy. The init job skips the existing tables.
2. Change the policy and TTL on every signal table. Replace the example
   times with the same columns shown below. Run it as the ClickHouse admin:

```sql
ALTER TABLE otel.otel_traces
  MODIFY SETTING storage_policy = 'tiered';
ALTER TABLE otel.otel_traces
  MODIFY TTL toDateTime(Timestamp) + INTERVAL 3 DAY TO VOLUME 'cold',
             toDateTime(Timestamp) + INTERVAL 30 DAY DELETE;

ALTER TABLE otel.otel_traces_trace_id_ts
  MODIFY SETTING storage_policy = 'tiered';
ALTER TABLE otel.otel_traces_trace_id_ts
  MODIFY TTL toDateTime(Start) + INTERVAL 3 DAY TO VOLUME 'cold',
             toDateTime(Start) + INTERVAL 30 DAY DELETE;

ALTER TABLE otel.otel_logs
  MODIFY SETTING storage_policy = 'tiered';
ALTER TABLE otel.otel_logs
  MODIFY TTL toDateTime(Timestamp) + INTERVAL 3 DAY TO VOLUME 'cold',
             toDateTime(Timestamp) + INTERVAL 30 DAY DELETE;

ALTER TABLE otel.otel_metrics_gauge
  MODIFY SETTING storage_policy = 'tiered';
ALTER TABLE otel.otel_metrics_gauge
  MODIFY TTL toDateTime(TimeUnix) + INTERVAL 3 DAY TO VOLUME 'cold',
             toDateTime(TimeUnix) + INTERVAL 30 DAY DELETE;
```

Apply the same two statements to `otel_metrics_sum`,
`otel_metrics_histogram`, `otel_metrics_exponential_histogram`, and
`otel_metrics_summary`. Use the configured database/table names if you changed
them.

3. Existing parts stay on the built-in `default` disk (exposed as the
   policy's `default` volume). TTL moves them once all rows in a part are older
   than `hotDays`. To move data earlier, move partitions explicitly (this can
   be I/O heavy):

```sql
ALTER TABLE otel.otel_logs MOVE PARTITION '20261030' TO VOLUME 'cold';
```

4. Add the case-sensitive body index. New installs get it from the default
   schema; existing installs need both statements:

```sql
ALTER TABLE otel.otel_logs
  ADD INDEX IF NOT EXISTS idx_body Body TYPE text(tokenizer = 'splitByNonAlpha');
ALTER TABLE otel.otel_logs MATERIALIZE INDEX idx_body;
```

`idx_body` serves case-sensitive `match(Body, ...)` and token search. The
existing `idx_lower_body lower(Body)` index remains for case-insensitive
predicates. Neither index changes query results; they only let ClickHouse skip
granules.

### Query behavior

Cold parts are still part of the live tables. The gateway does not need a
separate code path: ClickHouse reads the S3 object, caches it locally according
to `cacheSize`, and returns the same rows. Expect higher first-read latency and
S3 request cost for cold data. If the S3 bucket or credentials become
unavailable, queries that touch cold parts fail; hot-only queries can still
succeed.

## Automatic backup

### Values

```yaml
clickhouse:
  backup:
    enabled: true
    schedule: "0 2 * * *"
    activeDeadlineSeconds: 3600
    startingDeadlineSeconds: 600
    successfulJobsHistoryLimit: 3
    failedJobsHistoryLimit: 1
    endpoint: http://rustfs.observability.svc:9000
    bucket: skopos-backup
    prefix: skopos/backups
    existingSecret: skopos-backup-credentials
    accessKeyId: ""
    secretAccessKey: ""
```

The backup image defaults to the pinned ClickHouse image and runs
`clickhouse-client`; override `clickhouse.backup.image.*` only if you need a
different pinned build.

The CronJob:

- runs `BACKUP DATABASE <database> TO S3('<endpoint>/<bucket>/<prefix>/<database>/<UTC timestamp>', ...)`
  with `clickhouse-client`;
- sets `concurrencyPolicy: Forbid`, `startingDeadlineSeconds`, and
  `activeDeadlineSeconds` so a slow backup cannot overlap the next run
  indefinitely;
- keeps `successfulJobsHistoryLimit` and `failedJobsHistoryLimit` Jobs for
  inspection;
- gets S3 credentials from `backup.existingSecret` (keys `access-key-id` and
  `secret-access-key`) or from a generated Secret when inline values are used;
- uses the existing ClickHouse admin Secret for ClickHouse authentication;
- is disabled by default, so normal CI and a default install need no S3
  credentials.

The bucket must already exist. The chart refuses to render if the backup
endpoint/bucket/prefix overlaps the live cold tier, because a restore into that
location could collide with live parts. Use a different bucket or a
non-overlapping prefix such as `skopos/backups` versus `skopos/cold`.

Each run is a full database backup. The chart does not prune old backups; use
the bucket's lifecycle rules or an external retention policy. For large
databases, schedule backups outside peak hours and monitor the Job deadline.

### Restore

List the timestamped prefixes:

```sh
aws --endpoint-url http://rustfs:9000 \
  s3 ls s3://skopos-backup/skopos/backups/otel/ --recursive | head
```

Restore into a new database name first. This is the command shape used by the
integration test:

```sql
RESTORE DATABASE otel AS otel_restore
FROM S3('http://rustfs:9000/skopos-backup/skopos/backups/otel/20261101T060737Z',
        '<access-key-id>', '<secret-access-key>');
```

Then verify:

```sql
SELECT count() FROM otel_restore.otel_logs;
SELECT min(Timestamp), max(Timestamp) FROM otel_restore.otel_logs;
```

The restored tables keep the backup's storage policy. Restore on a ClickHouse
with the same `tiered` configuration mounted, or override the policy if you are
restoring to a local-only server:

```sql
RESTORE DATABASE otel AS otel_restore
FROM S3(...)
SETTINGS storage_policy = 'default';
```

Do not restore over the live `otel` database while the gateway is serving it.
After verification, either point the chart at the restored database
(`clickhouse.database=otel_restore`; the init job is idempotent for existing
tables) or copy/rename tables during a maintenance window. If you restore into
a non-empty table, ClickHouse requires `allow_non_empty_tables = 1`, which can
duplicate rows; prefer a clean database.

## Test harness

`test/integration/s3` runs a real ClickHouse + RustFS test:

```sh
PATH=/tmp/opencode/linux-amd64:$PATH ./test/integration/s3/run.sh
```

It renders the chart's tiered schema, applies all tables, inserts hot and old
log rows, forces TTL, asserts the old part is on the cold disk and still
queryable, checks `idx_body` in `EXPLAIN indexes = 1`, runs `BACKUP DATABASE`
into a separate bucket/prefix, lists the objects, and restores to
`otel_restore`. See the harness README for details.

## Limitations

- Single-node ClickHouse. The chart does not replicate, so a lost PVC still
  loses the hot copy even when the cold tier is enabled.
- TTL movement and merges are asynchronous. A part can remain hot past
  `hotDays` until ClickHouse merges/moves it.
- `ttl_only_drop_parts = 1` means the delete rule drops whole parts; mixed-age
  parts may survive longer than `totalDays` until a merge separates them.
- Cold reads depend on S3 availability and add request cost/latency.
- Backups are full logical copies, not incremental, and are not verified by
  the chart after upload. Monitor the CronJob and periodically practice a
  restore.
- The chart does not create S3 buckets, IAM policies, or lifecycle rules.
- No external compactor: ClickHouse merges and compacts hot and cold parts
  itself. S3 object layout is ClickHouse-managed; do not edit it manually.
