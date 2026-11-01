# PromQL support (alpha)

Status: **alpha**. No compatibility guarantee. Unlisted functions, operators,
and extensions return an explicit error. Like Prometheus, `last_over_time`
keeps the metric name and all other functions drop it unless documented
otherwise. Unsupported metric types are called out below; they are no longer
allowed to return a silent empty vector.

PromQL is parsed with the official Prometheus parser
(`github.com/prometheus/prometheus/promql/parser`, v0.315.1, Apache-2.0). The
evaluator runs in the gateway over samples fetched from ClickHouse.

The claimed surface below is checked by a differential harness that runs the
same deterministic series through this engine and the official Prometheus
`promql` engine. See [Tests](#tests).

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
- Lookback: 5 minutes for instant selectors. A sample exactly at
  `evaluation time - 5m` is stale; the window is left-open.
- Range selectors are left-open as in Prometheus: `metric[5m]` at `10:00`
  covers samples with timestamps in `(09:55, 10:00]`.
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
| Range aggregations | `avg_over_time`, `min_over_time`, `max_over_time`, `sum_over_time`, `count_over_time`, `last_over_time`, `stddev_over_time`, `stdvar_over_time`, `present_over_time`, `quantile_over_time`, `absent_over_time` |
| Histograms | `histogram_quantile` over `_bucket` series from the histogram table; `_count` and `_sum` series are also exposed |
| Math | `abs`, `ceil`, `floor`, `sqrt`, `exp`, `ln`, `log2`, `log10`, `round`, `clamp`, `clamp_min`, `clamp_max` |
| Ordering | `sort`, `sort_desc` (NaN sorts last in both directions) |
| Labels | `label_replace`, `label_join` |
| Presence | `absent`, `absent_over_time` |
| Helpers | `time`, `timestamp`, `scalar`, `vector` |

Counter resets are handled explicitly: a negative delta is treated as a reset.
`rate`/`increase` extrapolate to the range boundaries using Prometheus's
average-sample-interval rule, including the counter zero-point guard that
prevents extrapolating a counter below zero. `timestamp()` returns the original
sample time, not the evaluation time. Metric names are dropped for functions and
arithmetic operations; comparisons without `bool` retain them, as in Prometheus.
`last_over_time` keeps the metric name because it acts like an offset in
Prometheus; all other `*_over_time` functions and `rate`/`irate`/`increase`
drop it. There are no staleness markers in ClickHouse, so staleness is
approximated by the 5-minute lookback.

`histogram_quantile` follows the reference classic-histogram algorithm:
`q < 0` returns `-Inf`, `q > 1` returns `+Inf`, `q = NaN` returns `NaN`, a
missing `+Inf` bucket or fewer than two buckets returns `NaN`, duplicate upper
bounds are coalesced, and non-monotonic bucket counts are repaired before
interpolation.

`label_replace` and `label_join` preserve the metric name unless the destination
label is `__name__`. Replacing labels in a way that produces two series with the
same label set returns an explicit error, as Prometheus does.

`absent` and `absent_over_time` return `1` with the equality matchers of a
direct selector when their argument is empty. Regex and negated matchers are
omitted, and duplicate matchers for the same label are dropped, matching
Prometheus's historical behaviour.

## Aggregations and binary operators

- Aggregations: `sum`, `avg`, `min`, `max`, `count`, `stddev`, `stdvar`,
  `group`, `topk`, `bottomk`, `count_values` with `by` and `without`.
- `topk`/`bottomk` return the original series labels and metric name. `k <= 0`
  returns an empty vector, and NaNs are selected last.
- Arithmetic and comparison operators between scalar/vector and
  vector/vector.
- Vector/vector matching supports one-to-one, `group_left`, and `group_right`,
  with `on`/`ignoring` and the `group_*` include list. One-to-one matches
  reduce the result labels to the matching labels (or remove the ignored
  labels), exactly as Prometheus does. Many-to-many matching without a group
  modifier returns an explicit error.
- Set operators (`and`, `or`, `unless`) are supported with `on`/`ignoring`.
  They retain the labels and metric name of the selected series; `or` emits
  right-hand series whose matching signature is absent on the left.

## Metric types

| OTel metric type | Status |
| --- | --- |
| Gauge | Read from the gauge table |
| Sum | Read from the sum table (counter semantics are not inferred; use `rate`/`increase` on monotonic sums) |
| Classic histogram | Expanded to `_bucket`, `_count`, `_sum`; `histogram_quantile` works |
| Summary | Expanded to `_count`, `_sum`, and quantile series carrying a `quantile` label |
| Exponential histogram | **Not supported.** A selector that matches only exponential-histogram data returns an explicit `bad_data` error, not an empty vector |
| Native histogram samples | Not supported (no native histogram storage in the exporter schema) |

The exponential-histogram probe is bounded: it runs only when a selector with a
metric-name matcher produced no supported series, and reads at most 1000 rows
(or the configured scan limit, if lower). If a selector matches both supported
series and exponential histograms, the supported series are returned; the
unsupported type is only reported when there is no supported result. Selectors
without a metric-name matcher (for example the `/labels` endpoint's empty
selector, or `{service_name="x"}`) do not probe or fail on exponential
histograms.

## Known limitations

- Sample resolution is ClickHouse `DateTime` seconds for metric tables.
- Native histograms and exemplars are not supported.
- Exponential histograms are stored but not queryable (see the table above).
- Metric unit suffixes are not appended (see the label section above).
- Examples of unlisted functions that return an explicit error:
  `deriv`, `predict_linear`, `holt_winters`, `resets`, `changes`, `delta`,
  `idelta`, `first_over_time`, `histogram_count`, `histogram_sum`, and the
  experimental `limitk`/`limit_ratio` aggregations.
- A query scans the gauge, sum, histogram, and summary tables for each
  selector, plus one bounded probe of the exponential-histogram table when the
  selector matched nothing. The gateway enforces a row scan limit
  (default 200,000) and returns an error instead of truncating.
- The maximum query range is 7 days and the maximum number of range points is
  11,000.
- Query results are evaluated in the gateway, not pushed down to ClickHouse.
  This is an alpha trade-off favoring correctness and simple SQL.

## Tests

- Golden SQL tests for the emitter.
- Unit tests for selectors, lookback and range-window boundaries, counter
  resets and zero-point extrapolation, aggregations (`topk`, `bottomk`,
  `count_values`), set operators, `group_left`/`group_right`, `absent`,
  `label_replace`/`label_join`, `histogram_quantile` edge cases, `sort` NaN
  ordering, summary expansion, and the exponential-histogram error path.
- A differential oracle at `test/compat/prometheus`. It is a separate Go module
  so the main module's vendor tree is unchanged. It loads one deterministic
  series set into both engines and compares instant and range results for every
  function and operator claimed above: exact label sets and timestamps, with a
  1e-9 relative tolerance on values. Run it with:

  ```sh
  cd test/compat/prometheus
  go test ./...
  ```

- Integration tests against ClickHouse 26.8 for instant queries, range queries,
  histogram quantile, summary expansion, exponential-histogram errors,
  metadata, and label normalization.
