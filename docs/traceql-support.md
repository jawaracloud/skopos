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

Trace-by-ID returns OTLP JSON by default and OTLP protobuf when requested via
the `Accept` header. Trace and span IDs are hexadecimal. The v2 protobuf response
uses the Tempo `TraceByIDResponse` envelope (field 1 contains the OTLP trace);
the v1 protobuf response is the OTLP trace itself. The envelope is encoded
manually so the gateway does not import Tempo's AGPL-licensed proto module.
Grafana's Tempo datasource requests v2 protobuf first and falls back to v1 on
404.

## TraceQL subset

A single spanset `{ ... }` with `&&` and `||` is supported. Conditions are
`key op value` with these operators:

- `=`, `!=`
- `=~`, `!~`
- `>`, `<`, `>=`, `<=`

Supported key forms:

- `resource.<attribute>` — resource attributes; `resource.service.name` maps to
  the exporter's `ServiceName` column.
- `span.<attribute>` — span attributes.
- Unscoped attributes are looked up in span attributes first, then resource
  attributes.
- Intrinsics: `service.name`, `name`/`span.name`/`span:name`,
  `status`/`span.status`/`span:status`, `kind`/`span.kind`/`span:kind`,
  `duration`/`span.duration`/`span:duration`.

Durations accept Go duration strings such as `1s`, `250ms`, or `1m`. Status and
kind comparisons are case-insensitive. Regex matchers are fully anchored, as in
Tempo. A condition on a missing attribute does not match, including `!=` and
`!~`.

## Tag-based search

Tempo's `tags=` parameter is supported as logfmt-like key/value pairs. Values
are matched as case-insensitive substrings, as in Tempo.

`minDuration`, `maxDuration`, `limit`, `start`, and `end` are supported.
`start`/`end` accept Unix seconds or RFC3339.

## Search response

Search returns trace summaries:

```json
{
  "traces": [
    {
      "traceID": "4bf92f3577b34da6a3ce929d0e0e4736",
      "rootServiceName": "checkout",
      "rootTraceName": "GET /checkout",
      "startTimeUnixNano": "1793491200000000000",
      "durationMs": 2000
    }
  ],
  "metrics": {}
}
```

`spanSets` are not included yet.

## Known limitations

- No structural operators (`>>`, `<<`, `~`), pipelines, trace-level queries,
  metrics queries, or multiple spansets.
- No event/link scopes in TraceQL; tags endpoints expose resource, span, and
  intrinsic scopes.
- Search summaries use the matched spans in the requested time range. A partial
  time range can therefore produce a partial root/duration.
- No pagination or `spss` span-set limiting.
- The maximum query range is 7 days and the gateway enforces a row scan limit
  (default 200,000).

## Tests

- Unit tests for the TraceQL subset, tag search, and search grouping.
- API tests for JSON and protobuf trace responses, tags, tag values, echo, and
  error handling.
- Integration tests against ClickHouse 26.8 with real `otel_traces` rows.
