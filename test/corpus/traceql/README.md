# TraceQL corpus

`corpus.json` is a deterministic trace corpus with reference search, tag, and
trace-by-id results. It is consumed by:

- `internal/tempo/corpus_test.go` (in-memory gateway API)
- `internal/tempo/corpus_integration_test.go` (real ClickHouse, build tag
  `integration`)
- `test/compat/tempo` (real Grafana Tempo container)

The traces have a fixed base time (`baseTimeUnixNano`, 2026-11-01T00:00:00Z) and
span offsets relative to that base. The Tempo harness shifts the base to
`now - 30s` so the data lands inside Tempo's live-trace window; relative
ordering, durations, attributes, events, and links are unchanged.

## Provenance

Expected results were captured from **Grafana Tempo 2.7.2** running with the
local backend and the OTLP HTTP receiver. They were not generated from the
gateway. The differential harness re-checks the corpus against Tempo on every
run, so a corpus edit that disagrees with the reference fails twice: once for
the expected set and once for the Tempo result.

Queries marked `skipTempo` exercise gateway behavior that Tempo 2.7 does not
implement (the `nil` extension, `span:id` search, and `link:traceID` tag
values). The gateway test still checks them.

## Format

- `traces[].spans[]`: span name, service, kind, status, start offset (ns),
  duration (ns), resource attributes, span attributes, events, and links.
- `queries[]`: TraceQL query, expected trace IDs, and expected matched span IDs.
  `expectError` marks queries that must return HTTP 400. The optional
  `expectErrorContains` string must appear in the HTTP 400 error body and names
  the unsupported feature (structural operator, multiple spansets, pipeline,
  metrics query, grouping, or malformed syntax).
- `tagNameTests[]`: expected names for each Tempo v2 tag scope.
- `tagValueTests[]`: expected `/api/v2/search/tag/{tag}/values` values.
- `traceTests[]`: expected span IDs, names, root span, events, and links for
  trace-by-id.

Rejected query cases are a gateway contract: they are always checked for
HTTP 400 on the gateway and are not sent to Tempo, because Tempo supports
several forms (structural operators, multiple spansets, pipelines, metrics)
that the gateway intentionally rejects. The differential harness still checks
every accepted query against Tempo.
