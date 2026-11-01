# Skopos

> **Alpha software.** Skopos is an open-source observability stack built on
> OpenTelemetry eBPF Instrumentation (OBI), OpenTelemetry Collector, and
> ClickHouse. It is not production-ready. APIs, schemas, and chart values may
> change without notice.

[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/jawaracloud/skopos/badge)](https://securityscorecards.dev/viewer/?uri=github.com/jawaracloud/skopos)

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
| Distributed read path | Alpha | Optional public frontend plus stateless query workers (`distributed.enabled=true`); the frontend has no ClickHouse credentials. |
| Basic authentication | Optional | `auth.enabled=true` protects query/version routes with one operator-managed credential; `/healthz` and `/readyz` stay open for probes. Use TLS. |
| systemd deployment | Alpha | Unit files for single-process and frontend/query split, local schema renderer, and a full operations guide in [docs/systemd.md](docs/systemd.md). |
| AI SRE context API | Alpha | Optional, compact `/api/v1/context/*` catalog for metrics, services, logs, and traces, plus a stdio MCP server. Detail defaults to `minimal`; unknown signals return `known:false`. See [docs/ai-context.md](docs/ai-context.md). |
| S3 retention and backups | Alpha | Optional ClickHouse tiered storage (hot -> S3 cold, still queryable) and a separate scheduled `BACKUP DATABASE` CronJob. Tested against RustFS 1.0.4. See [docs/storage.md](docs/storage.md). |
| LogQL full-text | Alpha | Stream selectors and body line filters are pushed into ClickHouse; `match(Body, ...)` uses the `idx_body` text index. Go remains the final evaluator. |
| OpenSSF | Measured, public | Official Scorecard action reports **7.8** and publishes the badge. Best Practices evidence is ready; a maintainer must submit it at bestpractices.dev. See [docs/openssf-scorecard.md](docs/openssf-scorecard.md). |
| Release tooling | Partial | Chart repository is live and the chart release is keyless-signed; image publishing, SBOM attachment, and provenance are still scaffolded. See [docs/release.md](docs/release.md). |

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
|              hot parts -> optional S3 cold tier                  |
|                                       ^                          |
|                                       | HTTP, read-only user     |
|                         Skopos query workers (Go, internal)      |
|                         Prometheus / Loki / Tempo APIs (alpha)    |
|                                       ^                          |
|                                       |                          |
|                         Skopos frontend (public, no CH creds)    |
|                                       ^                          |
|                                       |                          |
|                         Grafana (user-managed)                   |
|                                                                  |
|  optional CronJob -> BACKUP DATABASE -> separate S3 prefix        |
+------------------------------------------------------------------+
```

OBI emits metrics and traces. Logs can arrive over OTLP through the Collector,
or through a separate log pipeline (for example a filelog receiver); a filelog
receiver must be verified before it is enabled. The gateway never stores or
accepts telemetry. It only reads from ClickHouse.

## Deployment modes

- **Single process (default).** `gateway.mode=all` keeps the v0.1 shape: one
  Deployment serves the three APIs and talks to ClickHouse.
- **Distributed read path.** `distributed.enabled=true` renders a public
  frontend and an internal query tier. The frontend proxies to
  `SKOPOS_QUERY_URLS` and reports ready only when a query backend is ready. The
  public Service keeps the name `skopos-gateway`.
- **S3 retention.** Tiered storage moves old ClickHouse parts to an
  S3-compatible cold volume; a separate backup CronJob writes to a different
  prefix. Backups are for restore, not for serving queries. ClickHouse owns
  merges/compaction; there is no Skopos compactor.

The detailed decisions are in [ADR 0002](docs/adr/0002-distributed-read-path.md).

## Quick start

Prerequisites:

- Kubernetes 1.27 or newer.
- A Linux node kernel that OBI supports. OBI needs kernel 5.8+ with BTF, or a
  RHEL-family 4.18+ kernel with the eBPF backports. See
  [OBI's support matrix](https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation/blob/v1.0.0/SUPPORT_MATRIX.md).
- Helm 3.19+ or Helm 4.
- A default StorageClass, or set `clickhouse.persistence.storageClass`.

The gateway image is not published yet. For a data-plane-only install from the
local chart:

```sh
helm install skopos ./deploy/helm/skopos \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

Or install from the published Helm chart repository:

```sh
helm repo add skopos https://jawaracloud.github.io/skopos/
helm repo update
helm install skopos skopos/skopos \
  --devel \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

See [docs/chart-repository.md](docs/chart-repository.md) for chart versioning and
the release flow.

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

For the distributed read path:

```sh
helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set distributed.enabled=true \
  --set frontend.replicaCount=2 \
  --set query.replicaCount=2
```

For tiered S3 retention and scheduled backups, see
[docs/storage.md](docs/storage.md). The local integration test uses RustFS:

```sh
./test/integration/s3/run.sh
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
make build                # build the gateway
make mcp                  # build the stdio MCP server
make test                 # unit tests
make test-integration     # requires a ClickHouse HTTP endpoint
make fuzz                 # short anti-leak fuzz run
make helm-lint            # helm lint
make helm-template        # render the chart
make check                # vet + test + fuzz + helm lint
make e2e-obi              # real privileged OBI e2e (Docker, BTF, tracefs)
make compat-cerberus      # three-way PromQL/LogQL/TraceQL differential
make storage-test         # ClickHouse + RustFS tiered storage/backup test
make bench                # synthetic compression + latency benchmark
make scorecard            # OpenSSF Scorecard CLI (requires a GitHub token)
make release-check        # vet/test/Helm/release script syntax
```

The query-core tests live in `internal/sqlgen` (golden SQL and fuzzing),
`internal/chclient` (HTTP client and ClickHouse integration), and
`internal/httpapi` (health endpoints). The language heads have unit and
integration tests in `internal/prom`, `internal/tempo`, and `internal/loki`.
Reference harnesses live in `test/compat/` and corpora in `test/corpus/`.
The Helm chart is in `deploy/helm/skopos`.

## Security

Skopos has optional HTTP Basic authentication on the public API. `/healthz`
and `/readyz` stay open for Kubernetes probes; all query and version routes are
protected when `auth.enabled=true`. Basic auth is only safe over TLS, and it is
one shared credential with no per-user isolation. The gateway and ClickHouse
services are ClusterIP-only and ship with NetworkPolicies. Do not expose the
gateway to the internet without TLS and an authenticating edge. See
[SECURITY.md](SECURITY.md) and [docs/install.md](docs/install.md).

## Documentation

- [ADR 0001: observability stack](docs/adr/0001-observability-stack.md)
- [ADR 0002: distributed read path](docs/adr/0002-distributed-read-path.md)
- [Installation and operations](docs/install.md)
- [Running the gateway with systemd](docs/systemd.md)
- [AI SRE context API](docs/ai-context.md)
- [Tiered storage and backups](docs/storage.md)
- [Helm chart repository](docs/chart-repository.md)
- [Support matrix](docs/support-matrix.md)
- [Beta gate status](docs/beta-gates.md)
- [PromQL support](docs/promql-support.md)
- [TraceQL support](docs/traceql-support.md)
- [LogQL support](docs/logql-support.md)
- [Benchmarks](docs/benchmarks.md)
- [OpenSSF Scorecard](docs/openssf-scorecard.md)
- [OpenSSF Best Practices](docs/openssf-best-practices.md)
- [Release runbook](docs/release.md)
- [Dependency and supply-chain policy](docs/dependencies.md)
- [Contributing](CONTRIBUTING.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
