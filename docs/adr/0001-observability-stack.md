# ADR 0001: Open-source observability stack on OBI and ClickHouse

- Status: Accepted for the v0.1 alpha line
- Date: 2026-11-01
- Working name: Skopos
- License: Apache-2.0

## Context

Teams that use Grafana, Datadog, or Dynatrace want to move to an open-source
stack without rebuilding their dashboards. Skopos targets that migration: OBI
instruments applications without code changes, a Collector writes OTLP data to
ClickHouse, and a query gateway exposes Prometheus, Loki, and Tempo compatible
APIs so Grafana can keep its datasource types.

The v0.1 goal is one `helm install` on a single-node ClickHouse, with metrics,
traces, and logs labeled alpha. HA, multi-tenancy, a custom UI, and a managed
offering are out of scope.

## Decisions

| ID | Decision | Status |
| --- | --- | --- |
| D1 | Ship OSS first as a Helm chart with complete configuration. One install per user (single-tenant). | Accepted |
| D2 | Grafana is the first UI. A custom UI (React, TypeScript, Vite, Zod, Zustand) comes later. | Accepted |
| D3 | Go 1.27, stdlib-first. Non-stdlib dependencies only when they are official from the provider and required for migration. | Accepted |
| D4 | The query layer is written in-house. cerberus and oteldb are behavioral references, not dependencies. | Accepted |
| D5 | The agent is OBI plus OTel Collector, installed by Helm, with no custom supervisor or control plane. | Accepted |
| D5a | OBI runs standalone from the official image and sends OTLP to Collector contrib, which writes to ClickHouse. The OBI receiver mode needs a custom Collector build and is deferred. | Accepted 2026-11-01 |
| D6 | Metrics, traces, and logs ship together in v0.1, all labeled alpha. | Accepted |
| D7 | ClickHouse single-node is bundled in the chart. HA and replication belong to the cloud version. | Accepted |
| D8 | The schema mirrors the OTel Collector ClickHouse exporter schema, pinned to one exporter version; DDL runs in a chart init job. | Accepted |
| D9 | Alpha means no compatibility guarantee and a published support list. Beta criteria are in "Quality gates". | Accepted |
| D10 | Ingest in v0.1 is OTLP through the Collector only. Remote-write mirroring is added after verification. | Accepted |
| D11 | No built-in auth in v0.1. The gateway is ClusterIP with NetworkPolicies and a documented ingress example. | Accepted |
| D12 | TTL and PVC size are chart values. Initial numbers come from compression benchmarks on real data. Disk-full behavior is documented. | Accepted |
| D13 | Apache-2.0. No AGPL dependency. | Accepted |
| D14 | CI gates as described in "Quality gates". | Accepted |
| D15 | Development is agentic. The working rules are in CONTRIBUTING.md. | Accepted |
| D16 | Hetzner (Singapore allowed) is for demos and benchmarks only. No Indonesian data-residency claim in the initial pitch. | Accepted |
| D17 | The ClickHouse client uses the HTTP interface with stdlib `net/http`. The official driver is only adopted if proven necessary. | Accepted 2026-11-01 |
| D18 | The official Prometheus PromQL parser (Apache-2.0) is the only stdlib-first exception for parsing. LogQL and TraceQL parsers are written in-house without copying code. | Accepted 2026-11-01 |

The three previously proposed decisions (D5a, D17, D18) were confirmed before
M0 started. The name Skopos was chosen after checking the CNCF project naming
space; no CNCF project uses it.

## Architecture

```text
applications -> OBI (DaemonSet) -> OTel Collector -> ClickHouse
                                                    ^
                                                    | HTTP, read-only user
                                      Skopos gateway (Go)
                                      Prometheus/Loki/Tempo APIs
                                                    ^
                                      Grafana (user-managed)
```

OBI emits metrics and traces only. Logs require a separate Collector pipeline.
The gateway is read-only and never stores telemetry.

## Milestones

- **M0: repository foundation.** Go module, directory structure, license,
  security policy, contributing rules, dependency policy, and CI skeleton.
- **M1: data plane.** ClickHouse StatefulSet with PVC, TTL and disk values; init
  job running the pinned exporter DDL; Collector contrib with the ClickHouse
  exporter; OBI DaemonSet with least-privilege capabilities; gateway Deployment
  and Service; NetworkPolicies; Grafana datasource example; `values.schema.json`.
- **M2: query core.** Shared plan IR, ClickHouse SQL emitter, HTTP client with
  limits, basic HTTP server, structured logging, golden/fuzz/integration tests.
- **M3: PromQL head (alpha).** Grafana's endpoints, tier-1 functions, staleness,
  counter resets, and lookback tests. Published support list.
- **M4: TraceQL head (alpha).** Tempo endpoints used by Grafana, corpus compared
  with real Tempo, published support list.
- **M5: LogQL head (alpha).** Loki endpoints, Loki benchmark corpus, published
  support list.
- **M6: hardening and real numbers.** Compression, cost, p95 latency, human
  security review of the SQL emitter/Helm/NetworkPolicy, and benchmarks.
- **M7: alpha release.** README labels, support matrix, signed chart and images,
  SBOM, changelog, and a clean-cluster install following the README.

## Quality gates

Alpha means no compatibility guarantee. The supported function list is
published per language, and unsupported functions return an explicit error, not
an empty or incorrect result.

A signal can move to beta when:

1. Every claimed function passes the reference corpus without an allow-list
   (PromQL compliance tester, Loki benchmark corpus, or the in-house TraceQL
   corpus).
2. The three-way diff (reference, cerberus, gateway) is zero on the corpus.
3. No open security finding remains in the SQL emitter.

Release-blocking CI gates:

- PromQL compliance tester, Loki benchmark corpus, and the in-house TraceQL
  corpus.
- Three-way diff against real Prometheus and cerberus.
- SQL golden tests and emitter fuzzing.
- `govulncheck`, `helm lint`, and module checksum verification.

The Prometheus compliance tester has limited coverage (for example, instant
queries are not tested). Passing it does not prove correctness. The project also
runs differential queries from the supported grammar against real Prometheus and
the gateway.

## Risks and open items

| Item | Status | How to resolve |
| --- | --- | --- |
| ClickHouse exporter exists in the pinned contrib distribution | Verified: exporter is in contrib v0.164.0; image pinned by digest. | Re-check on every version bump. |
| Collector remote-write receiver for mirroring | Not verified | Check component and maturity before enabling. |
| Tempo wire format requested by Grafana (JSON vs protobuf) | Verified: Tempo returns OTLP JSON by default and protobuf with `Accept: application/protobuf`; both are implemented using the official OTLP messages. | Replay against a real Grafana during the alpha release checklist. |
| OBI in CI | OBI needs kernel 5.8+, BTF, and privileges; standard CI runners may not qualify. | Use an OTLP generator in CI and test OBI on a dedicated VM. |
| Official PromQL parser dependency tree | Measured: 15 modules in the build, all Apache-2.0/BSD-3/MIT; vendored. | Re-check on every version bump. |
| Real ClickHouse compression and cost | Not measured | M6. |
| Minimum supported ClickHouse version | Tested 26.8.36.3; logs use text indexes from 26.2. | Expand the test matrix before beta. |
| Product differentiation versus cerberus, oteldb, ClickStack, and SigNoz | Not written | Add one sentence to README before release. |

## Out of scope for v0.1

- Multi-tenancy (cloud plan: one database per tenant; database and table names
  are configuration from the start).
- HA, replication, and managed operations.
- A custom UI.
- Agent supervisor, control plane, signed updates (TUF), per-agent mTLS, and
  multi-destination fan-out.
- Built-in auth, quotas, and billing.
- Alerting and ruler. User alerts stay in Grafana or Prometheus.
- A migration CLI and per-user query parity verification.
- Indonesian data-residency claims.

## Implementation status (2026-11-01)

M0-M5 alpha slices are implemented in the repository:

- M0 foundation, M1 data plane, and M2 query core.
- M3 PromQL head: selectors, matchers, counter functions, range aggregations,
  `histogram_quantile`, basic math, scalar/vector and one-to-one vector/vector
  operators, `offset`, and aggregations. Support list in
  `docs/promql-support.md`.
- M4 TraceQL head: single-spanset tag filters, tag search, tags/tag values, and
  trace by ID in OTLP JSON and protobuf. Support list in
  `docs/traceql-support.md`.
- M5 LogQL head: stream selectors and line filters, labels, label values, and
  series. Support list in `docs/logql-support.md`.

The beta gates remain open: the full PromQL compliance tester, the Loki
benchmark corpus, the in-house TraceQL corpus, and the three-way differential
diff. OBI has not yet been exercised on a qualifying VM. M6 hardening and M7
release packaging are still pending.
