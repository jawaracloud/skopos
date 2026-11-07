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
| `golang.org/x/net` | `v0.62.0` | BSD-3-Clause |
| `golang.org/x/sys` | `v0.49.0` | BSD-3-Clause |
| `golang.org/x/text` | `v0.43.0` | BSD-3-Clause |

`golang.org/x/net` is an indirect module required by Prometheus test
dependencies; no `x/net` package is linked into the gateway binary. It was
updated from v0.60.0 to v0.62.0 on 2026-11-07 to fix `GO-2026-6860` and
`GO-2026-6866`. The Prometheus compatibility test module
(`test/compat/prometheus`) pins the same v0.62.0 so OSV scans of both module
graphs stay clean.

LogQL and TraceQL parsers are written in-house without copying code from Loki
or Tempo (both AGPL-3.0). No AGPL dependency is linked or vendored.

The PromQL differential oracle under `test/compat/prometheus/` is a separate
test-only Go module. It imports the official Prometheus engine and storage
helpers but is not linked into the gateway binary and does not change the main
module's `vendor/` tree. Its dependencies are pinned in that module's
`go.mod`/`go.sum`.

Dependency changes require a separate, human-reviewed PR. The `vendor/` tree is
regenerated with `go mod vendor`; CI runs `go mod verify` and verifies the
vendor diff.

## CI and development tools

These tools are not linked into the gateway binary:

| Tool | Version | Purpose |
| --- | --- | --- |
| Go | `go.mod` (`1.27.3`) | Build and test; CI reads the version from `go.mod` |
| Helm | 3.19+ or 4.x | Chart lint and render |
| govulncheck | v1.9.0 | Vulnerability scan |
| Docker / Compose | any current release | ClickHouse integration tests, Loki/Tempo/cerberus harnesses, S3 + RustFS test, OBI e2e |
| actionlint | v1.7.12 | Workflow syntax checks in the required `policy` job |
| zizmor | v1.32.0 | Workflow security checks in the required `policy` job |
| go-licenses | v1.6.0 | License allowlist check in the required `policy` job |
| OpenSSF Scorecard CLI | v5.6.0 | Scorecard scan via `scripts/scorecard.sh`; checksum-verified, not linked into the gateway |
| gitleaks | v8.30.1 | Local secret scan during the OpenSSF pass; not a CI dependency |

Dependabot runs weekly with a five-day cooldown for Go modules, GitHub Actions,
and Dockerfiles/Compose files. Chart image tags in `values.yaml` are upgraded by
hand as separate, reviewed pull requests because the Dependabot Helm ecosystem
does not update image tags in values files.

## Pinned container images

The chart pins every third-party image by tag and digest. A version bump is a
separate, human-reviewed PR per the working rules.

| Component | Image | Pinned version | Digest |
| --- | --- | --- | --- |
| ClickHouse | `clickhouse/clickhouse-server` | `26.8.36.3` | `sha256:d426d2f612e1e189c90f9a3f8dcd85a03c6851273833817a296bb327e63aa7a7` |
| OTel Collector contrib | `otel/opentelemetry-collector-contrib` | `0.164.0` | `sha256:340c9ca6a36617084c6661bdc4f0fe92b7cda5a805d61673ec8db375fddc522b` |
| OBI | `otel/ebpf-instrument` | `v1.0.0` | `sha256:a68d70e934260f72540f157ccf4fb4adfa38f11de060d7d905696dad6a3fca02` |

Reference implementations are test-only containers, pinned in their harnesses:

| Component | Image | Pinned version | Digest |
| --- | --- | --- | --- |
| Tempo reference | `grafana/tempo` | `2.7.2` | pinned in `test/compat/tempo/` |
| Loki reference | `grafana/loki` | `3.5.7` | `sha256:0eaee7bf39cc83aaef46914fb58f287d4f4c4be6ec96b86c2ed55719a75e49c8` |
| cerberus reference | `ghcr.io/tsouza/cerberus` | `v1.23.1` | `sha256:e5ced00b1ee85c371295147ed0bce9caa4a5c5554443ee8aabc329d5351dc4a5` |
| Prometheus reference | `prom/prometheus` | `v3.15.1` | `sha256:fa3302279710be2f21a8301ec17a8b4f2301fd829bd113839b3b6b02b6f191a2` |
| RustFS S3 test | `rustfs/rustfs` | `1.0.4` | `sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2` |
| AWS CLI test client | `amazon/aws-cli` | `2.31.7` | `sha256:8173c3c7c71e25b0824025b90a85df6e24c696f646498dca9dd370ea446e4fc8` |
| Alpine test helper | `alpine` | `3.22` | `sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f` |

The Skopos gateway image is built from this repository. The gateway is disabled
by default so a default install never pulls a mutable or unpublished image tag.
Release installs use the attached `values.release.yaml`, which enables the
gateway and sets `gateway.image.digest` to the published image digest. The
release pipeline owns that digest.

## Schema pin

The ClickHouse DDL mirrors the ClickHouse exporter schema from
`open-telemetry/opentelemetry-collector-contrib` at tag `v0.164.0`, the same
version as the pinned Collector image. Columns and types match that version;
indexes and TTL are customized as permitted by the exporter's schema-management
documentation. The DDL lives in the chart ConfigMap and is executed by the init
job. The exporter is configured with `create_schema: false` so only one process
owns schema changes.

## Release security

The release path is implemented in `scripts/release.sh`,
`scripts/verify-release.sh`, and `.github/workflows/release.yml`:

- One annotated `vX.Y.Z` tag releases both the image and chart.
- The tag must be an ancestor of `main`; the workflow refuses to overwrite an
  existing image tag or GitHub release.
- `scripts/release.sh` builds the image, chart, SBOMs, checksums, and metadata
  only. It never pushes or signs.
- Every image and chart is signed with keyless Sigstore using the GitHub Actions
  OIDC identity and the transparency log. There are no long-lived signing keys.
- GitHub artifact attestations cover provenance and SBOMs.
- GitHub releases are created as drafts, fully populated, then published as
  immutable releases.
- `make release-check` runs vet, tests, Helm lint/render, module checksum
  verification, and vendor drift checks before publishing.
- `govulncheck`, the SQL fuzz gate, the PromQL/Loki/Tempo differentials, the
  S3/RustFS integration test, and the Helm render matrix are blocking CI gates.
- The official OpenSSF Scorecard action runs weekly, on pushes to `main`, and on
  manual dispatch. It publishes results to the public Scorecard API and uploads
  SARIF to code scanning. `make scorecard` remains the local CLI fallback.
