# OpenSSF Best Practices Badge: application draft

This is a complete, honest draft for the
[OpenSSF Best Practices Badge](https://www.bestpractices.dev/) at the **passing**
level. It is not a submitted application, and it is not a claim that the badge
has been awarded.

## Status: ready to submit with documented exceptions

The repository is **public** at `github.com/jawaracloud/skopos` (visibility
changed on 2026-11-01). The passing criterion `repo_public` is now met, and the
public issue tracker, security advisory form, and commit history are
accessible to an assessor. The badge has **not** been submitted or awarded; a
maintainer still has to submit this draft at <https://www.bestpractices.dev/>.
The solo-maintainer review exception below must be answered honestly rather
than claimed as full compliance.

Other known gaps that are **not** hidden by this draft:

- No image release under the current supply-chain process has been published.
  The legacy `skopos-0.1.0-alpha.1` chart release exists, but it predates the
  tag-only, keyless-signed, immutable release process. Release notes, unique
  version identifiers for the image, and signed-image evidence are therefore
  still pending until `v0.1.0-alpha.2` is published.
- The passing-level review expectation is **not fully met**. The project has
  one maintainer, so the `main-protection` ruleset requires a pull request and
  green checks but **zero approving reviews**. This is an accepted exception,
  not a claim of two-person review. Residual risk: no independent human review,
  and an admin account can edit the ruleset. See
  [openssf-scorecard.md](openssf-scorecard.md#review-exception-one-maintainer).
- There is no independent human security review yet. This is a Gold-level
  criterion and a documented beta gate, not something this draft claims.
- The full Prometheus compliance tester and the full Loki benchmark corpus are
  not integrated. The project has a 212-case Prometheus differential oracle and
  an 11-case Loki corpus, documented honestly in `docs/beta-gates.md`.

Resolved in the 2026-11-07 dependency pass: `golang.org/x/net` is v0.62.0 in
both the root module and `test/compat/prometheus`. The two OSV entries
`GO-2026-6860` and `GO-2026-6866` are fixed, the OSV API returns no findings
for v0.62.0, and `govulncheck v1.9.0 ./...` reports no vulnerabilities.

## Status legend

- **Met** — evidence exists and is verifiable in the repository or CI.
- **Met (...)** — the core evidence exists with the limitation named in the
  evidence column. For example, `Met` means an external
  assessor cannot verify it until the repository is public.
- **Not met** — the criterion is not satisfied.
- **N/A** — not applicable; reason given.
- **Needs human** — only a maintainer can attest to this, not an AI agent.
- **Not demonstrated** — the project has no history that proves it yet.
- **Partial** — a suggested criterion is only partly met.

## Basics

| ID | Status | Evidence and action |
| --- | --- | --- |
| `description_good` | Met | `README.md` explains the problem: Grafana-native metrics, traces, and logs on OBI + Collector + ClickHouse, without a new UI. |
| `interact` | Met | `README.md` has install/build instructions, `CONTRIBUTING.md` explains contribution, and `.github/ISSUE_TEMPLATE/` provides feedback forms. |
| `contribution` | Met | `CONTRIBUTING.md` describes the PR/test/review process, including the human review gates. |
| `contribution_requirements` | Met | `CONTRIBUTING.md` lists tests-first, exact-value tests, no silent skips, dependency policy, and the "one PR, one change" rule. |
| `floss_license` | Met | Apache-2.0 in `LICENSE`. |
| `floss_license_osi` | Met | Apache-2.0 is OSI-approved. |
| `license_location` | Met | `LICENSE` at the repository root; the README license section also states Apache-2.0. |
| `documentation_basics` | Met | `README.md`, `docs/install.md`, `docs/support-matrix.md`, and the per-language support pages. |
| `documentation_interface` | Met (caveat) | `docs/support-matrix.md`, `docs/promql-support.md`, `docs/traceql-support.md`, `docs/logql-support.md`, and `docs/install.md` describe endpoints, supported inputs, chart values, and explicit unsupported behavior. There is no formal OpenAPI document. |
| `sites_https` | Met | GitHub repository and documentation URLs are HTTPS. |
| `discussion` | Met | GitHub Issues are enabled and URL-addressable; `.github/ISSUE_TEMPLATE/` covers bug reports and feature requests. GitHub Discussions is disabled, which is acceptable because Issues provide the required mechanism. |
| `english` | Met | All project documentation and issue templates are in English. |
| `maintained` | Met | Recent commits (latest 2026-11-06) and active CI. The Scorecard "created within 90 days" warning is repository age, not inactivity. |

## Change Control

| ID | Status | Evidence and action |
| --- | --- | --- |
| `repo_public` | **Met** | The repository is public at `github.com/jawaracloud/skopos` as of 2026-11-01. |
| `repo_track` | Met | Git history records author, timestamp, and change in every commit. |
| `repo_interim` | Met | The repository contains dozens of incremental commits and feature branches, not only final release snapshots. |
| `repo_distributed` | Met | Git is used. |
| `version_unique` | Not met | No image release under the current process exists yet. The in-tree `VERSION ?= 0.1.0-alpha.1` is a build default, not a published image release. Publish `v0.1.0-alpha.2` to satisfy this criterion. |
| `version_semver` | Met (release pending) | The in-tree version and chart `appVersion` use SemVer (`0.1.0-alpha.1`). Publish a release with the same scheme. |
| `version_tags` | Not met | Only the legacy lightweight chart tag `skopos-0.1.0-alpha.1` exists. Create an annotated `v0.1.0-alpha.2` tag and release for the first image release under the new process. |
| `release_notes` | Not met | No image release exists. The release workflow writes notes with the image digest, chart digest, and verification commands. Publish `v0.1.0-alpha.2` and record the notes. |
| `release_notes_vulns` | N/A | No release exists and there are no publicly known fixed vulnerabilities to list. Revisit for every release that fixes a CVE. |

### Review and branch protection

The `main-protection` ruleset requires a pull request, green required checks,
linear history, and blocks force pushes and deletion. It applies to admins and
has no bypass actors. It requires **zero approving reviews** because the project
has one maintainer. That is an accepted exception, not a claim that independent
review exists; do not describe this project as having two-person review until a
second maintainer joins and the approval requirements change. The residual risk
is recorded in
[openssf-scorecard.md](openssf-scorecard.md#review-exception-one-maintainer).

## Reporting

| ID | Status | Evidence and action |
| --- | --- | --- |
| `report_process` | Met | GitHub Issues plus `.github/ISSUE_TEMPLATE/bug_report.yml`; security reports use the private process in `SECURITY.md`. |
| `report_tracker` | Met | GitHub Issues is enabled. |
| `report_responses` | Not demonstrated | There are zero issues in the repository, so there is no majority-response history in the last 2–12 months. Do not claim this until public reports exist and are answered. |
| `enhancement_responses` | Not demonstrated | Same as above; no enhancement requests exist yet. |
| `report_archive` | Met | GitHub Issues is the searchable archive. |
| `vulnerability_report_process` | Met | `SECURITY.md` explains the private advisory process and supported versions. |
| `vulnerability_report_private` | Met | `SECURITY.md` links the GitHub private advisory form at `https://github.com/jawaracloud/skopos/security/advisories/new`. External reporters can use it now. |
| `vulnerability_report_response` | Not demonstrated | No vulnerability reports exist, so there is no response history. `SECURITY.md` now states 7-day acknowledgement, 14-day initial assessment, and 90-day default disclosure targets. The maintainer must honor them. |

## Quality

| ID | Status | Evidence and action |
| --- | --- | --- |
| `build` | Met | `Makefile` target `build` runs `go build`; `go build ./...` is also a CI step. |
| `build_common_tools` | Met | Go, Make, and Helm are standard tools. |
| `build_floss_tools` | Met | The Go toolchain, Make, and Helm are FLOSS. Docker is only needed for the optional image/release path, not for the Go build. |
| `test` | Met | `go test ./...`, `make test`, and the test seams documented in `CONTRIBUTING.md`. |
| `test_invocation` | Met | Standard `go test ./...` and a `make test` wrapper. |
| `test_most` | Partial | `go test -cover ./...` on 2026-11-07: `auth` 100.0, `httpapi` 91.1, `frontend` 87.3, `semctx` 85.5, `checkpoint` 85.0, `config` 81.3, `loki` 76.5, `chclient` 73.9, `tempo` 73.6, `prom` 67.0, `sqlgen` 66.6, `plan` 31.1, `cmd/gateway` 46.3; small test helpers are lower or have no statements. There is no coverage gate. This is a suggested criterion, not a must. |
| `test_continuous_integration` | Met | `.github/workflows/ci.yml` runs unit, race, fuzz, ClickHouse integration, PromQL oracle, Tempo/Loki differentials, cerberus, and Helm renders. |
| `test_policy` | Met | `CONTRIBUTING.md` rules 1–4 require tests first, exact values, no silent skips, and no guessing. The PR template repeats the test requirement. |
| `tests_are_added` | Met | Recent changes add tests: `1276021` adds LogQL pushdown tests, `860f6bd` adds frontend tests, `902a989` adds the cerberus gate. Use `git show --stat <commit>` to inspect. |
| `tests_documented_added` | Met | `CONTRIBUTING.md` "Test seams" and "Development loop"; `.github/pull_request_template.md`. |
| `warnings` | Met | `go vet ./...` is a blocking CI step. |
| `warnings_fixed` | Met | CI fails on `go vet` and `go test`; `make check` includes vet. |
| `warnings_strict` | Partial | `go vet` is the default suite, not a maximal linter. `staticcheck`/`gosec` are not enabled; `go test -race` and the fuzz gate cover other classes. |

## Security

| ID | Status | Evidence and action |
| --- | --- | --- |
| `know_secure_design` | Needs human | A maintainer must attest to secure-design knowledge. The repository has an ADR, threat model notes in `SECURITY.md`, a read-only ClickHouse user, parameterized SQL, and least-privilege OBI capabilities, but an agent cannot certify a person's knowledge. |
| `know_common_errors` | Needs human | Same: the maintainer must attest to knowledge of common vulnerability classes and mitigations (injection, authz, SSRF, supply chain, unsafe deserialization, etc.). |
| `crypto_published` | N/A | The gateway does not implement cryptography; it speaks HTTP and does not terminate TLS. Operators must provide TLS at the ingress or service mesh if their threat model requires it. No custom crypto algorithms. |
| `crypto_call` | N/A | No cryptography is called or re-implemented by the project. |
| `crypto_floss` | N/A | No cryptographic functionality in the project. |
| `crypto_keylength` | N/A | No project-managed keys or key lengths. |
| `crypto_working` | N/A | No project-managed cryptographic algorithms or modes. |
| `crypto_weaknesses` | N/A | No project-managed weak algorithms. |
| `crypto_pfs` | N/A | No project-managed key agreement. |
| `crypto_password_storage` | Met (caveat) | Optional Basic auth credentials live in a Kubernetes Secret and are compared in constant time in the gateway process. The Secret at-rest protection depends on the cluster (enable etcd encryption); Basic auth must run over TLS. |
| `crypto_random` | N/A | The gateway does not generate keys or nonces. |
| `delivery_mitm` | Met (release pending) | Source is delivered over HTTPS GitHub; release images would use HTTPS/GHCR. No binary release exists yet, so the release path is not demonstrated. |
| `delivery_unsigned` | N/A | Release checksums are published inside the release assets over HTTPS. The legacy chart release is keyless-signed; the first image release under the new process is pending. |
| `vulnerabilities_fixed_60_days` | Met | `GO-2026-6860` (CVE-2026-78661) and `GO-2026-6866` (CVE-2026-97045), published 2026-10-27, affected `golang.org/x/net` before v0.62.0. The root module and `test/compat/prometheus` now pin v0.62.0; the fix landed on 2026-11-07, within 60 days. An OSV API query for v0.62.0 returns no findings, `govulncheck v1.9.0 ./...` reports "No vulnerabilities found," and the local Scorecard Vulnerabilities check reports 10. |
| `vulnerabilities_critical_fixed` | N/A | No critical vulnerabilities have been reported. |
| `no_leaked_credentials` | Met | Gitleaks v8.30.1 scanned all 27 commits and the working tree on 2026-11-01 with no leaks. GitHub secret scanning and push protection are enabled on the public repository; the repository also has private vulnerability reporting and Dependabot alerts/security updates. |

## Analysis

| ID | Status | Evidence and action |
| --- | --- | --- |
| `static_analysis` | Met | `go vet ./...` runs on every push/PR. `actionlint` and `zizmor` run in the required `policy` job, and `zizmor` checks workflow permissions, template injection, and dangerous triggers. |
| `static_analysis_common_vulnerabilities` | Met | Default CodeQL setup for Go is configured and uploads results to code scanning. On 2026-11-07 the open-alert API returned `[]`; the latest analysis (commit `75dbcf6`, CodeQL 2.27.4) completed with 0 results across 34 rules. `govulncheck` covers dependency vulnerabilities separately. |
| `static_analysis_fixed` | N/A | No confirmed medium-or-higher static-analysis finding is open; 0 open and 0 dismissed code-scanning alerts. |
| `static_analysis_often` | Met | `ci.yml` runs `go vet` on every push and pull request, and default CodeQL setup analyzes pushes to `development`/`main` and pull requests. |
| `dynamic_analysis` | Met | Unit tests, race detector, ClickHouse integration tests, SQL emitter fuzzing, OBI e2e (manual), and reference differentials. |
| `dynamic_analysis_unsafe` | N/A | The project is Go (memory-safe). It does not compile C/C++ project code; OBI is an external container. |
| `dynamic_analysis_enable_assertions` | Met | Tests and fuzzing run with Go assertions/panics enabled; `go test -race` is a blocking CI step. |
| `dynamic_analysis_fixed` | N/A | No confirmed medium-or-higher dynamic-analysis finding is open. |

## Suggested application narrative

Use short, factual answers. The table above is the source of truth. For each
criterion, link the file and command that proves it; do not paste claims that
are not in this repository.

The most important answers to get right:

- **Repo URL:** only after the repository is public.
- **Discussion:** the public GitHub Issues URL.
- **Contribution:** `CONTRIBUTING.md`.
- **Security:** `SECURITY.md` and the private advisory URL.
- **Code review:** describe the solo-maintainer exception honestly. The project
  requires PRs and green checks but zero approvals; do not claim two-person
  review or a satisfied Code-Review criterion. See
  [openssf-scorecard.md](openssf-scorecard.md#review-exception-one-maintainer).
- **Build/test:** `Makefile`, `.github/workflows/ci.yml`, and the exact commands
  `make test`, `make check`.
- **Release:** leave `version_unique`, `version_tags`, and `release_notes` as
  "not met" until a real release exists. Do not claim them from an in-tree
  version string.
- **Security knowledge:** answer personally. The maintainer must be able to
  explain the design and common error classes; do not let an agent answer this
  on their behalf.

## After approval

1. Add the badge URL issued by bestpractices.dev. Do not invent a badge URL or
   add a placeholder to `README.md`.
2. Record the project ID and approval date in this file.
3. Re-check the criteria when the first signed release is published, when
   dependency review changes, and when the issue tracker has response history.

## Silver and Gold

This draft targets **passing** only. Silver and Gold are not claimed. Known
gaps include an independent security review (Gold `security_review`), signed
releases (Silver `signed_releases`), reproducible builds, per-file license
headers, a bus factor above one, and broader test coverage. Adding
`CODE_OF_CONDUCT.md` and `GOVERNANCE.md` helps the Silver governance/conduct
criteria but does not make the project Silver-ready.
