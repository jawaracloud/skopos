# ADR 0002: Distributed read path, S3 retention, and full-text logs

- Status: Accepted for the v0.2 line
- Date: 2026-11-01
- Supersedes: none
- Related: ADR 0001 (D1, D7, D11, D12)

## Context

ADR 0001 scoped v0.1 to a single-process gateway in front of a single-node
ClickHouse. That is still the default. Three operational limits appear as soon
as the alpha is used for more than a demo:

1. The public API and the ClickHouse query work run in the same process. A
   query burst, a rolling upgrade, or a bad client can take down the public
   endpoint even when ClickHouse is healthy.
2. Local disk is the only storage tier. Keeping every signal forever is not
   affordable, and deleting at TTL loses the data permanently.
3. LogQL fetches every row in the time range and filters in Go. The ClickHouse
   text index on the log body exists but was not used by the query path.

The user-facing requirement is a deployment shaped like a modern metrics
backend: separate public query entry point, stateless query workers, object
storage for cold data, and real full-text search. It is explicitly **not** a
request to copy Mimir or Thanos internals.

## Decisions

### D1: Split the read path into frontend and query workers

`SKOPOS_MODE` selects the process role:

| Mode | Role | ClickHouse credentials |
| --- | --- | --- |
| `all` | v0.1 single process | yes |
| `query` | stateless PromQL/LogQL/TraceQL worker | yes |
| `worker` | explicit alias for `query` | yes |
| `frontend` | public reverse proxy and readiness gate | no |

The frontend round-robins requests over `SKOPOS_QUERY_URLS`, preserves method,
path, query, headers, and body, and returns 503 when no query backend is ready.
It serves `/healthz`, `/readyz`, and `/version` locally. The public Service
keeps the name `skopos-gateway`; the query Service is internal.

### D2: ClickHouse remains the backend and owns compaction

There is no Skopos ingester, block store, store-gateway, or compactor. The
OpenTelemetry Collector writes OTLP to ClickHouse; ClickHouse merges parts,
applies TTL, and (when enabled) moves cold parts to S3. A Skopos compactor would
duplicate ClickHouse's MergeTree merge process and operate on a different data
unit than Thanos blocks.

### D3: S3 retention uses ClickHouse tiered storage, not just backups

When enabled, tables use a `tiered` storage policy:

- hot volume on local disk;
- cold volume on an S3-compatible endpoint (RustFS for the local test);
- TTL moves parts to cold after `hotDays`;
- TTL deletes after `totalDays`.

This keeps cold data queryable through the same gateway. ClickHouse still owns
merges and part movement.

A separate scheduled `BACKUP DATABASE ... TO S3(...)` job writes to a backup
prefix that is distinct from the live cold tier. A backup is for disaster
recovery and restore; the gateway does **not** query backups directly. TTL
deletion is not a backup, and a backup is not a queryable retention tier.

### D4: Full-text search is pushed down, Go remains the authority

LogQL stream selectors on known columns and all body line filters are rendered
as parameterized ClickHouse predicates:

- `|=` / `!=` use `position(Body, ?)`, which is exact substring matching but is
  not accelerated by the text index;
- `|~` / `!~` use `match(Body, ?)`, which can use the case-sensitive
  `idx_body` text index;
- `service_name`, `severity_text`, `trace_id`, `span_id`, and `event_name`
  selectors use the corresponding columns.

Go's `Query.Evaluate` still runs on every returned row. The SQL predicate is a
necessary pre-filter, never the final answer. Parser stages, extracted-label
filters, and arbitrary attribute labels stay in Go because their semantics do
not map safely to a single SQL expression.

### D5: Do not use token search as a silent substitute for substring search

`hasToken` is faster but changes semantics. A LogQL `|=` filter is a substring
match, not a token match. The implementation therefore does not rewrite
substring filters to `hasToken`. If a future fast path is added, it must be an
explicit, documented mode.

## Consequences

- A public frontend can be scaled independently from query workers. Query
  workers can be scaled independently from the frontend.
- The frontend has no ClickHouse credentials, so a frontend compromise does
  not directly expose the database.
- ClickHouse is still a single node in the default chart. This ADR splits the
  read path, not the storage layer; ClickHouse replication/HA remains a
  separate decision.
- S3 cold reads are slower than hot reads. The ClickHouse S3 disk cache should
  be sized for the expected query working set.
- Full-text regex pushdown is index-accelerated only when ClickHouse can extract
  complete tokens from the pattern. Unanchored substring and some regex queries
  still scan.
- The public Service still has no authentication. Use an authenticating ingress
  or proxy; the frontend does not add auth.
- The backup job needs S3 credentials and a restore runbook. Normal CI does not
  require those credentials.

## Rejected alternatives

- **Mimir/Thanos-style ingester, store-gateway, and compactor.** The write path
  is the Collector and the storage format is ClickHouse MergeTree parts, not
  immutable TSDB blocks. Reimplementing those roles would add components
  without removing ClickHouse.
- **S3 as the source of truth for live queries.** This would require a new
  block format, index, and query engine. It is a different product, not a
  deployment mode.
- **Backups as retention.** A backup is not queryable through the gateway and
  restoring it is an operational event. Retention needs the cold tier.
- **Using `hasToken` for every `|=`.** It silently changes substring semantics.
- **Pushing parser stages into SQL.** JSON/logfmt extraction and label-filter
  precedence are part of the LogQL contract and are safer in Go.

## Follow-ups

- Add query-result caching and time-range splitting to the frontend when real
  query load justifies it. Do not split PromQL blindly; counter windows,
  lookback, and staleness need boundary handling.
- Evaluate ClickHouse replication/Keeper or ClickHouse Cloud for HA.
- Add a restore drill to CI or a scheduled environment; a backup that has never
  been restored is not a verified backup.
- Re-run the full Prometheus compliance tester and Loki benchmark corpus after
  the distributed split.
