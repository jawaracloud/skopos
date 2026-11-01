# cerberus three-way differential

This harness compares three independent readings of one deterministic corpus:

1. the reference implementation (a real Prometheus TSDB for PromQL, recorded
   Loki 3.5.7 responses for LogQL, recorded Tempo 2.7.2 responses for TraceQL);
2. [cerberus](https://github.com/tsouza/cerberus) v1.23.1, reading ClickHouse;
3. the Skopos gateway under review, reading the same ClickHouse rows.

ADR 0001 names cerberus as the behavioral reference and makes the three-way
diff a beta gate. Cerberus is only run as a pinned container image; no cerberus
code is vendored or copied.

## Run it

```sh
# Gate used by CI: fails only when Skopos diverges from the reference.
test/compat/cerberus/run.sh

# Show every case and unified diff, including cerberus-only gaps.
test/compat/cerberus/run.sh --verbose

# Strict three-way mode: any known cerberus gap fails too.
test/compat/cerberus/run.sh --strict

# Keep the stack for inspection.
CERBERUS_COMPAT_KEEP=1 test/compat/cerberus/run.sh --verbose
```

The command starts ClickHouse, cerberus, and Prometheus with Docker Compose,
builds and starts the working tree's `cmd/gateway`, seeds all three signals,
and removes the stack afterwards. It needs Docker with Compose v2, Go 1.27,
Python 3, and `curl`. The runner uses non-privileged containers only; it is
reliable on a stock `ubuntu-24.04` GitHub runner.

Host ports (override with the environment variables in `run.sh`):

| Service | Default |
| --- | --- |
| ClickHouse HTTP | `18131` |
| ClickHouse native | `19031` |
| cerberus | `18092` |
| Prometheus | `18093` |
| Skopos | `18088` |

## Pinned reference

| Component | Pin |
| --- | --- |
| cerberus | `ghcr.io/tsouza/cerberus:v1.23.1@sha256:e5ced00b1ee85c371295147ed0bce9caa4a5c5554443ee8aabc329d5351dc4a5` |
| ClickHouse | `clickhouse/clickhouse-server:26.8.36.3@sha256:d426d2f612e1e189c90f9a3f8dcd85a03c6851273833817a296bb327e63aa7a7` |
| Prometheus | `prom/prometheus:v3.15.1@sha256:fa3302279710be2f21a8301ec17a8b4f2301fd829bd113839b3b6b02b6f191a2` |
| Loki (recorded reference) | `grafana/loki:3.5.7` (`test/compat/loki`) |
| Tempo (recorded reference) | `grafana/tempo:2.7.2` (`test/compat/tempo`) |

Bump a pin in `docker-compose.yml`, then run `--verbose`. The cerberus digest is
the multi-arch manifest digest reported by `docker image inspect
ghcr.io/tsouza/cerberus:<tag> --format '{{json .RepoDigests}}'`.

## Corpus

| Signal | Cases | Source |
| --- | ---: | --- |
| PromQL | 44 (36 instant, 8 range) | `promql.json` + `metrics.json` |
| LogQL | 11 | `test/corpus/logql/corpus.json` |
| TraceQL search | 55 | `test/corpus/traceql/corpus.json` |
| TraceQL tag scopes | 5 scopes in 1 check | same corpus |
| TraceQL tag values | 10 | same corpus |
| TraceQL trace-by-id | 3 | same corpus |
| **Total** | **124 checks** | |

PromQL covers selectors and regex matchers, `rate`/`increase`, classic
`histogram_quantile`, histogram `_count`/`_sum`, summary quantiles/count/sum,
aggregations (`sum`/`avg`/`count`/`topk`/`bottomk`/`count_values`), binary and
set operators, vector matching with `group_left`, `label_replace`, `absent`,
`*_over_time`, `sort_desc`, `round`, `clamp`, and the same query shapes over
ranges. Summary cases are included deliberately even though cerberus does not
read the summary table; they are recorded as a cerberus gap below.

The metrics fixture is generated from `metrics.json` into two identical
datasets: rows in the OTel ClickHouse tables (`otel_metrics_gauge`,
`otel_metrics_sum`, `otel_metrics_histogram`, `otel_metrics_summary`) and an
OpenMetrics file imported into a real Prometheus TSDB. The OTel histogram
`BucketCounts` column is per-bucket; the OpenMetrics writer accumulates it into
Prometheus `_bucket` series. This distinction was a corpus bug in the first
draft of this harness: the gateways correctly reconstructed cumulative buckets
from per-bucket rows while the reference had been given cumulative counts, so
`histogram_quantile` disagreed. Fixing the fixture, not any gateway, made all
three agree.

Log entries model Loki stream labels as OTel resource attributes
(`service.name`, `severity_text`) and also fill the exporter's `ServiceName`
and `SeverityText` columns. The corpus is the same 11-case file used by
`test/compat/loki`; its expected values were recorded from Loki 3.5.7 and the
live reference check is `test/compat/loki/run.sh verify`.

Trace spans come from `test/corpus/traceql` (3 traces, 7 spans) and are inserted
with the corpus package's own `ClickHouseRows` renderer. Expected search and
tag results are the corpus values recorded from Tempo 2.7.2; the live reference
check is `test/compat/tempo/run.sh`.

## Normalization rules (presentation only)

The runner normalizes only these differences, each of which is visible in the
wire format rather than in the query answer:

| Area | Rule |
| --- | --- |
| Ordering | Sort series, streams, samples, tags, tag values, trace summaries, and spans deterministically. |
| Prom floats | Compare numeric values at 12 significant digits so `0.6666666666666666` and `0.6666666666666667` agree; NaN and ±Inf compare exactly. |
| Prom timestamps | Normalize epoch seconds to integers. |
| Loki envelope | Ignore the `stats` object and HTTP-level envelope differences. |
| Loki synthesized labels | Drop `detected_level`, `__error__`, `__error_details__`; they are Loki-generated, not query inputs or corpus outputs. |
| Tempo search envelope | Compare trace IDs, matched span IDs, root service/name, trace start, and full trace duration. Ignore `metrics`, duplicate `spanSet`/`spanSets` projection, per-span `name` in search summaries, and `serviceStats`; those are not part of the corpus search contract. The full trace summary (start = min span start, duration = max end − min start, root = earliest parentless span) is computed from the corpus spans. |
| Tempo IDs | Accept hex or base64 IDs; decode base64 to hex. Span IDs may be 16 hex characters, trace IDs 32. |
| Tempo enums | Normalize `SPAN_KIND_*` and `STATUS_CODE_*` strings and numeric OTLP enums to lowercase names. |
| Tempo attributes | Normalize the OTLP key/value list and the ClickHouse map form to one string map. |
| Tempo tag values | Ignore the value `type` (`keyword`, `status`, `string`) and case-fold values. |
| Tempo error parity | Compare HTTP status, not the error body text; the corpus expects 400 for the explicitly rejected constructs. |
| Trace-by-id | Compare span IDs/parents/names/timestamps/kind/status/resource and span attributes in the common set. Events and links are compared against the corpus per gateway because cerberus omits them from trace-by-id responses (gap below). Status message is not in the corpus contract; cerberus also omits it from its v2 status object. |

No query-level difference is normalized away. A difference in a selector, an
aggregation, a metric value, a matched span set, a tag name/value, or an event
or link is printed as a unified diff. The harness prints every case with
`--verbose` and every failure by default.

## Current result on this host (2026-11-01)

Docker 29.9.2, Compose 5.6.1, kernel 6.18.55, x86_64.

```text
summary: 124 cases; 85 pass; 39 known cerberus gaps; 0 cerberus regressions; 0 fixed baseline entries; 0 Skopos divergences
ok: Skopos matches the reference on every corpus case
```

The gate is zero for Skopos against the reference, and zero against cerberus on
every case where cerberus implements the feature. Cerberus v1.23.1 diverges on
39 checks; every one was traced to cerberus behavior, not to Skopos or the
corpus. The exact case keys are pinned in `known-cerberus-gaps.json`: a new
cerberus divergence fails the gate as `CERBERUS_REGRESSION`, and a documented
gap that starts passing fails as `CERBERUS_FIXED` until the baseline is updated.
The next section groups the 39 by root cause with a reproducer.

## Known cerberus gaps

These are precise remaining gaps in the reference gateway at the pinned
version. They are reported, never silently skipped. Running `--strict` fails
until they are fixed upstream or the corpus is intentionally narrowed.

### 1. PromQL summary metrics are not read (4 checks)

Cerberus reads the gauge/sum/histogram tables and validates the summary table,
but its PromQL path has no summary reader. Skopos and Prometheus both return
`<name>{quantile=...}`, `_count`, and `_sum`.

Affected: `summary_quantile`, `summary_count`, `summary_sum`,
`summary_count_range`.

```sh
curl -sG localhost:18093/api/v1/query --data-urlencode 'query=demo_rpc_duration_seconds_count' --data-urlencode time=1793491500
# reference: 110
curl -sG localhost:18092/api/v1/query --data-urlencode 'query=demo_rpc_duration_seconds_count' --data-urlencode time=1793491500
# cerberus: empty result
```

### 2. PromQL range rate drops the +Inf histogram bucket (1 check)

For an instant query cerberus returns `_bucket{le="+Inf"}`; for a
`query_range` `rate(_bucket[2m])` it omits it, so
`histogram_quantile(0.9, ...)` becomes NaN after the first step. Prometheus and
Skopos return 0.9.

Affected: `histogram_quantile_range`.

```sh
curl -sG localhost:18092/api/v1/query_range \
  --data-urlencode 'query=rate(demo_latency_seconds_bucket[2m])' \
  --data-urlencode start=1793491320 --data-urlencode end=1793491500 --data-urlencode step=60
# no le="+Inf" series
```

### 3. LogQL rate range step alignment (1 check)

Loki aligns range-query steps to Unix-epoch multiples of `step` (the corpus
case has `start=00:01`, `end=00:06`, `step=5m`, so the only evaluation point is
`00:05`). Cerberus evaluates at `00:01` and `00:06`, returning two smaller
rates. Skopos matches Loki.

Affected: `rate`.

```sh
curl -sG localhost:18092/loki/api/v1/query_range \
  --data-urlencode 'query=rate({service_name="checkout", severity_text="ERROR"}[5m])' \
  --data-urlencode start=2026-11-01T00:01:00Z \
  --data-urlencode end=2026-11-01T00:06:00Z \
  --data-urlencode step=5m
# cerberus returns 1793491260 and 1793491560; reference returns 1793491500
```

### 4. TraceQL search summary uses the matched span, not the full trace (20 checks)

When the matching spans do not include the root, cerberus reports the earliest
matched span as `startTimeUnixNano` instead of the trace start. When a child
extends past the root's end (the catalog trace), cerberus reports the root span
duration as `durationMs` instead of `max(end) - min(start)`. Tempo and Skopos
report the full trace.

Reproducers:

```sh
# catalog trace: root lasts 100ms, child ends at 130ms after trace start.
curl -sG localhost:18092/api/search --data-urlencode 'q={ }' --data-urlencode start=1793491140 --data-urlencode end=1793491800
# cerberus durationMs=100; reference and Skopos durationMs=130

# matched span is a child: trace start becomes the child's start.
curl -sG localhost:18092/api/search --data-urlencode 'q={ span:status = ok }' --data-urlencode start=1793491140 --data-urlencode end=1793491800
# trace 4bf... start=1793491200500000000; reference and Skopos start=1793491200000000000
```

### 5. TraceQL event time-since-start is unsupported (1 check)

`{ event:timeSinceStart >= 1s }` matches the checkout trace on Tempo and
Skopos; cerberus returns an empty trace list.

Affected: `event time since start`.

### 6. TraceQL unsupported-construct error parity (6 checks)

The corpus marks pipelines, structural operators, multiple spansets, metrics
queries, unknown span intrinsics, and instrumentation attributes as explicit
errors. Tempo 2.7 and Skopos return HTTP 400. Cerberus returns HTTP 200 with an
empty result (the metrics query even returns a synthetic `|...` trace ID).

Affected: `pipeline rejected`, `structural rejected`,
`multiple spansets rejected`, `metrics rejected`,
`unknown span intrinsic rejected`, `instrumentation attributes rejected`.

```sh
curl -s -o /dev/null -w '%{http_code}\n' -G localhost:18092/api/search \
  --data-urlencode 'q={ } >> { span:name = "GET /cart" }' \
  --data-urlencode start=1793491140 --data-urlencode end=1793491800
# cerberus 200; Tempo and Skopos 400
```

### 7. TraceQL tag-value discovery is incomplete (5 checks)

| Tag | Cerberus | Reference / Skopos |
| --- | --- | --- |
| `event:name` | empty | `exception` |
| `link:traceID` | empty | `6bf92f...4738` |
| `trace:rootService` | empty | `cart`, `catalog`, `checkout` |
| `span:status` | `Error`, `Ok` | `error`, `ok`, `unset` |
| `span:kind` | `Client`, `Internal`, `Server` | full fixed enum including `consumer`, `producer`, `unspecified` |

```sh
curl -s localhost:18092/api/v2/search/tag/span:kind/values?start=1793491140\&end=1793491800
```

### 8. Trace-by-id omits events and links (1 check)

For trace `4bf92f3577b34da6a3ce929d0e0e4736`, cerberus returns the spans with
correct IDs/names/attributes but no `events` (the corpus has an `exception`
event) and no `links` (the corpus links to `6bf92f...4738`). Its v2 `status`
object also has no `message`, while Skopos preserves the corpus
`statusMessage`. The event and link query paths themselves work; this is a
response-shape omission.

```sh
curl -s localhost:18092/api/v2/traces/4bf92f3577b34da6a3ce929d0e0e4736
```

### 9. Presentation-only omissions (not counted as gaps)

These are visible in the wire format but do not change any corpus query result;
the normalizer ignores them:

- Search `spanSet.spans` omits the per-span `name` field and there is no
  `serviceStats` object.
- The intrinsic tag scope contains extra aliases (`span:id`, `span:parentID`,
  `link:traceID`, ...). The reference/intrinsic set is still a subset; the
  harness accepts the superset.
- Tag scopes are returned in a different order.
- Trace-by-id v2 uses base64 IDs and enum strings; v1 uses Tempo's
  `{"batches": ...}` protobuf-JSON shape. The harness normalizes both.

## Files

| File | Purpose |
| --- | --- |
| `run.sh` | Starts the stack, seeds data, builds Skopos, runs the comparison, cleans up. |
| `docker-compose.yml` | Pinned ClickHouse, cerberus, and Prometheus services. |
| `prometheus.yml` | Empty scrape config; the TSDB is imported from OpenMetrics. |
| `seed/main.go` | Renders the chart schema, seeds logs/traces/metrics, writes OpenMetrics. Reuses `test/corpus/traceql`. |
| `metrics.json` | Deterministic metric fixture (gauges, counter, histogram, summary). |
| `promql.json` | 44 PromQL cases with the instant/range evaluation times. |
| `known-cerberus-gaps.json` | Exact baseline of the 39 documented cerberus divergences. |
| `harness.py` | Normalization, three-way comparison, exact diffs, baseline check, and reporting. |

The harness is manual by default only in the sense that it is not run on every
push; the CI job `cerberus three-way differential` in
`.github/workflows/ci.yml` runs `test/compat/cerberus/run.sh` on
`ubuntu-24.04`. It uses no privileged capabilities, no host mounts beyond the
temporary `.render` directory, and no secrets. If a future runner image lacks
Docker Compose v2, run the same command on a workstation with Docker; that is
the exact manual fallback.
