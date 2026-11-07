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
`promql` engine, and by the official upstream PromQL compliance scripts from a
pinned Prometheus release. See [Tests](#tests).

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
- `@` modifiers with an absolute Unix timestamp, `start()`, or `end()`;
  `offset` is applied relative to the `@` timestamp.
- Subqueries (`metric[5m:1m]`) are not supported and return an explicit error.

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
bounds within one histogram are coalesced, and non-monotonic bucket counts are
repaired before interpolation. Selecting two classic histograms with different
metric names but the same remaining label set is rejected instead of silently
merging their buckets.

`label_replace` and `label_join` preserve the metric name unless the destination
label is `__name__`. `label_replace` accepts any source label name, including
one that is not present (`""` matches the empty string). An empty destination
value removes the label. Replacing labels in a way that produces two series with
the same label set returns an explicit error, as Prometheus does.

`absent` and `absent_over_time` return `1` with the equality matchers of a
direct selector when their argument is empty. Regex and negated matchers are
omitted, and duplicate matchers for the same label are dropped, matching
Prometheus's historical behaviour.

## Aggregations and binary operators

- Aggregations: `sum`, `avg`, `min`, `max`, `count`, `stddev`, `stdvar`,
  `group`, `topk`, `bottomk`, `count_values` with `by` and `without`. The
  `quantile` aggregation is not implemented and returns an explicit error.
- `topk`/`bottomk` return the original series labels and metric name. `k <= 0`
  returns an empty vector, NaNs are selected last, and a NaN `k` returns an
  error as in Prometheus.
- `sum`, `avg`, `stddev`, and `stdvar` use compensated summation. `min`/`max`
  ignore NaN values unless every value in the group is NaN.
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
- PromQL warning and info annotations are not emitted. Cases that expect a
  warning/info annotation fail the official suite.
- Prometheus 3 `__type__`/`__unit__` metric metadata labels are not
  implemented.
- Prometheus 3 delayed `__name__` removal is not implemented. A function drops
  the metric name immediately, so a later `label_replace`/`label_join` cannot
  recover it from `__name__`.
- Start timestamps (`@st` in fixtures and `start_timestamp()`) are not stored
  or evaluated.
- Top-level string literals are not supported as query results.
- Instant queries that would return a range vector (a matrix, for example
  `metric[5m]`) are rejected.
- Result ordering is not guaranteed. `sort`/`sort_desc` order a single instant
  result, but the HTTP API does not promise ordering across queries and
  `topk`/`bottomk` output is label-sorted.
- Examples of unlisted functions that return an explicit error:
  `deriv`, `predict_linear`, `holt_winters`, `resets`, `changes`, `delta`,
  `idelta`, `first_over_time`, `histogram_count`, `histogram_sum`,
  `histogram_fraction`, `histogram_quantiles`, the date/time functions
  (`minute`, `month`, `year`, `day_of_month`, `day_of_year`,
  `days_in_month`), the trigonometric functions, `info`, `sort_by_label`,
  `double_exponential_smoothing`, `mad_over_time`, `ts_of_first_over_time`,
  `ts_of_last_over_time`, `min_of`/`max_of`, `range`/`step`/`start`/`end`
  used as functions (the `@ start()`/`@ end()` selector modifiers are
  supported), and the experimental `limitk`/`limit_ratio` aggregations.
- Experimental syntax is not enabled: duration expressions, binary-operator
  fill modifiers, and anchored/smoothed range selectors.
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

- The official Prometheus compliance suite at `test/compat/prometheus`. The
  harness pins Prometheus **3.15.1** (commit
  `bae3eced0a5369e050e83ea73bc381d7c4b1a719`, archive SHA-256
  `4636cc523bda30bdf92bf7b05b4491bb3d3038d89ebb3223c7b9a71f23e9e9c9`) and
  downloads the tag archive into a cache directory after verifying the
  checksum. It then runs the unmodified upstream
  `promql/promqltest/testdata/*.test` scripts through an in-process adapter
  around `internal/prom`, using the official test storage. This exercises the
  same evaluator as the gateway's query endpoints, but not the ClickHouse SQL
  translation; the differential oracle and integration tests cover that layer.
  Run it with:

  ```sh
  test/compat/prometheus/compliance.sh
  ```

  The last full run (2026-11-07) executed 2,243 eval cases: **687 passed**,
  **1,556 failed with an explicit documented unsupported feature**, 0
  unexpected failures, 0 skipped. This is **not** a full compliance pass.
  Results by file:

  | File | Pass | Expected unsupported |
  | --- | ---: | ---: |
  | `aggregators.test` | 110 | 50 |
  | `at_modifier.test` | 41 | 37 |
  | `collision.test` | 2 | 0 |
  | `duration_expression.test` | 6 | 53 |
  | `extended_vectors.test` | 22 | 152 |
  | `fill-modifier.test` | 1 | 45 |
  | `functions.test` | 193 | 245 |
  | `histograms.test` | 50 | 135 |
  | `info.test` | 1 | 69 |
  | `limit.test` | 0 | 37 |
  | `literals.test` | 20 | 5 |
  | `name_label_dropping.test` | 20 | 10 |
  | `native_histograms.test` | 0 | 522 |
  | `operators.test` | 127 | 87 |
  | `range_queries.test` | 8 | 10 |
  | `selectors.test` | 31 | 0 |
  | `staleness.test` | 10 | 7 |
  | `start_timestamps.test` | 0 | 26 |
  | `subquery.test` | 2 | 32 |
  | `trig_functions.test` | 0 | 19 |
  | `type_and_unit.test` | 43 | 15 |

  Unsupported categories, with the number of cases they account for:

  | Category | Cases | Why |
  | --- | ---: | --- |
  | `native-histograms-file` | 522 | native histogram fixture file |
  | `native-histograms-data` | 219 | native histogram samples return an explicit error |
  | `unsupported-function` | 156 | function outside the published list |
  | `extended-range-selectors` | 152 | experimental anchored/smoothed selectors |
  | `experimental-feature` | 99 | experimental function/syntax gate |
  | `subqueries` | 72 | subqueries are not supported |
  | `info-function` | 69 | experimental `info()` |
  | `duration-expressions` | 53 | experimental duration expressions |
  | `fill-modifiers` | 45 | experimental binary fill modifiers |
  | `limitk-limit-ratio` | 37 | experimental `limitk`/`limit_ratio` |
  | `start-timestamps` | 26 | `@st` and `start_timestamp()` |
  | `trig-functions` | 19 | trigonometric functions |
  | `native-histograms-expected` | 15 | expected native histogram points |
  | `annotations` | 15 | warning/info annotations are not emitted |
  | `type-unit-labels` | 15 | `__type__`/`__unit__` metadata labels |
  | `delayed-name-removal` | 10 | delayed `__name__` removal |
  | `quantile-aggregation` | 8 | `quantile` aggregation |
  | `ordered-results` | 7 | `expect ordered` checks |
  | `staleness-markers` | 7 | no staleness markers in ClickHouse |
  | `string-literals` | 5 | top-level string results |
  | `range-vector-instant` | 4 | instant matrix results |
  | `count-values-invalid-label` | 1 | error text for non-UTF-8 label input |

  The mapping from every failing case to a category lives in
  `test/compat/prometheus/compliance/exceptions.json`; the run report contains
  a per-case detail table. The gate fails if any failing case does not match a
  category, so an undocumented failure cannot pass silently.
  `compliance.sh --files selectors.test,operators.test` runs a reduced subset.

- Integration tests against ClickHouse 26.8 for instant queries, range queries,
  histogram quantile, summary expansion, exponential-histogram errors,
  metadata, and label normalization.
