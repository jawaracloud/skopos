# Supply-chain security

This page describes the controls Skopos uses to protect its build and release
pipeline. It is an evidence log, not a claim that the project is immune to
compromise. Every control below links to the place that enforces it.

## Control matrix

| Layer | Control | Enforced by | Evidence |
| --- | --- | --- | --- |
| Who triggers releases | One annotated `vX.Y.Z` tag; no manual publish | `.github/workflows/release.yml` trigger and preflight | Workflow refuses non-annotated tags, non-`main` commits, existing tags, releases, and image tags |
| Who can create release tags | Repository admins only | Tag ruleset for `refs/tags/v*` | GitHub ruleset; update and deletion blocked |
| Who can change `main` | Pull request with required checks, zero required approvals, admins included | `main` ruleset | Ruleset with `enforce_admins=true`; direct pushes and force pushes blocked |
| Agent authority | Non-admin token with contents and pull-request write; no admin, workflow, or secret access | [Agent access](agent-access.md) | Token permission check and ruleset drill |
| What CI executes | `pull_request` workflows; no `pull_request_target` | `.github/workflows/ci.yml`; `policy` job | `zizmor` and an explicit pattern check |
| Workflow permissions | Read-only by default; write scopes only in publish jobs | Workflow-level `permissions` | `policy` job and code review |
| Action supply chain | 40-character SHA pins; selected action allowlist | Repository Actions settings; `policy` job | `sha_pinning_required=true`; `actionlint`/`zizmor` |
| Container image inputs | Digest pins in Dockerfile, Compose, chart, and CI services | Repository files; `policy` job | Digest pins checked by review and CI |
| Go dependencies | `go.sum` plus committed `vendor/`; drift and checksum checks | `ci.yml`, `Makefile`, `release.yml` | `go mod verify`, `go mod vendor`, `git diff --exit-code` |
| Dependency updates | Dependabot with a five-day cooldown; no auto-merge | `.github/dependabot.yml` | Cooldown blocks brand-new releases; dependency changes remain separate PRs |
| New vulnerable dependencies | Dependency review on every PR | `.github/workflows/dependency-review.yml` | Blocks high/critical vulnerabilities and AGPL/GPL licenses |
| License policy | Allowlist check | `policy` job | `go-licenses check` with Apache-2.0, BSD-3-Clause, MIT, ISC |
| Release build | Read-only build job; separate publish jobs | `.github/workflows/release.yml` | Build artifacts pass between jobs; publish jobs have minimal scopes |
| Release credentials | Keyless OIDC only; no long-lived signing keys | Release workflow | No `COSIGN_PRIVATE_KEY`; `id-token: write` only in signing jobs |
| Image and chart signatures | Keyless Sigstore with transparency log | Release workflow | `cosign sign`, `cosign verify`; Sigstore bundles attached to release |
| Provenance and SBOM | GitHub artifact attestations | Release workflow | `actions/attest@v4` for image and chart |
| Release immutability | Draft release populated before publish | Repository setting and release workflow | GitHub immutable releases; assets and tag locked after publish |
| Consumer verification | Documented cosign and `gh attestation verify` commands | `docs/release.md`; `scripts/verify-release.sh` | `make verify-release` |
| Secure chart default | Gateway disabled unless release values enable it | `deploy/helm/skopos/values.yaml` | Default install cannot pull a mutable image tag |
| Incident response | Token, release, and dependency playbooks | `docs/security/incident-response.md` | Maintainer runbook |

## Where we still fall short

- **One maintainer.** There is no second human reviewer and no protected
  environment with required reviewers. The rulesets and automated gates reduce
  mistakes, but a compromised admin account can edit them.
- **No egress firewall.** GitHub-hosted runners have unrestricted outbound
  network access. The release jobs are short-lived and credential-scoped, but a
  compromised step could still exfiltrate data it can read.
- **No Actions Data Stream.** Workflow telemetry is limited to GitHub logs and
  the audit log. There is no near-real-time external feed for anomalous
  dependency resolution or network behavior.
- **No SLSA L3.** GitHub artifact attestations provide signed build provenance,
  but the project does not run a hardened, isolated SLSA builder.
- **No signed commits or tags.** Release tags are annotated and protected by a
  ruleset, but they are not GPG-signed. Signed tags would add non-repudiation,
  not protection against a compromised admin account.
- **No reproducible builds.** The build uses vendored modules and `-trimpath`,
  but there is no independent rebuild comparison.
- **Legacy chart release.** `skopos-0.1.0-alpha.1` predates this process and is
  not covered by the current signing and provenance guarantees.

## Review cadence

Review this page when a release workflow, ruleset, action allowlist, or
dependency policy changes. Re-run the OpenSSF Scorecard after the first release
under the new process and record the result in
[../openssf-scorecard.md](../openssf-scorecard.md).
