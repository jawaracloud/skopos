# Skopos documentation

This is the index for the Skopos repository. Start with the path that matches
your role; every link is relative to this `docs/` directory.

## Start here

| Document | Answers |
| --- | --- |
| [Project README](../README.md) | What Skopos is, why it exists, status, architecture, quick start |
| [Installation and operations](install.md) | Kubernetes/Helm install, configuration, auth, Grafana, upgrades |
| [Support matrix](support-matrix.md) | What is implemented and what is explicitly not supported |
| [Beta gate status](beta-gates.md) | Evidence for each quality gate and what remains open |

## AI SRE context

The context layer is the part of Skopos built specifically for AI SRE agents:
explicit catalog context, observed metadata with provenance, compact token
budgets, and MCP tools.

| Document | Answers |
| --- | --- |
| [AI context API and MCP](ai-context.md) | `/api/v1/context/*`, `known:false`, observed data, suggestions, coverage, `/metrics`, stdio/HTTP MCP |
| [OpenSSF Scorecard](openssf-scorecard.md) | Current Scorecard result and remaining maintainer actions |
| [OpenSSF Best Practices](openssf-best-practices.md) | Honest badge application draft and evidence |

## Deployment

| Document | Answers |
| --- | --- |
| [Installation and operations](install.md) | Helm chart, ClickHouse, Collector, OBI, Grafana, auth |
| [Running as systemd](systemd.md) | Single process or frontend/query split without Kubernetes |
| [Helm chart distribution](chart-repository.md) | OCI chart install, chart versioning, signature verification |
| [Tiered storage and backups](storage.md) | Hot/cold S3 storage, TTL, `BACKUP`/`RESTORE`, RustFS test |
| [Durability checkpoint](checkpoint.md) | Recent-window WAL, segment/gzip upload, loss bounds, replay, `checkpoint.*` Helm values |
| [Release process](release.md) | Tag-only image/OCI chart release, SBOM, keyless signing, attestations, immutable releases |

## Security

| Document | Answers |
| --- | --- |
| [Supply-chain controls](security/supply-chain.md) | What protects the pipeline, where each control lives, and the remaining gaps |
| [Supply-chain threat model](security/threat-model.md) | Assets, actors, trust boundaries, top threats, residual risk |
| [Agent and automation access](security/agent-access.md) | Non-admin token, verification, rotation |
| [Incident response](security/incident-response.md) | Token compromise, bad release, registry incident, recovery |

## Compatibility

| Document | Answers |
| --- | --- |
| [Support matrix](support-matrix.md) | Endpoint and signal support at a glance |
| [PromQL support](promql-support.md) | Supported PromQL surface, semantics, and known gaps |
| [TraceQL support](traceql-support.md) | Tempo endpoints, TraceQL subset, OTLP JSON/protobuf |
| [LogQL support](logql-support.md) | Loki endpoints, parser stages, metric queries, full-text pushdown |

## Operations and performance

| Document | Answers |
| --- | --- |
| [Benchmarks](benchmarks.md) | Compression, storage cost, and gateway query latency |
| [Storage](storage.md) | Retention, cold tier, backup/restore, capacity behavior |
| [Durability checkpoint](checkpoint.md) | Loss bounds, local WAL capacity, S3 uploads, replay |
| [Installation](install.md) | Limits, NetworkPolicy, TTL, disk-full behavior, troubleshooting |

## Architecture and decisions

| Document | Answers |
| --- | --- |
| [ADR 0001](adr/0001-observability-stack.md) | OBI + Collector + ClickHouse + compatible gateway, scope and decisions |
| [ADR 0002](adr/0002-distributed-read-path.md) | Distributed frontend/query split, S3 retention, full-text, no compactor |
| [Dependency policy](dependencies.md) | Go modules, pinned images, reference test containers, supply chain |

## Security and project process

| Document | Answers |
| --- | --- |
| [Security policy](../SECURITY.md) | Reporting, supported versions, security model and limitations |
| [Contributing](../CONTRIBUTING.md) | Test-first rules, review gates, dependency policy |
| [Governance](../GOVERNANCE.md) | Maintainer model and decision process |
| [Code of Conduct](../CODE_OF_CONDUCT.md) | Community expectations and reporting |

## Suggested reading paths

**Kubernetes / Helm operator**

1. [Project README](../README.md)
2. [Installation](install.md)
3. [Support matrix](support-matrix.md)
4. [Storage](storage.md)
5. [Benchmarks](benchmarks.md)

**systemd / non-Kubernetes operator**

1. [Running as systemd](systemd.md)
2. [Support matrix](support-matrix.md)
3. [Storage](storage.md)
4. [AI context API](ai-context.md) if an AI SRE agent will query it

**AI SRE agent or MCP integration**

1. [AI context API and MCP](ai-context.md)
2. [Support matrix](support-matrix.md)
3. [PromQL](promql-support.md), [LogQL](logql-support.md), and [TraceQL](traceql-support.md)

**Contributor or reviewer**

1. [Contributing](../CONTRIBUTING.md)
2. [Beta gate status](beta-gates.md)
3. [Architecture ADRs](adr/)
4. [Dependency policy](dependencies.md)

## Source layout

```text
cmd/gateway/           gateway and query-worker binary
cmd/skopos-mcp/        stdio/HTTP MCP server for AI SRE agents
cmd/skopos-checkpoint/ OTLP durability checkpoint and replay binary
internal/              query heads, SQL emitter, ClickHouse client, context, auth
internal/checkpoint/   stdlib WAL, SigV4 S3 client, gzip uploader, replay
deploy/helm/       Helm chart
deploy/systemd/    systemd units and env examples
deploy/examples/   Grafana, Kubernetes, and context catalog examples
test/              corpora, differential harnesses, e2e, benchmarks
docs/              this documentation set
```

The latest systemd and AI context docs are on the `development` branch until
they are promoted to `main` through a release.
