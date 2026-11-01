# Skopos

> **Alpha software.** Skopos is an open-source observability stack built on
> OpenTelemetry eBPF Instrumentation (OBI), OpenTelemetry Collector, and
> ClickHouse. It is not production-ready. APIs, schemas, and chart values may
> change without notice.

Skopos keeps the Grafana workflows you already have. It stores metrics, traces,
and logs in ClickHouse and exposes Prometheus, Loki, and Tempo compatible query
APIs through a small Go query gateway, so existing Grafana dashboards can keep
their datasource types.

The name comes from the Greek word for "watcher" or "target". It follows the
CNCF tradition of naming observability projects after watchers and signals
(Prometheus, Jaeger, Loki, Tempo).

## Why Skopos

Skopos is Apache-2.0, covers metrics, traces, and logs together, and keeps
Grafana's native Prometheus, Loki, and Tempo datasources instead of requiring a
new UI. It is built on OBI, the OpenTelemetry Collector, and ClickHouse, with no
AGPL dependency and no per-agent vendor control plane. Unlike a PromQL-only
gateway, it includes a Tempo- and Loki-compatible surface; unlike a
ClickHouse-native observability UI, the dashboard layer remains the user's
existing Grafana.

## Status

| Component | Status | Notes |
| --- | --- | --- |
| Helm data plane (OBI, Collector, ClickHouse) | Alpha | M1 complete; `helm lint`/`helm template`, a k3d generator smoke test, and a real OBI v1.0.0 eBPF e2e on kernel 6.18 (`make e2e-obi`, manual). |
| Query core (plan IR, SQL emitter, ClickHouse HTTP client) | Alpha | M2: golden SQL, fuzz, unit, and real-ClickHouse integration tests. |
| Prometheus-compatible API | Alpha | M3: query/query_range, series, labels, metadata, summary metrics, set operators, group matching, and a 212-case differential oracle against the official Prometheus engine. See [docs/promql-support.md](docs/promql-support.md). |
| Tempo-compatible API | Alpha | M4: search with spanSets, trace by ID (JSON/protobuf), tags/scopes, trace/span intrinsics, and a 55-case corpus compared with Tempo 2.7.2. See [docs/traceql-support.md](docs/traceql-support.md). |
| Loki-compatible API | Alpha | M5: log queries, JSON/logfmt parser stages, label filters, `count_over_time`/`rate`, labels/series, and an 11-case corpus compared with Loki 3.5.7. See [docs/logql-support.md](docs/logql-support.md). |
| Grafana datasource examples | Alpha-ready | All three datasource types point at the gateway. |
| Benchmarks | Measured | Compression and p95 latency in [docs/benchmarks.md](docs/benchmarks.md). |
| Three-way differential | Passed for Skopos | 124 checks vs Prometheus/Loki/Tempo and cerberus v1.23.1: 0 Skopos divergences, 39 cerberus-only gaps baseline-pinned. |
| Release tooling | Scaffolded | SBOM fallback, signed-image hooks, chart package, and release workflow in [docs/release.md](docs/release.md). |

The gateway serves `/healthz`, `/readyz`, `/version`, and the alpha PromQL,
TraceQL, and LogQL endpoints. Unsupported functions return an explicit error.
The language support lists are narrow; do not point production Grafana
instances at an alpha deployment.

## Architecture

```text
Kubernetes cluster
+------------------------------------------------------------------+
|  applications (no code changes)                                  |
|        ^ eBPF                                                    |
|  OBI (DaemonSet) ---------OTLP--------+                          |
|                                       v                          |
|                         OTel Collector (contrib)                 |
|                         | clickhouse exporter                    |
|                         v                                        |
|              ClickHouse single-node (StatefulSet + PVC)          |
|              otel_traces, otel_logs, otel_metrics_*              |
|                                       ^                          |
|                                       | HTTP, read-only user     |
|                         Skopos gateway (Go, ClusterIP)           |
|                         Prometheus / Loki / Tempo APIs (alpha)    |
|                                       ^                          |
|                                       |                          |
|                         Grafana (user-managed)                   |
+------------------------------------------------------------------+
```

OBI emits metrics and traces. Logs can arrive over OTLP through the Collector,
or through a separate log pipeline (for example a filelog receiver); a filelog
receiver must be verified before it is enabled. The gateway never stores or
accepts telemetry. It only reads from ClickHouse.

## Quick start

Prerequisites:

- Kubernetes 1.27 or newer.
- A Linux node kernel that OBI supports. OBI needs kernel 5.8+ with BTF, or a
  RHEL-family 4.18+ kernel with the eBPF backports. See
  [OBI's support matrix](https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation/blob/v1.0.0/SUPPORT_MATRIX.md).
- Helm 3.19+ or Helm 4.
- A default StorageClass, or set `clickhouse.persistence.storageClass`.

The gateway image is not published yet. For a data-plane-only install:

```sh
helm install skopos ./deploy/helm/skopos \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

To build and run the gateway locally:

```sh
docker build -t skopos-gateway:0.1.0-alpha.1 .
# For k3d: k3d image import -c <cluster> skopos-gateway:0.1.0-alpha.1
helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set gateway.enabled=true \
  --set gateway.image.repository=skopos-gateway \
  --set gateway.image.tag=0.1.0-alpha.1 \
  --set gateway.image.digest= \
  --set gateway.image.pullPolicy=IfNotPresent
```

Check the data plane:

```sh
kubectl -n observability get pods
kubectl -n observability get pvc
```

After enabling the gateway, check its health and query the three heads:

```sh
kubectl -n observability port-forward svc/skopos-gateway 8080:8080
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz

curl -s -G http://127.0.0.1:8080/api/v1/query \
  --data-urlencode 'query=up'
curl -s 'http://127.0.0.1:8080/api/search?q=%7B%7D'
curl -s 'http://127.0.0.1:8080/loki/api/v1/labels'
```

The full installation, TTL, disk-full, and Grafana provisioning notes are in
[docs/install.md](docs/install.md).

## Development

Skopos is stdlib-first. M0–M2 had no non-standard-library Go dependencies.
M3 adds the official Prometheus PromQL parser and the official OTLP protobuf
types for Tempo responses; both are recorded in [docs/dependencies.md](docs/dependencies.md).

```sh
make test                 # unit tests
make test-integration     # requires a ClickHouse HTTP endpoint
make fuzz                 # short anti-leak fuzz run
make helm-lint            # helm lint
make helm-template        # render the chart
make check                # vet + test + fuzz + helm lint
make e2e-obi              # real privileged OBI e2e (Docker, BTF, tracefs)
make compat-cerberus      # three-way PromQL/LogQL/TraceQL differential
make bench                # synthetic compression + latency benchmark
make release-check        # vet/test/Helm/release script syntax
```

The query-core tests live in `internal/sqlgen` (golden SQL and fuzzing),
`internal/chclient` (HTTP client and ClickHouse integration), and
`internal/httpapi` (health endpoints). The language heads have unit and
integration tests in `internal/prom`, `internal/tempo`, and `internal/loki`.
Reference harnesses live in `test/compat/` and corpora in `test/corpus/`.
The Helm chart is in `deploy/helm/skopos`.

## Security

Skopos v0.1 has no built-in authentication. The gateway and ClickHouse services
are ClusterIP-only and ship with NetworkPolicies, but anyone who can reach the
gateway can query all telemetry in the configured database. Do not expose it to
the internet. See [SECURITY.md](SECURITY.md) and [docs/install.md](docs/install.md).

## Documentation

- [ADR 0001: observability stack](docs/adr/0001-observability-stack.md)
- [Installation and operations](docs/install.md)
- [Support matrix](docs/support-matrix.md)
- [PromQL support](docs/promql-support.md)
- [TraceQL support](docs/traceql-support.md)
- [LogQL support](docs/logql-support.md)
- [Benchmarks](docs/benchmarks.md)
- [Release runbook](docs/release.md)
- [Dependency and supply-chain policy](docs/dependencies.md)
- [Contributing](CONTRIBUTING.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
