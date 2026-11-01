# Support matrix (v0.1 alpha)

Alpha means no compatibility guarantee. The table below is the public list of
what is implemented. Anything not listed must return an explicit error, not an
empty or incorrect result.

## Signals

| Signal | Ingest | Storage | Query API | Status |
| --- | --- | --- | --- | --- |
| Metrics | OBI or OTLP clients -> Collector -> ClickHouse | `otel_metrics_gauge`, `_sum`, `_histogram`, `_exponential_histogram`, `_summary` | Prometheus-compatible (M3) | Alpha |
| Traces | OBI or OTLP clients -> Collector -> ClickHouse | `otel_traces`, `otel_traces_trace_id_ts` | Tempo-compatible (M4) | Alpha |
| Logs | OTLP clients -> Collector -> ClickHouse; filelog receiver not bundled | `otel_logs` | Loki-compatible (M5) | Alpha |

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

The gateway exposes all three APIs on one port. The Grafana datasource example
points all three datasource types at that service.

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
  filters, and `count_over_time`/`rate` with `sum`/`sum by`. The 11-case corpus
  is compared with Loki 3.5.7 for the supported subset.

## ClickHouse version

Tested with ClickHouse **26.8.36.3**. The schema uses the exporter's full-text
indexes for logs, which require ClickHouse 26.2 or newer. Older versions are not
supported in the v0.1 alpha. The minimum supported version will be re-evaluated
with the beta test matrix.

## Known limitations

- The chart's init job uses `CREATE TABLE IF NOT EXISTS`; it does not migrate
  existing tables. Changing table columns or TTL requires a manual migration.
- The gateway has no auth. See [SECURITY.md](../SECURITY.md).
- OBI's kernel and capability requirements are external constraints, not
  something the chart can validate.
- Query evaluation is in the gateway, not pushed down to ClickHouse. The row
  scan limit and 7-day range bound protect the single-node alpha deployment.
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
  integration, fuzz, Helm, PromQL oracle, Tempo/Loki references, and the
  cerberus three-way gate.
