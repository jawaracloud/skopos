# Installation and operations

> Alpha: the chart installs the data plane and the gateway health endpoints.
> The Prometheus, Loki, and Tempo query APIs are not implemented until M3–M5.
> Do not expose this installation to the internet.

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

## Install

```sh
helm install skopos ./deploy/helm/skopos \
  --namespace observability \
  --create-namespace
```

The init job waits for ClickHouse, creates the pinned schema, and creates the
read-only gateway user. If the job fails, inspect its logs before retrying:

```sh
kubectl -n observability logs job/skopos-clickhouse-init
```

Check health:

```sh
kubectl -n observability port-forward svc/skopos-gateway 8080:8080
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
```

`/readyz` returns 503 when ClickHouse is unreachable. `/healthz` only reports
that the gateway process is alive.

## Configuration

The important values are:

| Value | Default | Meaning |
| --- | --- | --- |
| `clickhouse.database` | `otel` | Database name. Validated as an SQL identifier. |
| `clickhouse.tables.*` | `otel_*` | Table names. Validated as SQL identifiers. |
| `clickhouse.ttl.tracesDays` | `7` | Trace and trace-lookup TTL in days. |
| `clickhouse.ttl.logsDays` | `7` | Log TTL in days. |
| `clickhouse.ttl.metricsDays` | `30` | Metrics TTL in days. |
| `clickhouse.persistence.size` | `50Gi` | PVC request. |
| `clickhouse.persistence.storageClass` | `""` | Empty uses the cluster default. |
| `clickhouse.limits.*` | see `values.yaml` | Gateway user limits in ClickHouse. |
| `gateway.image.digest` | `""` | Release images must set this. |
| `networkPolicy.enabled` | `true` | Component ingress NetworkPolicies. |
| `networkPolicy.gateway.allowFrom` | `[{namespaceSelector: {}}]` | Namespace/label selectors allowed to reach the gateway. Empty means no ingress. Tighten this to your Grafana namespace. |

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

The datasources will return connection or 404 errors until M3–M5 implement the
query endpoints. The example is included so the wiring can be reviewed early.

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

## Security notes

- No built-in auth. The gateway is a ClusterIP service. Do not expose it with an
  Ingress without adding authentication and TLS at the edge.
- The default gateway NetworkPolicy allows pods from any namespace to reach port
  8080. Tighten `networkPolicy.gateway.allowFrom` to your Grafana namespace.
  Setting it to an empty list denies all ingress.
- The Collector and ClickHouse are reachable only from the components that need
  them by default. The v0.1 policies restrict ingress; egress restrictions are
  part of the M6 hardening work.
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
