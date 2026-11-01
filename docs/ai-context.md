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
- `include` is a whitelist: `labels`, `query`, `runbook`, `related`, `caveats`,
  `provenance`.
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
GET /api/v1/context/metrics/{name}?detail=minimal
GET /api/v1/context/metrics/{name}?detail=full&include=labels,caveats
GET /api/v1/context/services/{service}?limit=10
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

### Service response

`GET /api/v1/context/services/checkout` returns the service description, owner,
runbook, a bounded list of metrics, and optional log/trace hints. It does not
inline full metric contexts; the agent requests those separately by name.

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

## Agent flow

```text
1. list metrics for a service or role
2. get one metric context with detail=minimal
3. optionally request detail=full or include=labels
4. run the recommended query
```

This keeps the model context small: one compact service card, one compact
metric, then a query.

## Phase 2

- MCP tools: `get_service_context`, `get_metric_context`, `list_metrics`,
  `query_promql`, `query_logql`, `search_traces`.
- Automatic observed metadata with `source=clickhouse`, `confidence` lower than
  explicit catalog.
- Log and trace context cards.
- Coverage metrics such as `skopos_context_coverage_ratio`.
- `/.well-known/skopos.json` discovery.
