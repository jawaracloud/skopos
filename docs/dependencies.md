# Dependency and supply-chain policy

Skopos follows ADR 0001: Go 1.27, stdlib-first. A non-standard-library module is
allowed only when it is official from the provider, required for migration, and
recorded here before it lands.

## Current Go dependencies

M0-M2 used only the Go standard library. M3-M5 add two provider-official
dependencies and the official protobuf runtime, plus their permissive
transitive modules.
`go.mod`, `go.sum`, and `vendor/` are committed. Builds use the vendored tree.

| Module | Version | License | Owner | Why |
| --- | --- | --- | --- | --- |
| `github.com/prometheus/prometheus` | `v0.315.1` | Apache-2.0 | Prometheus Authors | Official PromQL parser (`promql/parser`), the D18 exception. |
| `go.opentelemetry.io/proto/otlp` | `v1.11.1` | Apache-2.0 | OpenTelemetry Authors | OTLP trace messages for Tempo JSON/protobuf responses. |
| `google.golang.org/protobuf` | `v1.36.12` | BSD-3-Clause | Google | Protobuf runtime for OTLP messages. |

Transitive modules (all permissive: Apache-2.0, BSD-3-Clause, or MIT):

| Module | Version | License |
| --- | --- | --- |
| `github.com/beorn7/perks` | `v1.0.1` | MIT |
| `github.com/cespare/xxhash/v2` | `v2.3.0` | MIT |
| `github.com/dennwc/varint` | `v1.0.0` | MIT |
| `github.com/grafana/regexp` | `v0.0.0-20250905093917-f7b3be9d1853` | BSD-3-Clause |
| `github.com/munnerz/goautoneg` | `v0.0.0-20191010083416-a7dc8b61c822` | BSD-3-Clause |
| `github.com/prometheus/client_golang` | `v1.24.1` | Apache-2.0 |
| `github.com/prometheus/client_model` | `v0.6.3` | Apache-2.0 |
| `github.com/prometheus/common` | `v0.71.0` | Apache-2.0 |
| `github.com/prometheus/procfs` | `v0.21.1` | Apache-2.0 |
| `go.uber.org/atomic` | `v1.11.0` | MIT |
| `golang.org/x/sys` | `v0.48.0` | BSD-3-Clause |
| `golang.org/x/text` | `v0.42.0` | BSD-3-Clause |

LogQL and TraceQL parsers are written in-house without copying code from Loki
or Tempo (both AGPL-3.0). No AGPL dependency is linked or vendored.

Dependency changes require a separate, human-reviewed PR. The `vendor/` tree is
regenerated with `go mod vendor`; CI runs `go mod verify`.

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
implemented in the M0–M5 scaffold:

- SBOM generation for the gateway image and Go module.
- Image and chart signing.
- Checksum verification for downloaded release artifacts.
- A separate, reviewed dependency and image version-bump workflow.
- `govulncheck` and module checksum verification as blocking CI gates (the
  current CI runs both).
