# PromQL support (alpha)

Status: **alpha**. No compatibility guarantee. Unlisted functions, operators,
and extensions return an explicit error. Unsupported metric types are called out
below because they can return an empty vector rather than an error.

PromQL is parsed with the official Prometheus parser
(`github.com/prometheus/prometheus/promql/parser`, v0.315.1, Apache-2.0). The
evaluator runs in the gateway over samples fetched from ClickHouse.

## Endpoints

| Endpoint | Status |
| --- | --- |
| `GET/POST /api/v1/query` | Implemented |
| `GET/POST /api/v1/query_range` | Implemented |
| `GET /api/v1/series` | Implemented |
| `GET /api/v1/labels` | Implemented |
| `GET /api/v1/label/{name}/values` | Implemented |
| `GET /api/v1/metadata` | Implemented |
| `GET /api/v1/query_exemplars` | Empty response (exemplars are not stored) |
| `GET /api/v1/status/buildinfo` | Implemented |

## Selectors and labels

- Instant and range vector selectors.
- Label matchers `=`, `!=`, `=~`, `!~`.
- `offset`.
- Lookback: 5 minutes for instant selectors.
- Label regex matchers are fully anchored, matching Prometheus.
- Metric names and label names are normalized: characters outside
  `[A-Za-z0-9_]` become `_`. For example `http.server.request.duration` becomes
  `http_server_request_duration` and `http.method` becomes `http_method`.
- `service_name` comes from the exporter's `ServiceName` column. Other labels
  come from the metric `Attributes` map.
- Metric unit suffixes are **not** appended yet. Query the normalized OTel
  name, not the Prometheus-convention name with `_seconds` or `_bytes`.
- `@` modifiers and subqueries are not supported.

## Functions

| Group | Supported |
| --- | --- |
| Counters | `rate`, `irate`, `increase` |
| Range aggregations | `avg_over_time`, `min_over_time`, `max_over_time`, `sum_over_time`, `count_over_time`, `last_over_time`, `stddev_over_time`, `stdvar_over_time`, `present_over_time`, `quantile_over_time` |
| Histograms | `histogram_quantile` over `_bucket` series from the histogram table; `_count` and `_sum` series are also exposed |
| Math | `abs`, `ceil`, `floor`, `sqrt`, `exp`, `ln`, `log2`, `log10`, `round`, `clamp`, `clamp_min`, `clamp_max` |
| Ordering | `sort`, `sort_desc` |
| Helpers | `time`, `timestamp`, `scalar`, `vector` |

Counter resets are handled explicitly: a negative delta is treated as a reset.
`rate`/`increase` extrapolate to the range boundaries using Prometheus's
average-sample-interval rule. `timestamp()` returns the original sample time,
not the evaluation time. Metric names are dropped for functions and arithmetic
operations; comparisons without `bool` retain them, as in Prometheus. There are
no staleness markers in ClickHouse, so staleness is approximated by the
5-minute lookback.

## Aggregations and binary operators

- Aggregations: `sum`, `avg`, `min`, `max`, `count`, `stddev`, `stdvar`,
  `group` with `by` and `without`.
- Arithmetic and comparison operators between scalar/vector and
  vector/vector.
- Vector/vector matching is one-to-one. `group_left`, `group_right`, and
  many-to-many matching are rejected.
- Set operators (`and`, `or`, `unless`) are rejected.

## Known limitations

- Sample resolution is ClickHouse `DateTime` seconds for metric tables.
- Native histograms and exemplars are not supported.
- Only the gauge, sum, and classic histogram tables are read. OTel summary and
  exponential-histogram metrics are stored but not yet queryable through this
  head; a selector for one of those metric types returns an empty result rather
  than an error.
- Metric unit suffixes are not appended (see the label section above).
- `label_replace`, `label_join`, `absent`, `absent_over_time`, `topk`,
  `bottomk`, `count_values`, and other unlisted functions are rejected.
- A query scans gauge, sum, and histogram tables for each selector. The gateway
  enforces a row scan limit (default 200,000) and returns an error instead of
  truncating.
- The maximum query range is 7 days and the maximum number of range points is
  11,000.
- Query results are evaluated in the gateway, not pushed down to ClickHouse.
  This is an alpha trade-off favoring correctness and simple SQL.

## Tests

- Golden SQL tests for the emitter.
- Unit tests for selectors, counter resets, aggregation, `offset`, binary
  operators, `histogram_quantile`, and unsupported functions.
- Integration tests against ClickHouse 26.8 for instant queries, range queries,
  histogram quantile, metadata, and label normalization.
