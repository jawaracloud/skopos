# Support matrix (v0.1 alpha)

Alpha means no compatibility guarantee. The table below is the public list of
what is implemented. Anything not listed must return an explicit error, not an
empty or incorrect result.

## Signals

| Signal | Ingest | Storage | Query API | Status |
| --- | --- | --- | --- | --- |
| Metrics | OBI -> Collector OTLP -> ClickHouse | `otel_metrics_gauge`, `_sum`, `_histogram`, `_exponential_histogram`, `_summary` | Not implemented (M3 PromQL) | Alpha |
| Traces | OBI -> Collector OTLP -> ClickHouse | `otel_traces`, `otel_traces_trace_id_ts` | Not implemented (M4 TraceQL) | Alpha |
| Logs | Collector OTLP receiver (enabled); filelog receiver not bundled | `otel_logs` | Not implemented (M5 LogQL) | Alpha |

OBI does not emit logs. The default chart accepts logs through the Collector's
OTLP receiver. A manual k3d smoke test sent traces, metrics, and logs through
the Collector and read them back from ClickHouse; this is not yet an automated
chart test. A filelog receiver is not bundled; enabling one requires verifying
the receiver component and its maturity first.

## Gateway endpoints

| Endpoint | Status |
| --- | --- |
| `GET /healthz` | Implemented |
| `GET /readyz` | Implemented |
| `GET /version` | Implemented |
| Prometheus `/api/v1/*` | Not implemented (M3) |
| Tempo `/api/*` | Not implemented (M4) |
| Loki `/loki/api/v1/*` | Not implemented (M5) |

## PromQL, TraceQL, LogQL

The per-language supported-function lists will be published in
`docs/promql-support.md`, `docs/traceql-support.md`, and
`docs/logql-support.md` when M3–M5 land. Until then, the only honest answer is
that the language heads are not implemented.

The gateway parses the configurable database and table names at startup. The
M3–M5 heads will build the plan schema from them; the current health-only server
does not query ClickHouse except for `/readyz`.

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
