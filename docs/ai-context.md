# AI SRE context API

Skopos exposes an optional semantic context catalog for AI SRE agents. The goal
is to remove guessing: an agent can ask what a metric means, what its unit and
direction are, who owns it, and how to query it. The catalog is explicit,
operator-authored JSON. The gateway does not invent descriptions, roles, or
units.

The context API is separate from the Prometheus, Loki, and Tempo query APIs.
Query responses stay compatible and are not inflated with metadata.

## Token-efficient by design

The API is deliberately bounded so an agent can request context without loading
the whole catalog.

| Level | Default | Fields | Target |
| --- | --- | --- | --- |
| `minimal` | yes | name, known, type, unit, role, direction, owner, service, description, flags, sources | under 1 KB per metric |
| `standard` | no | minimal + one query, runbook, label meanings | about 2–4 KB per metric |
| `full` | no | standard + related metrics, caveats, provenance | about 4–8 KB per metric |

Limits:

- `detail` defaults to `minimal`.
- `limit` defaults to `20`, maximum `50` for metric lists; `10`/`20` for service
  metrics.
- `include` is signal-specific: metrics accept `labels`, `query`, `runbook`,
  `related`, `caveats`, `provenance`; logs add `fields`; traces add
  `downstream`.
- empty fields are omitted, not emitted as `null`.
- descriptions are at most 160 bytes, caveats at most 2, queries at most 512
  bytes.
- responses include an `ETag`; send `If-None-Match` to get `304`.
- a compact list uses only name/type/unit/role/service/flags per item.

With the example catalog, a minimal metric response is about 286 bytes and a
full response about 675 bytes. The exact token count depends on the model
tokenizer; the server enforces the field and item limits above.

## Endpoints

```text
GET /api/v1/context
GET /api/v1/context/metrics?service=checkout&role=latency&limit=20
GET /api/v1/context/metrics?observed=true&limit=20
GET /api/v1/context/metrics/{name}?detail=minimal
GET /api/v1/context/metrics/{name}?detail=full&include=labels,caveats
GET /api/v1/context/services/{service}?limit=10
GET /api/v1/context/logs?service=checkout&limit=20
GET /api/v1/context/logs/{id}?detail=minimal
GET /api/v1/context/traces?service=checkout&limit=20
GET /api/v1/context/traces/{id}?detail=minimal
GET /api/v1/context/coverage
GET /api/v1/context/suggestions
GET /.well-known/skopos.json
GET /metrics
```

All routes are protected by Basic auth when auth is enabled, like the query
APIs. `/healthz` and `/readyz` remain open.

### Minimal metric response

```json
{
  "schema": "ctx/v1",
  "name": "http_server_request_duration_seconds",
  "known": true,
  "type": "histogram",
  "unit": "s",
  "role": "latency",
  "direction": "higher_is_worse",
  "owner": "checkout",
  "service": "checkout",
  "description": "HTTP server request duration",
  "flags": ["slo_metric"],
  "sources": ["catalog"]
}
```

### Unknown metric

The gateway does not guess:

```json
{
  "schema": "ctx/v1",
  "name": "weird_custom_metric_total",
  "known": false,
  "flags": ["missing_context"]
}
```

An agent should treat this as “ask the operator or discover the signal”, not as
an invitation to invent a role.

If the gateway is configured with observed metadata (`SKOPOS_CONTEXT_OBSERVED=true`,
the default), an unknown metric can include a clearly labeled observed block:

```json
{
  "schema": "ctx/v1",
  "name": "demo_temperature_celsius",
  "known": false,
  "observed": {
    "type": "gauge",
    "unit": "Cel",
    "description": "Demo temperature",
    "source": "clickhouse",
    "confidence": 0.4
  },
  "flags": ["missing_context", "observed_only"],
  "sources": ["clickhouse"]
}
```

`known` stays false: observed metadata is factual but has no semantic catalog
contract. It must not be treated as an explicit role/direction/owner.

### List response

```json
{
  "schema": "ctx/v1",
  "version": "2026-11-01.1",
  "items": [
    {
      "name": "http_server_request_duration_seconds",
      "type": "histogram",
      "unit": "s",
      "role": "latency",
      "service": "checkout",
      "flags": ["slo_metric"]
    }
  ],
  "total": 1
}
```

`has_more` is added when the catalog is larger than the requested limit.

### Log and trace cards

Log and trace context cards use the same minimal/standard/full and include
rules:

```json
{
  "schema": "ctx/v1",
  "id": "checkout.error",
  "known": true,
  "service": "checkout",
  "event": "exception",
  "description": "Checkout request failures",
  "severity": ["ERROR", "FATAL"],
  "flags": ["slo_metric"],
  "sources": ["catalog"]
}
```

```json
{
  "schema": "ctx/v1",
  "id": "checkout.get",
  "known": true,
  "service": "checkout",
  "operation": "GET /checkout",
  "kind": "server",
  "critical_path": true,
  "description": "Checkout request path",
  "flags": ["slo_metric"],
  "sources": ["catalog"]
}
```

`GET /api/v1/context/logs` and `/traces` return compact list items. Add
`?observed=true` to include observed ClickHouse streams/operations with stable
`obslog_`/`obstrace_` IDs. Their detail response stays `known:false` and carries
an observed block; it never invents a catalog description or owner.
`detail=standard` adds the query, runbook, fields/downstream; `detail=full`
adds caveats and provenance for catalog entries.

`GET /api/v1/context/coverage` returns documented counts. Add
`?observed=true` to include observed metric counts and a documented/observed
coverage ratio from ClickHouse. If the observation source is unavailable, the
response carries `observed_unavailable` rather than failing the documented
counts. When the gateway can query ClickHouse, `observed.logs` and
`observed.traces` contain observed stream/operation counts. `min` (default
`0.8`) controls whether low-coverage alerts are emitted.

`GET /api/v1/context/suggestions` lists observed-only metrics that have no
catalog entry. It proposes observed type/unit/description only, sets
`approval: "human_required"`, and never writes the catalog. A human must review
and commit the catalog change.

`GET /metrics` exposes Prometheus text gauges for documented/observed counts,
coverage ratio, and low-coverage alerts. Scrape it with the same Basic auth as
the context API if auth is enabled.

### Service response

`GET /api/v1/context/services/checkout` returns the service description, owner,
runbook, a bounded list of metrics, and optional log/trace hints. It does not
inline full metric contexts; the agent requests those separately by name.

### Discovery

`GET /.well-known/skopos.json` describes the context API, signal list, detail
levels, and query API paths. When Basic auth is enabled, pass the same
credentials as the other APIs.

## Catalog format

See [deploy/examples/context/catalog.json](../deploy/examples/context/catalog.json)
for a complete example.

```json
{
  "schema": "ctx/v1",
  "version": "2026-11-01.1",
  "metrics": {
    "http_server_request_duration_seconds": {
      "description": "HTTP server request duration",
      "type": "histogram",
      "unit": "s",
      "role": "latency",
      "direction": "higher_is_worse",
      "owner": "checkout",
      "service": "checkout",
      "query": "histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))",
      "runbook": "https://runbooks.example.com/checkout/latency",
      "labels": {"le": "Histogram bucket upper bound"},
      "caveats": ["Use rate() on _bucket, not the raw counter."],
      "flags": ["slo_metric"]
    }
  },
  "services": {
    "checkout": {
      "description": "Checkout API",
      "owner": "payments",
      "metrics": ["http_server_request_duration_seconds"],
      "logs": ["checkout error logs"],
      "traces": ["GET /checkout"]
    }
  }
}
```

`logs` and `traces` are top-level objects keyed by URL-safe context IDs. A
service can reference those IDs in its `logs`/`traces` lists. See the example
catalog for `checkout.error` and `checkout.get`.

### Enums

`type`: `counter`, `gauge`, `histogram`, `summary`.

`role`: `latency`, `traffic`, `error_rate`, `saturation`, `resource`,
`business_kpi`, `build_info`.

`direction`: `higher_is_worse`, `lower_is_worse`, `neutral`.

### Validation

The catalog is validated at startup. Startup fails on:

- wrong or missing schema/version,
- duplicate JSON keys,
- unknown fields,
- invalid metric/service names,
- missing description, type, unit, role, or direction,
- invalid URLs, roles, directions, flags, or labels,
- oversized fields or lists,
- related/service references that do not exist,
- a catalog larger than 1 MiB.

Strict failure is intentional: a bad catalog must not reach an AI as
half-truths.

## Enable it

Helm:

```sh
helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set context.enabled=true \
  --set-file context.catalog=deploy/examples/context/catalog.json
```

Or use an existing ConfigMap with a `catalog.json` key:

```sh
helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set context.enabled=true \
  --set context.existingConfigMap=skopos-context
```

The catalog is mounted read-only at `/etc/skopos/context/catalog.json` and
`SKOPOS_CONTEXT_FILE` points at it. The frontend proxies the context API to the
query tier; no extra mount is needed on the frontend.

systemd:

```sh
SKOPOS_CONTEXT_FILE=/etc/skopos/context/catalog.json
```

See [systemd.md](systemd.md) for the service setup.

## No guessing contract

1. `known: true` means the catalog contains an explicit entry.
2. `known: false` means no context exists; no fields are invented.
3. `sources: ["catalog"]` means the data came from the operator catalog.
4. Unknown/extra fields are omitted. The API does not fall back to name-based
   inference.
5. Query responses remain pure PromQL/LogQL/TraceQL responses.

Future sources (collector metadata, OTel semantic conventions, ClickHouse
observed type/unit) must be labeled with their own provenance and confidence;
they must not silently overwrite catalog data.

## MCP server

`cmd/skopos-mcp` is a stdio MCP server for AI SRE agents. It calls the gateway
over HTTP and exposes compact context plus query tools:

```text
list_metrics
get_metric_context
get_service_context
list_logs
get_log_context
list_traces
get_trace_context
get_context_coverage
get_context_suggestions
get_well_known
query_promql
query_logql
search_traces
```

Build and run:

```sh
make mcp
SKOPOS_GATEWAY_URL=http://127.0.0.1:8080   SKOPOS_AUTH_USERNAME=viewer   SKOPOS_AUTH_PASSWORD=change-me   ./bin/skopos-mcp
```

The server speaks newline-delimited JSON-RPC 2.0 on stdin/stdout, supports the
MCP `initialize`, `tools/list`, and `tools/call` methods, and returns tool errors
as MCP `isError` results. Responses larger than `SKOPOS_MCP_MAX_BYTES` (default
64 KiB) are rejected with a “narrow the query” error instead of flooding the
model context.

### Remote MCP transport

Set `SKOPOS_MCP_HTTP_ADDR` to run the same tools over HTTP instead of stdio:

```sh
SKOPOS_GATEWAY_URL=http://127.0.0.1:8080 \
SKOPOS_MCP_HTTP_ADDR=127.0.0.1:8090 \
SKOPOS_MCP_HTTP_USERNAME=mcp \
SKOPOS_MCP_HTTP_PASSWORD=change-me \
./bin/skopos-mcp
```

```text
GET  /healthz   # probe, unauthenticated
POST /mcp       # JSON-RPC request, JSON or text/event-stream response
GET  /mcp       # SSE stream; requires Mcp-Session-Id
DELETE /mcp     # terminate the session; 204
```

`initialize` responses include `Mcp-Session-Id`; send that header on subsequent
POST, GET, and DELETE requests. GET `/mcp` sends an initial `: connected` event
and keepalive comments. `POST /mcp` accepts one JSON-RPC message and caps the
request body at 1 MiB. Put this endpoint behind
TLS and use the optional MCP HTTP Basic auth; do not expose it directly to the
internet.

## Agent flow

```text
1. get_well_known / get_service_context
2. list_metrics or list logs/traces
3. get_*_context with detail=minimal
4. optionally request detail=full or include fields
5. run the recommended query
```

This keeps the model context small: one compact service card, one compact
signal card, then a query.

## Remaining beyond Phase 4

- Resumability for interrupted SSE streams.
- A reviewed write/apply path for suggestions (the API currently only proposes
  them; it never writes the catalog).
- More detailed observed log/trace cards (current observed detail is
  service/event/operation/kind plus count, not a full query/runbook card).
