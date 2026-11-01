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
| Distributed read path | **Passed for alpha** | `SKOPOS_MODE=all|query|worker|frontend`; unit and proxy smoke tests; Helm renders default, distributed, and combined storage; CI renders the distributed shape. | Add a multi-replica load test and a real ClickHouse frontend/query e2e. Basic auth is optional; per-user authorization is not implemented. |
| S3 tiered retention and backup | **Passed on host** | ClickHouse 26.8 + RustFS 1.0.4: cold part queryable, `idx_body` EXPLAIN, 52 backup objects, restore to a new database returned the same rows. `test/integration/s3/run.sh`; CI runs it. | Re-run with real cloud S3 and a scheduled restore drill. The chart does not create buckets. |
| AI SRE context API | **Passed for metrics/services/logs/traces** | Strict catalog validation, `minimal`/`standard`/`full` projections, `invalid detail/include/limit` rejection, `known:false` behavior, ETag/304, response-size tests, coverage/discovery, and MCP stdio tests. CI renders the catalog ConfigMap. | Add observed-metadata provenance and remote MCP transport. |
| LogQL full-text pushdown | **Passed for supported subset** | `internal/loki` pushes selector and line-filter predicates; 67 log + 4 metric + 12 metadata integration cases are byte-identical to the unfiltered path. `match(Body, ...)` uses `idx_body`; `position` is exact but not index-assisted. | Add more regex/token cases and measure real-data index selectivity. `|=` is not token search. |
| OpenSSF Scorecard | **Measured, public** | Official `ossf/scorecard-action` reports **7.8** on public `main` (run `41286275750`) and publishes the badge. Branch-Protection reads `-1` with `GITHUB_TOKEN`; add the optional `SCORECARD_TOKEN` fine-grained PAT with Administration read to score it. | Code-Review still needs approved PRs; add the optional `SCORECARD_TOKEN`; keep CodeQL clean; fix the `golang.org/x/net` OSV entries; publish the gateway image and SBOM/provenance for Packaging; Signed-Releases is now 8 for the chart release. |
| OpenSSF Best Practices | **Application ready; submit manually** | `docs/openssf-best-practices.md` covers all 67 passing-level criteria honestly: 43 met/caveat, 14 N/A, 3 not met, 3 not demonstrated, 2 human, 2 partial. `repo_public` is now met. | A maintainer must submit at bestpractices.dev and answer the human-attestation criteria. Release, version tags, and release notes are still not met; CodeQL is enabled. |
| SQL golden + fuzz emitter | **Passed** | Golden SQL tests and 30s fuzz gate; user values never appear as literals. CI blocking. | Human security review of emitter/escaping before beta. |
| `govulncheck` + module checksums + vendor | **Passed** | CI runs govulncheck v1.9.0, `go mod verify`, and vendor drift. | Re-run on every dependency bump. |
| `helm lint` + render | **Passed** | Helm 3.22.1 and 4.3.1; CI renders default, overrides, digest-pinned gateway, distributed frontend/query, and storage/backup shapes. | — |
| Real OBI e2e | **Passed on host** | `make e2e-obi`: real OBI v1.0.0, kernel 6.18, BTF/tracefs, metrics + traces + Tempo search/trace/tags + v2 protobuf. | GitHub-hosted runners cannot provide privileged eBPF; keep manual or use a privileged runner/VM. |
| Compression and latency | **Measured** | `docs/benchmarks.md`: 7.96 B/metric sample, 83.92 B/log line, 24.58 B/span; p95 table. | Re-run on reference hardware and with real customer cardinality. |
| Release signing/publishing | **Partial** | The Helm chart is published at `https://jawaracloud.github.io/skopos/` and `skopos-0.1.0-alpha.1.tgz` is keyless-signed with a `.sig`/`.pem` pair (Scorecard Signed-Releases 8). The image release/SBOM/provenance is still scaffolded. | Publish the gateway image and SBOM/provenance; keep chart signing in the release workflow. |

## Signals and storage evidence

- The real OBI e2e still passes on this host; it is manual because GitHub-hosted
  runners are not a documented privileged-eBPF platform.
- The distributed frontend is a stateless reverse proxy. It does not connect to
  ClickHouse. Optional Basic auth is enforced at the public surface.
- Tiered storage moves ClickHouse parts to S3; ClickHouse still performs
  merges. A backup is a separate CronJob and is not queried by the gateway.
- Full-text pushdown is a performance optimization with Go as the final
  evaluator; it is not a new query language feature.

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
- The schema gained a case-sensitive `idx_body` text index and optional tiered
  TTLs. The cerberus and OBI schema renderers were updated to handle the new
  Helm conditionals; a renderer regression caused the three-way seed to fail
  until both were fixed.
- The repository became public on 2026-11-01, so the official Scorecard action
  now runs with published results and SARIF. The CLI remains the local fallback;
  the optional `SCORECARD_TOKEN` is only needed to score Branch-Protection.
