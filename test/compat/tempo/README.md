# Tempo differential harness

This harness starts a real Grafana Tempo container, pushes the deterministic
TraceQL corpus over OTLP/HTTP, starts the gateway's Tempo API in-process with the
same spans, and compares:

- `/api/search` trace IDs and matched span IDs
- `/api/v2/traces/{id}` JSON spans (IDs, names, parent IDs, start/duration,
  status, kind, attributes, events, links)
- `/api/v2/search/tags` scopes and names
- `/api/v2/search/tag/{tag}/values` for the corpus tag cases

It never links or vendors Tempo code. Tempo is only a container and an HTTP
reference, so no AGPL dependency enters the Go module.

## Run

```sh
test/compat/tempo/run.sh
```

or:

```sh
TEMPO_COMPAT=1 go test ./test/compat/tempo -run TestDifferential -v
```

Environment variables:

- `TEMPO_IMAGE` overrides the pinned image (default `grafana/tempo:2.7.2`).
- `DOCKER` overrides the Docker CLI binary (default `docker`).

Docker is required. When `TEMPO_COMPAT` is not set the test skips, so
`go test ./...` stays fast and Docker-free.

## What the harness does

1. Allocates two free localhost ports and writes a Tempo config with the local
   backend, OTLP HTTP/gRPC receivers, compactor disabled, and a one-hour
   `max_block_duration` / `trace_idle_period`. The long window keeps the corpus
   in Tempo's live traces for the whole comparison.
2. Starts `grafana/tempo:<version>` with `docker run`, waits for `/ready`, and
   pushes the corpus rendered as OTLP/HTTP JSON to `/v1/traces`.
3. Waits until Tempo returns every corpus trace from `{}` (live-trace search is
   asynchronous), then runs the corpus queries against both servers.
4. Compares search span sets, trace-by-id spans, tag scopes, and tag values.
5. Removes the container with `docker rm -f` in `t.Cleanup`.

## Limitations

- The corpus marks structural operators, multiple spansets, pipelines, metrics
  queries, grouping, functions, and malformed TraceQL as `expectError`. Those
  cases are checked against the gateway for HTTP 400 and a named-feature error
  body; they are not sent to Tempo, which supports several of those forms.
- Tempo 2.7.2 does not return results for TraceQL `span:id` search, and its
  tag-values endpoint returns empty for `link:traceID`; those corpus cases are
  marked `skipTempo` and are still checked against the gateway.
- Tempo 2.7 rejects `= nil` (added in Tempo 2.10); the corpus marks the `nil`
  case gateway-only.
- The harness compares JSON responses. The gateway's v2 protobuf envelope is
  covered by `internal/tempo` tests instead, because Tempo's protobuf messages
  cannot be imported without an AGPL dependency.
- If Docker or the image cannot be pulled, the harness fails when
  `TEMPO_COMPAT=1`; without that variable it skips.
