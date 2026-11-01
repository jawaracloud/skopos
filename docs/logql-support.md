# LogQL support (alpha)

Status: **alpha**. No compatibility guarantee. Anything not listed here returns
an explicit error.

## Endpoints

| Endpoint | Status |
| --- | --- |
| `GET/POST /loki/api/v1/query` | Implemented |
| `GET/POST /loki/api/v1/query_range` | Implemented |
| `GET /loki/api/v1/labels` | Implemented |
| `GET /loki/api/v1/label/{name}/values` | Implemented |
| `GET /loki/api/v1/series` | Implemented |
| `GET /loki/api/v1/status/buildinfo` | Implemented |

## Supported LogQL subset

Stream selector:

```logql
{service_name="checkout", severity_text=~"ERR.*"}
```

Supported matchers: `=`, `!=`, `=~`, `!~`. Label regex matchers are fully
anchored, as in Loki.

Line filters:

```logql
{service_name="checkout"} |= "timeout"
{service_name="checkout"} != "debug"
{service_name="checkout"} |~ "timeout|deadline"
{service_name="checkout"} !~ "healthcheck"
```

`query` and `query_range` support `start`, `end`, `limit`, and `direction`
(`forward` or `backward`, default backward). For log queries `step` is ignored.
The instant `/query` endpoint accepts `time` and evaluates a 5-minute window
ending at that time; without `time` it uses the last hour.
`start`/`end` accept RFC3339, Unix seconds, milliseconds, microseconds, or
nanoseconds; Grafana's nanosecond values are supported.

## Labels

The following stream labels are exposed when present:

- `service_name`
- `severity_text`
- `severity_number`
- `trace_id`
- `span_id`
- `event_name`
- Resource and log attributes, normalized with the same
  `[A-Za-z0-9_]` mapping as PromQL. For example `k8s.pod.name` becomes
  `k8s_pod_name`.

`/loki/api/v1/series` returns one label set per distinct stream. It can return
multiple sets when severity, trace ID, or other labels differ. Labels, label
values, and series accept either `match[]` selectors or a `query` stream
selector.

## Known limitations

- Parser stages (`| json`, `| logfmt`, `| pattern`, `| regexp`) are rejected.
- Label filters after a parser stage are rejected.
- LogQL metric queries (`rate`, `count_over_time`, `sum by (...)`, etc.) are
  rejected. This head returns log streams only.
- `unwrap`, `label_format`, `drop`, `keep`, and other pipeline stages are
  rejected.
- No `step`/`interval` matrix results.
- The maximum query range is 7 days and the gateway enforces a row scan limit
  (default 200,000).

## Tests

- Unit tests for stream selectors, line filters, label normalization, and
  unsupported LogQL.
- API tests for streams, labels, label values, series, limits, and errors.
- Integration tests against ClickHouse 26.8 with real `otel_logs` rows.
