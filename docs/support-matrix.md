# Support matrix (v0.1 alpha)

Alpha means no compatibility guarantee. The table below is the public list of
what is implemented. Anything not listed must return an explicit error, not an
empty or incorrect result.

## Signals

| Signal | Ingest | Storage | Query API | Status |
| --- | --- | --- | --- | --- |
| Metrics | OBI or OTLP clients -> Collector -> ClickHouse | `otel_metrics_gauge`, `_sum`, `_histogram`, `_exponential_histogram`, `_summary`; optional S3 cold tier | Prometheus-compatible (M3) | Alpha |
| Traces | OBI or OTLP clients -> Collector -> ClickHouse | `otel_traces`, `otel_traces_trace_id_ts`; optional S3 cold tier | Tempo-compatible (M4) | Alpha |
| Logs | OTLP clients -> Collector -> ClickHouse; filelog receiver not bundled | `otel_logs`; optional S3 cold tier | Loki-compatible (M5) | Alpha |

OBI does not emit logs. The default chart accepts logs through the Collector's
OTLP receiver. A manual k3d smoke test sent traces, metrics, and logs through
the Collector and read them back from ClickHouse; this is not yet an automated
chart test. A filelog receiver is not bundled; enabling one requires verifying
the receiver component and its maturity first.

## Query endpoints

| Endpoint group | Status | Support list |
| --- | --- | --- |
| Prometheus `/api/v1/*` | Implemented alpha | [promql-support.md](promql-support.md) |
| Tempo `/api/*` and `/api/v2/*` | Implemented alpha | [traceql-support.md](traceql-support.md) |
| Loki `/loki/api/v1/*` | Implemented alpha | [logql-support.md](logql-support.md) |
| Health `/healthz`, `/readyz`, `/version` | Implemented | — |
| Context `/api/v1/context/*` | Implemented alpha (optional catalog; metrics, services, logs, traces, coverage/alerts, discovery, observed metadata, suggestions) | [ai-context.md](ai-context.md) |
| Context metrics `/metrics` | Implemented alpha (Prometheus text gauges; auth applies) | [ai-context.md](ai-context.md) |
| MCP server | Implemented alpha (`cmd/skopos-mcp`, stdio and HTTP sessions/SSE) | [ai-context.md](ai-context.md#mcp-server) |

The gateway exposes all three APIs on one public port. The Grafana datasource
example points all three datasource types at that service.

## Deployment modes

| Mode | Status | Notes |
| --- | --- | --- |
| Single process (`gateway.mode=all`) | Default | One Deployment serves all three APIs and connects to ClickHouse. |
| systemd (`deploy/systemd/`) | Alpha | Same binary and schema under systemd; supports single-process or frontend/query split units. No Kubernetes policy/HPA. |
| Distributed read path (`distributed.enabled=true`) | Alpha | Public frontend proxies to internal stateless query workers. The frontend has no ClickHouse credentials. |
| Tiered S3 retention (`clickhouse.storage.enabled=true`) | Alpha | Hot local parts move to an S3-compatible cold volume; cold parts stay queryable. |
| Scheduled backups (`clickhouse.backup.enabled=true`) | Alpha | `BACKUP DATABASE ... TO S3(...)` CronJob to a separate prefix. Backups are restore-only, not queryable through the gateway. |

ClickHouse remains the backend and owns merges/compaction. There is no Skopos
ingester or Thanos-style compactor. See [storage.md](storage.md) and
[ADR 0002](adr/0002-distributed-read-path.md).

## Language support

- **PromQL:** selectors, matchers, `rate`/`irate`/`increase` with boundary
  extrapolation and counter resets, range aggregations, `histogram_quantile`,
  math, set operators (`and`/`or`/`unless`), `group_left`/`group_right`,
  `topk`/`bottomk`/`count_values`, `absent`/`absent_over_time`,
  `label_replace`/`label_join`, scalar/vector and vector/vector binary
  operators, `offset`, and summary metrics. Exponential histograms return an
  explicit `bad_data` error. The PromQL differential oracle exercises 212
  instant/range cases against the official Prometheus engine; it is not the
  full Prometheus compliance tester.
- **TraceQL:** one spanset with `&&`/`||`, resource/span/event/link/
  instrumentation scopes, trace/span intrinsics, tag search, tags and tag
  values, trace by ID in OTLP JSON or protobuf (v2 uses the Tempo
  `TraceByIDResponse` envelope), and search `spanSets`. The 55-case corpus is
  compared with Tempo 2.7.2 for the supported subset.
- **LogQL:** stream selectors, line filters, `| json`, `| logfmt`, label
  filters, and `count_over_time`/`rate` with `sum`/`sum by`. Selector predicates
  and body line filters are pushed into ClickHouse: `|=`/`!=` use
  `position(Body, ?)`; `|~`/`!~` use `match(Body, ?)` and can use the
  case-sensitive `idx_body` text index. Go still evaluates every returned row.
  The 11-case corpus is compared with Loki 3.5.7 for the supported subset.

## ClickHouse version

Tested with ClickHouse **26.8.36.3**. The schema uses the exporter's full-text
indexes for logs, which require ClickHouse 26.2 or newer. Older versions are not
supported in the v0.1 alpha. The minimum supported version will be re-evaluated
with the beta test matrix.

## Known limitations

- The chart's init job uses `CREATE TABLE IF NOT EXISTS`; it does not migrate
  existing tables. Changing table columns or TTL requires a manual migration.
- Basic auth is optional and uses one shared credential; there is no per-user
  authorization or tenant isolation. See [SECURITY.md](../SECURITY.md).
- OBI's kernel and capability requirements are external constraints, not
  something the chart can validate.
- Query evaluation is in the gateway, not pushed down to ClickHouse. The row
  scan limit and 7-day range bound protect the single-node alpha deployment.
  LogQL only pushes filters; metric aggregation still happens in the gateway.
- S3 cold reads are slower than hot reads and depend on the local ClickHouse
  S3 cache. A backup is not a queryable retention tier; restore it first.
- Tiered storage and backups need external S3-compatible buckets and
  credentials. The chart does not create buckets.
- The distributed frontend scales the read path only. ClickHouse is still a
  single node in the default chart; there is no ClickHouse replication/HA.
- The language support lists are intentionally narrow. The current harnesses
  cover a subset: PromQL differential oracle (212 cases), TraceQL corpus
  (55 cases), LogQL corpus (11 cases), and the cerberus three-way gate
  (124 checks, 0 Skopos divergences, 39 cerberus-only gaps baseline-pinned).
  The literal three-way zero is blocked by cerberus v1.23.1; the harness fails
  on any new or fixed cerberus gap.
- The full Prometheus compliance tester and the complete Loki benchmark corpus
  remain beta gates; see [beta-gates.md](beta-gates.md).
- The OBI e2e and release signing/publishing are manual because they need
  privileged eBPF or project credentials; normal CI covers build, unit,
  integration, fuzz, Helm, PromQL oracle, Tempo/Loki references, the cerberus
  three-way gate, and the S3/RustFS test. OpenSSF Scorecard runs weekly.
