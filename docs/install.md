# Installation and operations

> Alpha: the chart installs the data plane and the alpha Prometheus, Tempo,
> and Loki query APIs. Unsupported functions return explicit errors; see the
> per-language support lists. Do not expose this installation to the internet.

## Prerequisites

- Kubernetes 1.27+.
- Helm 3.19+ or Helm 4.
- A default StorageClass, or set `clickhouse.persistence.storageClass`.
- Linux nodes that OBI supports:
  - kernel 5.8+ with BTF, or
  - RHEL-family kernel 4.18+ with the eBPF backports.
- `hostPID` and the OBI host mounts (`/sys/fs/cgroup`, `/sys/kernel/tracing`)
  must be allowed by your pod security policy. OBI is a node-level agent.
- The default chart grants OBI `BPF`, `SYS_PTRACE`, `NET_RAW`,
  `CHECKPOINT_RESTORE`, `DAC_READ_SEARCH`, `PERFMON`, and `SYS_RESOURCE`. It does
  not grant `NET_ADMIN` or `SYS_ADMIN`. See `values.yaml` and OBI's security
  documentation before enabling trace-context propagation or network
  observability.
- Optional tiered storage and backups need S3-compatible endpoints, existing
  buckets, and credentials. See [storage.md](storage.md).

## Install the data plane

The chart is published at `https://jawaracloud.github.io/skopos/`. The gateway
image is not published yet, so install the data plane first:

```sh
helm repo add skopos https://jawaracloud.github.io/skopos/
helm repo update
helm install skopos skopos/skopos \
  --devel \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

Or install from a local checkout:

```sh
helm install skopos ./deploy/helm/skopos \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

The init job waits for ClickHouse, creates the pinned schema, and creates the
read-only gateway user. If the job fails, inspect its logs before retrying:

```sh
kubectl -n observability logs job/skopos-clickhouse-init
kubectl -n observability get pods
```

## Build and enable the gateway

```sh
docker build -t skopos-gateway:0.1.0-alpha.1 .
# k3d:
k3d image import -c <cluster> skopos-gateway:0.1.0-alpha.1
# kind:
# kind load docker-image skopos-gateway:0.1.0-alpha.1

helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set gateway.enabled=true \
  --set gateway.image.repository=skopos-gateway \
  --set gateway.image.tag=0.1.0-alpha.1 \
  --set gateway.image.digest= \
  --set gateway.image.pullPolicy=IfNotPresent
```

For a real cluster, push the image to a registry and set the repository, tag,
and digest. Release values must set `gateway.image.digest`.

Check health:

```sh
kubectl -n observability port-forward svc/skopos-gateway 8080:8080
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
```

`/readyz` returns 503 when ClickHouse is unreachable. `/healthz` only reports
that the gateway process is alive.

## Distributed read path (optional)

`distributed.enabled=true` splits the read path:

- `skopos-frontend` proxies Prometheus/Loki/Tempo requests to
  `SKOPOS_QUERY_URLS`; it has no ClickHouse credentials.
- `skopos-query` runs the three query heads against ClickHouse.
- The public Service keeps the name `skopos-gateway` and selects frontend pods.
- `/readyz` on the frontend returns 503 unless at least one query backend is
  ready.
- NetworkPolicy allows Grafana/ingress to the frontend, frontend to query, and
  query to ClickHouse.

```sh
helm upgrade --install skopos ./deploy/helm/skopos \
  --namespace observability \
  --set distributed.enabled=true \
  --set frontend.replicaCount=2 \
  --set query.replicaCount=2
```

The default (`distributed.enabled=false`, `gateway.mode=all`) remains the
single-binary deployment. `SKOPOS_MODE=worker` is an explicit alias for
`SKOPOS_MODE=query`; both register the three query heads. ClickHouse remains
the backend and owns merges/compaction; this is not a ClickHouse HA topology.

## Basic authentication

Authentication is optional and disabled by default. When enabled, all query
routes and `/version` require HTTP Basic auth; `/healthz` and `/readyz` are
exempt so Kubernetes probes continue to work.

Preferred: create a Secret with `username` and `password`, then reference it:

```sh
kubectl -n observability create secret generic skopos-auth \
  --from-literal=username=viewer \
  --from-literal=password='change-me'

helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set auth.enabled=true \
  --set auth.existingSecret=skopos-auth
```

For a local test only, inline values are also supported:

```sh
helm upgrade skopos ./deploy/helm/skopos --namespace observability \
  --set auth.enabled=true \
  --set auth.username=viewer \
  --set auth.password='change-me'
```

For a non-Helm deployment, set `SKOPOS_AUTH_ENABLED=true`,
`SKOPOS_AUTH_USERNAME`, and `SKOPOS_AUTH_PASSWORD` directly.

Configure Grafana's Prometheus, Loki, and Tempo datasources with `basicAuth:
true`, `basicAuthUser`, and the password in `secureJsonData`. Basic auth sends
reusable credentials with every request: use TLS at the ingress and do not
expose the gateway over plain HTTP.

## Configuration

The important values are:

| Value | Default | Meaning |
| --- | --- | --- |
| `clickhouse.database` | `otel` | Database name. Validated as an SQL identifier. |
| `clickhouse.tables.*` | `otel_*` | Table names used by the DDL, Collector, and gateway. Validated as SQL identifiers. The gateway parses them now; the M3–M5 query heads consume them. |
| `clickhouse.ttl.tracesDays` | `7` | Trace and trace-lookup TTL in days. |
| `clickhouse.ttl.logsDays` | `7` | Log TTL in days. |
| `clickhouse.ttl.metricsDays` | `30` | Metrics TTL in days. |
| `clickhouse.persistence.size` | `50Gi` | PVC request. |
| `clickhouse.persistence.storageClass` | `""` | Empty uses the cluster default. |
| `clickhouse.limits.*` | see `values.yaml` | Gateway user limits in ClickHouse, also used by the gateway client. |
| `clickhouse.ttl.*` | see `values.yaml` | Table TTLs. The chart keeps ClickHouse and the init client in UTC. Ignored for signal tables when tiered storage is enabled. |
| `clickhouse.storage.enabled` | `false` | Optional tiered storage: hot local PVC plus S3-compatible cold volume. Requires a bucket and credentials. See [storage.md](storage.md). |
| `clickhouse.storage.hotDays`, `totalDays` | `3`, `30` | Move parts to cold after `hotDays`; delete after `totalDays`. These replace `clickhouse.ttl.*` when storage is enabled. |
| `clickhouse.storage.cold.*` | empty | S3 endpoint, bucket, prefix, and credentials (or `existingSecret`) for the live cold tier. |
| `clickhouse.storage.cacheSize` | `10Gi` | Local read cache for cold parts, on the ClickHouse PVC. |
| `clickhouse.backup.enabled` | `false` | Optional CronJob that runs `BACKUP DATABASE` to a separate S3 destination. See [storage.md](storage.md). |
| `clickhouse.backup.schedule` | `0 2 * * *` | Cron schedule for the full backup. |
| `clickhouse.backup.*` | empty | Backup S3 endpoint, bucket, prefix, and credentials. The destination must not overlap the live cold tier. |
| `auth.enabled` | `false` | Enable HTTP Basic auth on query and version routes. `/healthz` and `/readyz` stay open. |
| `auth.existingSecret` | `""` | Secret with `username` and `password` keys. Preferred over inline values. |
| `auth.username`, `auth.password` | `""` | Inline credentials rendered into a Secret. Use only when `existingSecret` is empty. |
| `gateway.mode` | `all` | `all`, `query`/`worker`, or `frontend` for a single Deployment. |
| `distributed.enabled` | `false` | Split into a public frontend and an internal query tier. |
| `frontend.replicaCount` | `1` | Public proxy replicas when distributed. |
| `frontend.config.queryTimeout` | `30s` | Per-request proxy timeout. |
| `frontend.config.readinessTimeout` | `1s` | Frontend probe timeout for query `/readyz`. |
| `query.replicaCount` | `1` | Internal query worker replicas when distributed. |
| `gateway.image.digest` | `""` | Release images must set this. |
| `networkPolicy.enabled` | `true` | Component ingress NetworkPolicies. |
| `networkPolicy.gateway.allowFrom` | `[{namespaceSelector: {}}]` | Namespace/label selectors allowed to reach the gateway. Empty means no ingress. Tighten this to your Grafana namespace. |
| `networkPolicy.collector.allowFrom` | `[{namespaceSelector: {}}]` | Namespace/label selectors allowed to send OTLP to the Collector. Tighten this to the namespaces that emit telemetry. |

The chart generates ClickHouse admin and gateway passwords into a Secret. It
reuses the existing Secret on upgrade through `lookup`; do not delete it while
the database still has users created from it. To supply your own credentials,
set `clickhouse.auth.existingSecret` and provide the keys `admin-password` and
`gateway-password`.

## Grafana datasources

The example provisioning file is
`deploy/examples/grafana/provisioning/datasources/skopos.yaml`. It points all
three datasources at the gateway service. Replace the namespace and service
name if you changed them, then mount the file into Grafana's provisioning
directory.

The datasources use the alpha query heads. Supported queries are listed in
[promql-support.md](promql-support.md), [traceql-support.md](traceql-support.md),
and [logql-support.md](logql-support.md). Unsupported functions return an
explicit error, so a failing panel is usually a query outside the alpha list,
not a connection problem.

## Ingress example

`deploy/examples/kubernetes/ingress.example.yaml` is a starting point for an
ingress that points at the gateway Service. It is not part of the chart and it
is intentionally not secure by itself: terminate TLS at the ingress and enable
`auth.enabled=true` (or an authenticating proxy), add rate limiting, and
tighten `networkPolicy.gateway.allowFrom` to the ingress controller namespace.

## TTL, PVC, and disk-full behavior

- TTL is enforced by ClickHouse at the table level. The init job writes the TTL
  from the chart values. Changing TTL on an existing install requires a schema
  migration; the alpha chart's `CREATE TABLE IF NOT EXISTS` does not modify
  existing tables.
- The PVC is a single `ReadWriteOnce` volume. ClickHouse single-node does not
  replicate. A lost PVC loses telemetry.
- When the disk fills, ClickHouse rejects inserts with errors such as
  `NOT_ENOUGH_SPACE` or `TOO_MANY_PARTS`. The Collector retries according to
  `retry_on_failure` and its sending queue, then drops data when the queue is
  full. The gateway remains readable but queries may fail. Increase the PVC (if
  the StorageClass supports expansion), lower TTL, or add storage; do not delete
  the ClickHouse data directory while the pod is running.
- The chart does not set a Prometheus alert for disk usage. Use your cluster's
  PVC alerts.
- Tiered storage is optional and off by default. When enabled, cold parts live
  in S3 and remain queryable, while TTL still deletes them at `totalDays`.
  Backups are separate: they are for disaster recovery and must be restored
  before the gateway can query them. See [storage.md](storage.md) for values,
  existing-install `ALTER` steps, and restore commands.

## Security notes

- No built-in auth. The gateway is a ClusterIP service. Do not expose it with an
  Ingress without adding authentication and TLS at the edge.
- The default gateway NetworkPolicy allows pods from any namespace to reach port
  8080. Tighten `networkPolicy.gateway.allowFrom` to your Grafana namespace.
  Setting it to an empty list denies all ingress.
- The Collector accepts OTLP from any namespace by default so applications and
  log forwarders can send telemetry. Tighten
  `networkPolicy.collector.allowFrom` to the emitting namespaces. ClickHouse is
  reachable only from the Collector, gateway, and init job. The v0.1 policies
  restrict ingress; egress restrictions are part of the M6 hardening work.
- OBI mounts host paths and needs `hostPID`. Treat it as a privileged agent.

## Upgrade and uninstall

```sh
helm upgrade skopos ./deploy/helm/skopos -n observability
helm uninstall skopos -n observability
```

The ClickHouse PVC is not deleted by `helm uninstall`. Delete it explicitly only
after confirming you no longer need the data:

```sh
kubectl -n observability delete pvc data-skopos-clickhouse-0
```
