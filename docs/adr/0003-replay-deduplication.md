# ADR 0003: Idempotent checkpoint replay in ClickHouse

- Status: Accepted for the v0.1 alpha
- Date: 2026-11-07
- Supersedes: none
- Related: [checkpoint.md](../checkpoint.md), [storage.md](../storage.md)

## Context

The durability checkpoint replays original OTLP payloads through the Collector.
Transport is deliberately at-least-once: a crash between two POSTs, or between
the last POST and the S3 delete, sends some records twice. ClickHouse must make
the replay idempotent without changing the one canonical write path.

The original signal tables were `MergeTree` with non-unique `ORDER BY` keys.
Replacing the engine alone would collapse legitimate rows. OTLP also has no
universal record id:

- A span has `(TraceId, SpanId)`.
- A log record has no unique id.
- A metric data point has no unique id.
- Attribute maps can be serialized in a different order by a different process.

ClickHouse insert-level deduplication is not usable: the Collector re-batches
records before every insert, so a replayed block is not byte-identical to the
original insert block, and OTLP metadata is not carried into ClickHouse rows.

## Decisions

### D1: `ReplacingMergeTree` with a deterministic final sort-key component

All signal tables use `ReplacingMergeTree`. The existing sort prefix stays for
partition and primary-key pruning, and the final key component identifies the
record:

| Table | Final key component |
| --- | --- |
| `otel_traces` | `TraceId, SpanId` |
| `otel_logs` | `_skopos_row_hash`, a materialized hash of the whole row |
| metric tables | `_skopos_row_hash`, a materialized hash of the whole row |

The hash uses 128-bit `sipHash128` and `mapSort` on every attribute map and on
each map inside an exemplar/event/link array. Attribute insertion order
therefore cannot split a duplicate. `_skopos_row_hash` is materialized, so the
Collector's fixed insert column list does not need to know about it and
`SELECT *` does not expose it.

Attribute order is deterministic because the checkpoint stores the
Collector's own serialized OTLP payload and replay posts those exact bytes. The
`mapSort` canonicalization is a second line of defense for a processor or SDK
that changes map order.

### D2: `FINAL` on gateway reads, with a legacy fallback

`ReplacingMergeTree` deduplicates during background merges. A query issued
immediately after replay could still see both copies. The SQL emitter appends
`FINAL` to every signal-table read, so the deduplicated key applies on every
query.

ClickHouse rejects `FINAL` on plain `MergeTree` with `ILLEGAL_FINAL` (code 181).
The HTTP client retries the statement without `FINAL` on that error. This keeps
the gateway readable if it starts before the upgrade migration finishes; only
read-time deduplication is lost for that table until the migration completes.

### D3: Online migration in the ClickHouse init Job

`CREATE TABLE IF NOT EXISTS` cannot change an existing engine or sort key, and
ClickHouse has no `ALTER TABLE ... MODIFY ENGINE`. The init Job runs
`schema.sql` and then `migrate.sh`:

1. Rename the old table to `<table>__skopos_pre_dedup`.
2. Re-run `schema.sql` to create the new table with the current engine and key.
3. Backfill from the renamed table while the new table is already live.
4. Drop the renamed table.

The Collector retries inserts during the short rename window. The script is
idempotent: a later run copies from a leftover `__skopos_pre_dedup` table and
drops it. Repeated copies collapse on the new key. The trace-lookup table is a
materialized-view target, so the Job drops the view first and rebuilds the
derived index from the migrated traces table.

## Consequences

- Replaying the same segment never creates duplicate rows visible to the
  gateway, and background merges eventually reclaim the duplicate storage.
- Two byte-identical log or metric records are the same record and collapse to
  one. Without an SDK-supplied id there is no way to distinguish them. This is
  documented in [checkpoint.md](../checkpoint.md).
- `FINAL` adds read-time merge work. The signal tables are append-only and the
  sort keys retain their time/service prefix, so pruning still bounds the work.
- `trace_id_ts` remains a derived index. Its per-insert aggregate rows can
  differ if replay re-batches records; the gateway does not query it.
- An upgrade with large tables can outlive the default 600 s init-Job deadline.
  The Job is resumable; operators can raise
  `clickhouse.initJob.activeDeadlineSeconds`.

## Alternatives rejected

- **Insert deduplication tokens.** The Collector, not the checkpointer, writes
  to ClickHouse, and replay re-batches records; there is no stable insert-block
  token to carry.
- **A new ingest id.** OTLP has no shared record id and the ClickHouse exporter
  has a fixed schema. A synthetic id generated on insert would differ between
  the original and the replay.
- **Query-time `LIMIT 1 BY` / `GROUP BY` only.** It requires the same identity
  work, still needs a schema column for logs/metrics, and makes every read pay
  the dedup cost without giving the storage engine a key to merge.
- **`CollapsingMergeTree`.** It needs a sign column written by the producer;
  replay would have to emit the negative half of every original row.
- **Full table copy under a write pause.** It requires a maintenance window and
  scales with the whole retention window. The rename-then-backfill path keeps
  the new table live.
