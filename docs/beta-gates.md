# Beta gate status

This page is the single source of truth for the ADR's beta gates. Alpha means
no compatibility guarantee; a gate is only "passed" when the evidence below is
reproducible on the pinned references.

| Gate | Status | Evidence | Remaining work |
| --- | --- | --- | --- |
| PromQL compliance tester | **Partial** | `test/compat/prometheus` runs 212 instant/range differential cases against the official Prometheus `promql` engine: 0 mismatches. | Integrate the official `prometheus/compliance/promql` suite with a target adapter that scrapes the compliance replayer into ClickHouse and starts the gateway. Filter to the published support list or expand support. |
| Loki benchmark corpus | **Partial** | 11-case corpus in `test/corpus/logql`; 0 diffs against Loki 3.5.7 and the gateway. CI runs the reference harness. | Import the full upstream Loki benchmark corpus and close the unsupported queries (unwrap, more metric functions, stages). |
| TraceQL corpus | **Partial** | 55 search cases, 5 tag scopes, 10 tag-value cases, 3 trace-by-ID cases in `test/corpus/traceql`; compared with Tempo 2.7.2 and ClickHouse. CI runs the reference harness. | Add structural operators, pipelines, metrics queries, and multiple spansets; then expand the corpus. |
| Three-way diff (reference, cerberus, gateway) | **Passed for Skopos** | `test/compat/cerberus`: 124 checks; 0 Skopos divergences; 39 cerberus v1.23.1 gaps baseline-pinned. CI runs it. | The literal three-way zero is blocked by cerberus, not Skopos. Re-run when cerberus fixes its gaps; the harness fails on any new or fixed baseline entry. |
| SQL golden + fuzz emitter | **Passed** | Golden SQL tests and 30s fuzz gate; user values never appear as literals. CI blocking. | Human security review of emitter/escaping before beta. |
| `govulncheck` + module checksums + vendor | **Passed** | CI runs govulncheck v1.9.0, `go mod verify`, and vendor drift. | Re-run on every dependency bump. |
| `helm lint` + render | **Passed** | Helm 3.22.1 and 4.3.1; CI renders default, overrides, and digest-pinned gateway. | — |
| Real OBI e2e | **Passed on host** | `make e2e-obi`: real OBI v1.0.0, kernel 6.18, BTF/tracefs, metrics + traces + Tempo search/trace/tags + v2 protobuf. | GitHub-hosted runners cannot provide privileged eBPF; keep manual or use a privileged runner/VM. |
| Compression and latency | **Measured** | `docs/benchmarks.md`: 7.96 B/metric sample, 83.92 B/log line, 24.58 B/span; p95 table. | Re-run on reference hardware and with real customer cardinality. |
| Release signing/publishing | **Scaffolded** | `scripts/release.sh`, SBOM fallback, optional cosign hooks, release workflow; dry-run passes. | Provide project signing keys/OIDC, registry credentials, and publish the alpha chart/image. |

## Root causes closed in this pass

- OBI Config v2 without an explicit sampler exports metrics but no traces. The
  chart now sets `tracer_provider.sampler.always_on`; CI guards it.
- PromQL range/lookback boundaries, counter zero-point extrapolation,
  histogram quantile edge cases, group matching, set operators, and summary
  expansion were fixed against the official Prometheus engine (80 failures
  before, 0 after).
- Tempo v2 protobuf required the `TraceByIDResponse` envelope; it is encoded
  manually without importing Tempo's AGPL proto module.
- Loki's fixed-date corpus could age out of the short-lived reference
  container; the harness keeps chunks in the ingester and CI runs the
  differential.
- LogQL parser stages and metric queries were added and checked against Loki.
- The CI workflow had a YAML/action-context error; it is fixed and now runs
  unit, race, fuzz, integration, PromQL oracle, Tempo/Loki references, Helm,
  and the cerberus three-way gate.
- The reference Prometheus/Loki/Tempo and cerberus images are pinned; no AGPL
  code is linked or vendored.
