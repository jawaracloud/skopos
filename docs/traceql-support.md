# TraceQL support (alpha)

Status: **alpha**. No compatibility guarantee. Anything not listed here returns
an explicit error.

## Endpoints

| Endpoint | Status |
| --- | --- |
| `GET /api/search` | Implemented |
| `GET /api/traces/{traceID}` | Implemented (OTLP JSON; protobuf with `Accept: application/protobuf`) |
| `GET /api/v2/traces/{traceID}` | Implemented |
| `GET /api/search/tags` | Implemented |
| `GET /api/v2/search/tags` | Implemented |
| `GET /api/search/tag/{tag}/values` | Implemented |
| `GET /api/v2/search/tag/{tag}/values` | Implemented |
| `GET /api/echo` | Implemented |
| `GET /api/status/buildinfo` | Implemented |
| `GET /api/metrics/query` | Not implemented (404); metrics query strings sent to `/api/search` return 400 |

Trace-by-ID returns OTLP JSON by default and OTLP protobuf when requested via
the `Accept` header. Trace and span IDs are hexadecimal. The v2 protobuf response
uses the Tempo `TraceByIDResponse` envelope (field 1 contains the OTLP trace);
the v1 protobuf response is the OTLP trace itself. The envelope is encoded
manually so the gateway does not import Tempo's AGPL-licensed proto module.
Grafana's Tempo datasource requests v2 protobuf first and falls back to v1 on
404.

The v1 JSON response is the OTLP `resourceSpans` object. The v2 JSON response
wraps it as `{"trace": {"resourceSpans": ...}}`, matching Tempo's v2 envelope.

## TraceQL subset

The supported query shape is exactly one spanset `{ ... }` with conditions
combined by `&&` (same span) and `||` (either condition). `&&` binds tighter
than `||`. Parentheses are not supported. Supported operators:

- `=`, `!=`
- `=~`, `!~`
- `>`, `<`, `>=`, `<=`

Examples that work:

```traceql
{ }
{ resource.service.name = "checkout" }
{ span:status = error || span.http.status_code >= 500 }
{ span:duration > 1s && span:kind = client }
{ span:name =~ "GET /.*" }
```

`{ }` matches every trace. An empty `q` parameter disables TraceQL filtering.

All regular expressions are fully anchored, as in Tempo 2.7 and later:
`{ span.foo =~ "bar" }` is evaluated as `^(?:bar)$`. Use `.*` for substring
matching.

### Scopes and intrinsics

| Query form | Meaning |
| --- | --- |
| `resource.<attr>` | Resource attribute. `resource.service.name` reads the exporter's `ServiceName` column. |
| `span.<attr>` | Span attribute. `span.name` reads an attribute literally named `name`; if it is absent, the span intrinsic is used for backward compatibility. |
| `event.<attr>` | Event attribute; matches if any event on the span has the value. |
| `link.<attr>` | Link attribute; matches if any link on the span has the value. |
| Unscoped `<attr>` | Span attribute first, then resource attribute. |
| `.<attr>` | Explicitly unscoped; no intrinsic fallback. |
| `span:name` | Span name intrinsic. |
| `span:duration` | Span duration intrinsic. |
| `span:status` | Span status enum: `error`, `ok`, `unset`. Case-insensitive. |
| `span:statusMessage` | Span status message. |
| `span:kind` | Span kind enum: `server`, `client`, `producer`, `consumer`, `internal`, `unspecified`. Case-insensitive. |
| `span:id`, `span:parentID`, `span:traceID` | Hex IDs. |
| `trace:duration` | `max(end) - min(start)` over the fetched spans of the trace. |
| `trace:rootName` | Name of the earliest root span (empty parent ID), if one exists. |
| `trace:rootService` | Service name of the earliest root span, if one exists. |
| `trace:id` | Hex trace ID. |
| `event:name` | Event name intrinsic; matches if any event has the name. |
| `event:timeSinceStart` | Event timestamp minus span start, as a duration. |
| `link:traceID`, `link:spanID`, `link:traceState` | Link IDs/trace state. |
| `instrumentation:name`, `instrumentation:version` | Instrumentation scope name/version. |

Aliases accepted for Grafana and Tempo compatibility:

- `name`, `duration`, `status`, `statusMessage`, `kind` are the span intrinsics.
- `traceDuration` is `trace:duration`.
- `rootName` is `trace:rootName`.
- `rootService`, `rootServiceName`, `trace:rootServiceName` are
  `trace:rootService`.
- `trace:id`, `span:id`, and link IDs are compared case-insensitively.
- `link:traceId`/`link:spanId` are accepted as aliases for `link:traceID`/`link:spanID`.

Durations accept Go duration strings such as `1s`, `250ms`, `1m`, or `1h`.
Bare numbers are numeric comparisons. Quoted strings compare lexicographically
for `>`/`<`, matching Tempo.

### Missing attributes and `nil`

A condition on a missing attribute does not match, including `!=` and `!~`.
This matches Tempo 2.7: `{ span.missing != "x" }` returns no traces. It is not
the same as `{ span.missing = nil }`.

As a gateway extension, `= nil` and `!= nil` test attribute presence:

- `{ span.cache.hit != nil }` matches spans where the attribute exists.
- `{ span.missing = nil }` matches spans where the attribute does not exist.
- `!= nil` on a missing attribute is false; `= nil` on an existing attribute is
  false.

Tempo 2.7 itself rejects `nil` (support was added in Tempo 2.10), so corpus
queries that use `nil` are marked gateway-only in the differential harness.

### Rejected query shapes

Anything outside the shape above returns HTTP 400 with an error beginning with
`tempo:`. Unsupported syntax is never accepted and evaluated as an empty
result. Every rejection names the feature and ends with the supported subset:

```text
supported subset: one spanset { ... } with &&, || and comparison operators =, !=, =~, !~, >, <, >=, <=
```

Structural operators are rejected in every form, including after a spanset and
inside a malformed spanset:

| Operator | Meaning | Example |
| --- | --- | --- |
| `>>` | child | `{ } >> { span:name = "x" }` |
| `<<` | parent | `{ } << { span:name = "x" }` |
| `~` | sibling | `{ } ~ { span:name = "x" }` |
| `>` | descendant | `{ } > { span:name = "x" }` |
| `<` | ancestor | `{ } < { span:name = "x" }` |
| `!>>`, `!<<`, `!>`, `!<` | negated structural operators | `{ } !>> { span:name = "x" }` |

Example error:

```text
tempo: TraceQL structural operator ">>" is not supported; supported subset: one spanset { ... } with &&, || and comparison operators =, !=, =~, !~, >, <, >=, <=
```

Multiple spansets are rejected, including set operators outside the spanset and
adjacent spansets:

| Query | Error |
| --- | --- |
| `{a} && {b}` | `TraceQL multiple spansets are not supported (found "&&" between spansets)` |
| `{a} \|\| {b}` | `TraceQL multiple spansets are not supported (found "\|\|" between spansets)` |
| `{a} {b}` | `TraceQL multiple spansets are not supported` |
| `{ {a} }` | `TraceQL nested spansets are not supported` |

Pipelines and TraceQL metrics queries are not implemented. Search evaluates
conditions per span and has no trace-level aggregation stage, and the gateway
does not register `/api/metrics/query`. These forms are rejected rather than
silently ignored:

| Query | Error |
| --- | --- |
| `{a} \| count() > 1` | `TraceQL pipeline "\| count()" is not supported` |
| `{a} \| by(span:name)` | `TraceQL pipeline "\| by()" is not supported` |
| `{a} \| select(span:name)` | `TraceQL pipeline "\| select()" is not supported` |
| `{a} \| coalesce()` | `TraceQL pipeline "\| coalesce()" is not supported` |
| `{ } \| rate()` | `TraceQL metrics query "\| rate()" is not supported` |
| `{ } \| count_over_time()` | `TraceQL metrics query "\| count_over_time()" is not supported` |
| `{ } \| quantile_over_time(duration, .99)` | `TraceQL metrics query "\| quantile_over_time()" is not supported` |
| `{ } \| histogram_over_time(duration)` | `TraceQL metrics query "\| histogram_over_time()" is not supported` |
| `{ } \| compare()` | `TraceQL metrics query "\| compare()" is not supported` |

Grouping, functions, and aggregations are rejected. A structural-looking string
inside quotes is data and remains valid:

| Query | Error |
| --- | --- |
| `{ (span:name = "x") }` | `TraceQL grouping, functions, and aggregations are not supported (found "(")` |
| `{ span:count() > 1 }` | `TraceQL grouping, functions, and aggregations are not supported (found "(")` |
| `{ span:name = "GET /cart >> { span:name } \| rate()" }` | Accepted; the value is compared literally |

Other malformed shapes also fail explicitly instead of matching nothing:

| Query | Error |
| --- | --- |
| `{ span:name = "x"` | `TraceQL spanset is not closed: missing '}'` |
| `{ span:name = "x" } foo` | `TraceQL unexpected text after the spanset: "foo"` |
| `{ span:name = GET /cart }` | `TraceQL value "GET /cart" contains spaces; quote string values` |
| `{ span:foo }` | `TraceQL condition "span.foo" has no operator` |

## Tag-based search

Tempo's `tags=` parameter is supported as logfmt-like key/value pairs. Values
are matched as case-insensitive substrings, as in Tempo. Scoped keys such as
`span.http.method=GET` and `resource.service.name=checkout` are supported.

`minDuration`, `maxDuration`, `limit`, and `spss` are supported. `start`/`end`
accept Unix seconds or RFC3339. `spss` is the maximum number of matched spans
returned per span set (default 3, maximum 1000).

## Search response

Search returns trace summaries with root information, matched span sets, and
service statistics:

```json
{
  "traces": [
    {
      "traceID": "4bf92f3577b34da6a3ce929d0e0e4736",
      "rootServiceName": "checkout",
      "rootTraceName": "GET /checkout",
      "startTimeUnixNano": "1793491200000000000",
      "durationMs": 2000,
      "spanSet": {
        "spans": [
          {
            "spanID": "00f067aa0ba902b7",
            "name": "GET /checkout",
            "startTimeUnixNano": "1793491200000000000",
            "durationNanos": "2000000000"
          }
        ],
        "matched": 1
      },
      "spanSets": [ { "spans": [ ... ], "matched": 1 } ],
      "serviceStats": {
        "checkout": { "spanCount": 1, "errorCount": 1 }
      }
    }
  ],
  "metrics": {}
}
```

`spanSets[0].spans` contains up to `spss` matched spans. `matched` is the total
number of matched spans in the trace before the `spss` truncation. `spanSet`
(singular) is included for older clients. `serviceStats` counts every fetched
span in the trace, not only the matched spans, and matches Tempo's behavior.

The root service, root name, start time, and duration are computed from every
fetched span in the trace. A filtered query that matches only a child span still
reports the real root and the full trace duration.

## Tag names and values

`/api/search/tags` (v1) returns the union of resource, span, event, and link
attribute names, matching Tempo v1. `/api/v2/search/tags` returns the five Tempo
scopes in Tempo order: `span`, `event`, `link`, `resource`, and `intrinsic`.
The intrinsic list includes `trace:duration`, `trace:rootName`,
`trace:rootService`, `event:name`, `event:timeSinceStart`, `span:*`, and the
unscoped aliases.

`/api/v2/search/tag/{tag}/values` accepts scoped names such as
`resource.service.name`, `span:name`, `event.exception.type`,
`link:traceID`, and `trace:rootService`. `span:status` and `span:kind` return
Tempo's fixed enum value sets. Event and link attributes are read from the
exporter's `Events.Attributes` and `Links.Attributes` nested columns.

## Known limitations

- Structural operators (`>>`, `<<`, `>`, `<`, `~`, `!>>`, `!<<`, `!>`, `!<`),
  multiple spansets, pipelines, grouping, functions, aggregations, `select()`,
  and TraceQL metrics queries are not implemented. Every such query returns an
  explicit HTTP 400; see [Rejected query shapes](#rejected-query-shapes).
- The TraceQL metrics endpoint `/api/metrics/query` is not registered. Metrics
  query strings sent to `/api/search` are rejected with HTTP 400.
- Event and link conditions are evaluated against any event/link on the span;
  the gateway does not yet enforce that two event conditions refer to the same
  event object.
- Trace-level intrinsics are computed from the spans fetched in the requested
  time range. A partial time range can therefore produce a partial root and
  duration, as noted by Tempo for partial traces.
- Search summaries use the matched spans in the requested time range.
- `span:id` search works in the gateway but Tempo 2.7 search returns no results
  for it; the differential corpus marks it gateway-only.
- Tempo 2.7's tag-values endpoint returns an empty list for `link:traceID`; the
  gateway returns the link IDs. The differential corpus marks this gateway-only.
- No pagination and no `most_recent` hint.
- The maximum query range is 7 days and the gateway enforces a row scan limit
  (default 200,000).

## Tests

Run the deterministic corpus against the in-memory API and the parser:

```sh
go test ./internal/tempo ./test/corpus/traceql
```

Run the corpus against a real ClickHouse 26.8 instance:

```sh
CLICKHOUSE_URL=http://127.0.0.1:8123 go test -tags=integration ./internal/tempo
```

Run the Tempo differential harness (Docker required; pins
`grafana/tempo:2.7.2` and compares search, span sets, trace-by-id, tag names,
and tag values):

```sh
test/compat/tempo/run.sh
# or
TEMPO_COMPAT=1 go test ./test/compat/tempo -run TestDifferential -v
```

The harness starts Tempo with a one-hour live-trace window. It sends the same
corpus as OTLP/HTTP JSON to Tempo and seeds the gateway's API with the same
spans, then compares both. It does not link or vendor Tempo code and does not
introduce an AGPL dependency. Corpus cases marked `expectError` assert HTTP 400
and an error body naming the unsupported feature; they are gateway-only because
Tempo accepts several of those query forms.
