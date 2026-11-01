# LogQL support (alpha)

Status: **alpha**. No compatibility guarantee. Anything not listed here returns
an explicit error.

The reference implementation for this document and the differential corpus is
`grafana/loki:3.5.7`. Skopos stores logs in the ClickHouse exporter's
`otel_logs` table; it does not synthesize Loki's `detected_level` label or
parser diagnostic labels (`__error__`, `__error_details__`).

## Endpoints

| Endpoint | Status |
| --- | --- |
| `GET/POST /loki/api/v1/query` | Implemented |
| `GET/POST /loki/api/v1/query_range` | Implemented |
| `GET /loki/api/v1/labels` | Implemented |
| `GET /loki/api/v1/label/{name}/values` | Implemented |
| `GET /loki/api/v1/series` | Implemented |
| `GET /loki/api/v1/status/buildinfo` | Implemented |

## Stream selectors and line filters

```logql
{service_name="checkout", severity_text=~"ERR.*"}
{service_name="checkout"} |= "timeout"
{service_name="checkout"} != "debug"
{service_name="checkout"} |~ "timeout|deadline"
{service_name="checkout"} !~ "healthcheck"
```

Supported selector matchers: `=`, `!=`, `=~`, `!~`. Label regex matchers and
label-filter regexes are fully anchored; line-filter regexes are unanchored.
A missing selector label is treated as the empty string, so `{missing=""}`
matches streams without that label and `{missing!="x"}` also matches them.

`query` and `query_range` support `start`, `end`, `limit`, and `direction`
(`forward` or `backward`, default backward). For log queries `step` is ignored.
The instant `/query` endpoint accepts `time` and evaluates a 5-minute window
ending at that time; without `time` it uses the last hour.
`start`/`end` accept RFC3339, Unix seconds, milliseconds, microseconds, or
nanoseconds; Grafana's nanosecond values are supported.

## Query pushdown

The ClickHouse log fetcher pushes a subset of the selector and pipeline into
SQL. Pushdown is only a pre-filter: the gateway still evaluates the parsed
query in Go on every returned row, so results are unchanged when a predicate
cannot be pushed or when an index is unavailable.

Pushed into `WHERE`:

- Stream selectors on the exporter columns `service_name` -> `ServiceName`,
  `severity_text` -> `SeverityText`, `trace_id` -> `TraceId`, `span_id` ->
  `SpanId`, and `event_name` -> `EventName`. `=` and `!=` compare the column
  directly; `=~` and `!~` use ClickHouse `match` with the same anchored
  `^(?:...)$` pattern the Go evaluator compiles.
- All line filters: `|=` -> `position(Body, ?) > 0`, `!=` ->
  `position(Body, ?) = 0`, `|~` -> `match(Body, ?)`, `!~` ->
  `NOT match(Body, ?)`. `position` and Go `strings.Contains` are byte-exact and
  case-sensitive; `match` and Go `regexp` are RE2-family.
- Multiple selectors on the labels, label-values, and series endpoints are
  pushed as an OR only when every selector has a pushable predicate. If any
  selector is unconstrained (for example it filters only an attribute or an
  extracted label), the endpoint falls back to the unfiltered range.

Not pushed:

- Parser stages (`| json`, `| logfmt`) and label filters. They depend on
  extracted labels whose normalization and resource/log precedence are
  implemented in Go.
- Selectors on resource or log attributes (for example `k8s_pod_name`), even
  when a matching materialized column exists, until that normalization and
  precedence can be reproduced exactly.
- A regex that ClickHouse cannot compile. The fetcher retries the unfiltered
  query when ClickHouse returns a regex compilation error, so a Go-valid
  pattern never becomes a query error.

Known-column predicates include an extra `column = ''` arm. The exporter's
dedicated column is authoritative when it is non-empty, but `addAttributes`
fills an empty built-in label from `ResourceAttributes` and then
`LogAttributes`; keeping empty-column rows in the candidate set preserves that
fallback for the Go evaluator.

The API decodes ClickHouse JSON, which replaces invalid UTF-8 bytes with
U+FFFD, while ClickHouse predicates see the raw bytes. Regex filters (and
substring or selector values that contain U+FFFD) are therefore guarded with
`NOT isValidUTF8(...)`: rows with invalid bytes stay in the candidate set and
the Go evaluator decides. For regex filters this makes a text index a hint
rather than an exact skip, because ClickHouse must also read rows with invalid
UTF-8.

### Index dependencies

`match(Body, ...)` can use a case-sensitive full-text index on `Body`:

```sql
INDEX idx_body Body TYPE text(tokenizer = 'splitByNonAlpha')
```

The Helm schema defines this index. Existing deployments that predate it must
add and materialize it:

```sql
ALTER TABLE <database>.<logs_table>
    ADD INDEX idx_body Body TYPE text(tokenizer = 'splitByNonAlpha');
ALTER TABLE <database>.<logs_table> MATERIALIZE INDEX idx_body;
```

ClickHouse uses this index only when it can extract complete tokens from the
pattern (for example a phrase or anchored literal words); an unanchored
`match(Body, 'timeout')` is a substring regex and may still scan. `position`
substring filters are exact but do not use the text index at all.
`idx_lower_body` is not used by LogQL because `|=`, `|~`, and their negations
are case-sensitive.

Pushdown reduces rows read, but the gateway still applies the full selector,
parser stages, label filters, and metric aggregation in Go, and still enforces
the row scan limit.

## Parser stages

### `| json`

```logql
{service_name="checkout"} | json
{service_name="checkout"} | json level="level", code="error.code"
{service_name="checkout"} | json | level="error" | status >= 500
```

- `| json` without arguments extracts top-level scalar fields. Nested objects
  are flattened with an underscore (`{"a":{"b":1}}` becomes `a_b=1`); arrays
  and `null` are skipped. Number spelling is preserved (`1.50` stays `"1.50"`).
- `| json label="path"` extracts only the listed fields. `path` is a
  dot-separated object path such as `error.code`; array indexes, wildcards, and
  JMESPath expressions are rejected.
- Extracted names are normalized with the same `[A-Za-z0-9_]` mapping as other
  labels. If an extracted label collides with an existing stream label and has
  a different value, it is stored as `<name>_extracted`.
- A line whose body is not a JSON object is kept without extracted labels;
  Skopos does not add Loki's `__error__` diagnostic labels. Add a label filter
  if unparseable lines should be dropped.

### `| logfmt`

```logql
{service_name="checkout"} | logfmt
{service_name="checkout"} | logfmt | status >= 500 | level="error"
```

The parser accepts whitespace-separated `key=value` pairs and double-quoted
values with Go-style escapes. Invalid tokens are skipped and the line is kept
(non-strict behavior). `| logfmt` flags such as `--strict` are rejected.

## Label filters

Label filters run after parser stages and see both stream labels and extracted
labels:

```logql
| level="error"
| level!="debug"
| level=~"err(or)?"
| status >= 500
| duration > 10
```

- `=`, `!=`, `=~`, and `!~` compare strings. The value may be quoted; numeric
  literals are also accepted for `=` and `!=`. A missing label is the empty
  string, as in Loki.
- `>`, `>=`, `<`, and `<=` require an unquoted number. A missing or
  non-numeric label does not match.
- Regex filters are fully anchored.
- Filters may be interleaved with parser stages and line filters in any order.

## Log metric queries

```logql
count_over_time({service_name="checkout"}[5m])
rate({service_name="checkout"} | json | level="error" [5m])
sum by (severity_text) (count_over_time({service_name="checkout"}[5m]))
sum by (level) (rate({service_name="checkout"} | json [5m]))
sum (count_over_time({service_name="checkout"}[5m]))
```

Supported functions: `count_over_time` and `rate`. `rate` returns log entries
per second (`count / range seconds`), not bytes per second.

Supported aggregation: bare `sum(...)`, which produces one `{}` series, and
`sum by (label, ...) (...)`, which keeps only the listed labels. Group labels
missing from a stream are omitted from the output series, matching Loki. Other
aggregations and nested aggregations are rejected.

`query_range` returns Loki's `resultType: "matrix"`; an instant `/query`
returns `resultType: "vector"`. Behavior:

- `step` is required for metric `query_range` and accepts a Go duration
  (`60s`, `5m`) or a number of seconds (`60`). The instant endpoint ignores
  `step`.
- Evaluation points match Loki: the first point is the requested `start`
  rounded down to a Unix-epoch multiple of `step`, and the last is `end`
  rounded up to the next multiple. Samples are emitted in ascending time order.
- The range selector is inclusive at both ends: `[5m]` at time `t` includes an
  entry whose timestamp is exactly `t-5m`.
- Zero-count steps are omitted, as in Loki. A series is present only if it has
  at least one non-zero sample.
- Without `sum by`, the series labels are the stream labels plus labels
  extracted by parser stages.
- `limit` and `direction` apply to log queries only and are ignored for metric
  queries.
- A metric query fetches one range-selector lookback before its first
  evaluation point. The API enforces a 7-day query range and a 7-day maximum
  range selector; the fetch is also bounded by the row-scan limit.
- Metric results are capped at 100,000 samples per query. A query that would
  exceed the cap returns an explicit error rather than truncated data.

Supported range units: `ms`, `s`, `m`, `h`, `d`, `w`, `y`, including compound
values such as `1h30m`. Zero, negative, fractional, and overflowing ranges are
rejected.

## Labels

The following stream labels are exposed when present:

- `service_name`
- `severity_text`
- `severity_number`
- `trace_id`
- `span_id`
- `event_name`
- Resource and log attributes, normalized with the same `[A-Za-z0-9_]`
  mapping as PromQL. For example `k8s.pod.name` becomes `k8s_pod_name`.

`/loki/api/v1/series` returns one label set per distinct stream. It can return
multiple sets when severity, trace ID, or other labels differ. Labels, label
values, and series accept either `match[]` selectors or a `query` LogQL
expression. If the expression contains parser stages, metadata endpoints expose
the extracted labels as well. Metric expressions are rejected on these
endpoints with an explicit error.

## Known limitations

Explicitly rejected:

- Parser stages `| pattern`, `| regexp`, `| unpack`, `| ip`, and `| pack`.
- Pipeline stages `| line_format`, `| label_format`, `| drop`, `| keep`,
  `| decolorize`, `| unwrap`, and `| label_filter`.
- LogQL metric functions other than `count_over_time` and `rate`, including
  `bytes_rate`, `bytes_over_time`, `sum_over_time`, `avg_over_time`,
  `max_over_time`, `min_over_time`, `quantile_over_time`, and `stdvar_over_time`.
- Aggregations other than `sum` / `sum by`, and arithmetic between metric
  expressions or selectors.
- `offset`, vector matching, `bool`, and query frontend sharding syntax.

Other limits:

- The maximum query range is 7 days (configurable, default 7 days) and the
  gateway enforces a row scan limit (default 200,000 rows).
- Metric aggregation is computed in the gateway over the fetched log rows. A
  query that scans more than the row limit returns an error instead of a
  partial result. This is acceptable for the alpha single-node deployment but
  is not a substitute for Loki's chunk-level aggregation.
- Log queries return the raw matching lines; extracted labels are added to the
  stream label set, but the line body is not reformatted.

## Tests

- Unit tests for selectors, line filters, parser stages, label filters,
  extraction collisions, numeric comparisons, missing labels, regex anchoring,
  metric parsing, step alignment, zero-sample omission, limits, and errors.
- Pushdown tests for every matcher and line-filter operator, missing columns,
  multiple selectors, parameterization, and ClickHouse parameter escaping.
- API tests for streams, metrics, instant vectors, labels, label values,
  series, `query`/`match[]` filters, limits, direction, and unsupported queries.
- Integration tests against ClickHouse 26.8 with real `otel_logs` rows,
  including JSON/logfmt parsing and metric queries. The pushdown suite runs the
  same requests through the API with and without pushdown and requires
  byte-identical responses, including rows with invalid UTF-8; an EXPLAIN test
  also checks `idx_body` usage for a tokenizable `match` predicate.
- Deterministic corpus: `test/corpus/logql/corpus.json`, checked by
  `internal/loki.TestLogQLCorpus` and by the real-Loki differential harness.
- Differential harness: `test/compat/loki/run.sh verify` (Docker + Python 3);
  see `test/compat/loki/README.md` for the optional seeded-gateway comparison.
