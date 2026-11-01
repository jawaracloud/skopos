# Dependency and supply-chain policy

Skopos follows ADR 0001: Go 1.27, stdlib-first. A non-standard-library module is
allowed only when it is official from the provider, required for migration, and
recorded here before it lands.

## Current Go dependencies

M0–M2 have **no non-standard-library runtime or test dependencies**. The module
is built from the Go standard library only:

```text
github.com/mrofisr/skopos
```

`go.mod` and `go.sum` are committed. Vendoring becomes mandatory when the first
runtime dependency lands. The planned M3 exception is the official Prometheus
PromQL parser (`github.com/prometheus/prometheus/promql/parser`, Apache-2.0,
D18); it is not part of M0–M2 and still needs a dependency-tree review before
approval. LogQL and TraceQL parsers are written in-house without copying code
from Loki or Tempo.

## CI and development tools

These tools are not linked into the gateway binary:

| Tool | Version | Purpose |
| --- | --- | --- |
| Go | 1.27.x | Build and test |
| Helm | 3.19+ or 4.x | Chart lint and render |
| govulncheck | v1.9.0 | Vulnerability scan |
| Docker | any current release | ClickHouse integration tests |

## Pinned container images

The chart pins every third-party image by tag and digest. A version bump is a
separate, human-reviewed PR per the working rules.

| Component | Image | Pinned version | Digest |
| --- | --- | --- | --- |
| ClickHouse | `clickhouse/clickhouse-server` | `26.8.36.3` | `sha256:d426d2f612e1e189c90f9a3f8dcd85a03c6851273833817a296bb327e63aa7a7` |
| OTel Collector contrib | `otel/opentelemetry-collector-contrib` | `0.164.0` | `sha256:340c9ca6a36617084c6661bdc4f0fe92b7cda5a805d61673ec8db375fddc522b` |
| OBI | `otel/ebpf-instrument` | `v1.0.0` | `sha256:a68d70e934260f72540f157ccf4fb4adfa38f11de060d7d905696dad6a3fca02` |

The Skopos gateway image is built from this repository. The default chart uses a
tag for local development; release values must set
`gateway.image.digest` to the published image digest. The release pipeline owns
that digest.

## Schema pin

The ClickHouse DDL mirrors the ClickHouse exporter schema from
`open-telemetry/opentelemetry-collector-contrib` at tag `v0.164.0`, the same
version as the pinned Collector image. Columns and types match that version;
indexes and TTL are customized as permitted by the exporter's schema-management
documentation. The DDL lives in the chart ConfigMap and is executed by the init
job. The exporter is configured with `create_schema: false` so only one process
owns schema changes.

## Release security

The following are required before the alpha release (M7) and are not yet
implemented in the M0–M2 scaffold:

- SBOM generation for the gateway image and Go module.
- Image and chart signing.
- Checksum verification for downloaded release artifacts.
- A separate, reviewed dependency and image version-bump workflow.
- `govulncheck` and module checksum verification as blocking CI gates (the
  current CI runs both).
