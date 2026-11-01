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

## Status

| Component | Status | Notes |
| --- | --- | --- |
| Helm data plane (OBI, Collector, ClickHouse) | Alpha | M1 complete; `helm lint`/`helm template` and a k3d smoke test with a generator writing traces, metrics, and logs. OBI itself was not exercised in k3d (kernel/privilege constraints); its config was validated against the v1.0.0 binary. |
| Query core (plan IR, SQL emitter, ClickHouse HTTP client) | Alpha | M2: golden SQL, fuzz, unit, and real-ClickHouse integration tests. |
| Prometheus-compatible API | Not implemented | Planned for M3. |
| Tempo-compatible API | Not implemented | Planned for M4. |
| Loki-compatible API | Not implemented | Planned for M5. |
| Grafana datasource examples | Included | They will return errors until the M3–M5 APIs land. |

The gateway currently serves `/healthz`, `/readyz`, and `/version`. It does not
yet serve query endpoints. Do not point production Grafana instances at it.

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
|                         Prometheus / Loki / Tempo APIs (M3–M5)   |
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
kubectl -n observability port-forward svc/skopos-gateway 8080:8080
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
```

The full installation, TTL, disk-full, and Grafana provisioning notes are in
[docs/install.md](docs/install.md).

## Development

Skopos is stdlib-first. M0–M2 have no non-standard-library Go dependencies.

```sh
make test                 # unit tests
make test-integration     # requires a ClickHouse HTTP endpoint
make fuzz                 # short anti-leak fuzz run
make helm-lint            # helm lint
make helm-template        # render the chart
make check                # vet + test + helm lint
```

The query-core tests live in `internal/sqlgen` (golden SQL and fuzzing),
`internal/chclient` (HTTP client and ClickHouse integration), and
`internal/httpapi` (health endpoints). The Helm chart is in `deploy/helm/skopos`.

## Security

Skopos v0.1 has no built-in authentication. The gateway and ClickHouse services
are ClusterIP-only and ship with NetworkPolicies, but anyone who can reach the
gateway can query all telemetry in the configured database. Do not expose it to
the internet. See [SECURITY.md](SECURITY.md) and [docs/install.md](docs/install.md).

## Documentation

- [ADR 0001: observability stack](docs/adr/0001-observability-stack.md)
- [Installation and operations](docs/install.md)
- [Support matrix](docs/support-matrix.md)
- [Dependency and supply-chain policy](docs/dependencies.md)
- [Contributing](CONTRIBUTING.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
