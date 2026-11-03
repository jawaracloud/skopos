# Supply-chain threat model

This threat model covers the source repository, CI/CD, dependencies, release
artifacts, and the identities that can change them. It does not cover runtime
security of the gateway, ClickHouse, OBI, or the Kubernetes cluster; those are
covered in `SECURITY.md` and the deployment documentation.

## Assets

- Source code and the committed `vendor/` tree.
- GitHub Actions workflows, composite actions, and repository settings.
- Maintainer and agent credentials, including tokens and SSH keys.
- The GitHub organization and repository administration.
- Container image tags and digests in GHCR.
- Helm chart OCI tags and digests.
- GitHub release assets, SBOMs, signatures, and attestations.
- Consumer trust in the install and verification instructions.

## Actors and trust boundaries

| Actor | Trust | Can affect |
| --- | --- | --- |
| Repository admin | Highest; can edit rulesets and repository settings | Everything in the repository and releases |
| Maintainer | Write, review, and release authority | Source, workflows, releases |
| Agent identity | Write to branches and pull requests; no admin, workflow, or secret access | Proposed changes only |
| External contributor | Fork or branch pull requests | Proposed changes, never release credentials |
| Upstream dependency maintainer | Provides code consumed through vendored modules, actions, and images | Build inputs |
| GitHub, Sigstore, and the container registry | Platform trust | Build execution, OIDC identity, signing, storage |
| Consumer | Verifies artifacts before use | Deployment decisions |

The main trust boundaries are:

1. A fork or agent branch to the repository's `main` branch.
2. A pull request workflow to a write-scoped release workflow.
3. The build job to the publish and signing jobs.
4. The repository to GHCR and GitHub releases.
5. Published artifacts to consumer installations.

## Top threats and controls

### 1. Compromise of a maintainer or admin account

An attacker with the maintainer account can create a release tag, change
workflows, or edit rulesets. Controls:

- Hardware-key or passkey 2FA and offline recovery codes.
- No long-lived PATs where a short-lived token or GitHub App token can be used.
- Agent identities cannot administer the repository.
- Tag and `main` rulesets with `enforce_admins=true` and admin-only tag creation.
- Release preflight requires the tag to be on `main` and refuses existing
  artifacts.
- Immutable releases prevent post-publication asset and tag changes.

Residual risk: a compromised admin can edit the ruleset. This cannot be fully
removed with a single administrator; a second trusted maintainer is the only
structural fix.

### 2. Abuse of release authority

An attacker with write access could try to push a tag or dispatch a release for
an unreviewed commit. Controls:

- Release workflow triggers only on `push: tags: v*`.
- No manual publish input.
- Tag ruleset restricts `v*` creation to repository admins.
- Preflight checks annotation, ancestry on `main`, chart version, existing
  release/image tags, and release checks.
- Publish jobs run in the `release` environment with a `v*` tag policy.

### 3. Compromise of an upstream action, module, or image

A compromised dependency could execute in CI or ship inside the image. Controls:

- Actions are pinned by full commit SHA; the repository enforces SHA pinning.
- The action allowlist is limited to GitHub-owned and explicitly allowed
  actions.
- Container images are pinned by digest in Dockerfiles, Compose files, the
  chart, and CI services.
- Go modules are vendored and reviewed; `go mod verify` and vendor drift checks
  run in CI and the release preflight.
- Dependabot uses a five-day cooldown; dependency changes are separate,
  reviewed PRs.
- Dependency review blocks high/critical vulnerabilities and disallowed
  licenses.

Residual risk: a compromised upstream release that passes review can still ship.
Vendoring and cooldown reduce the window but do not eliminate it.

### 4. Malicious pull request

A contributor could try to execute code in CI or steal credentials. Controls:

- No `pull_request_target`; PR workflows run with read-only tokens.
- `policy` job rejects dangerous workflow patterns and unpinned actions.
- The release workflow never checks out or executes PR code.
- Publish jobs have no access to PR code and run only from a tag on `main`.
- CODEOWNERS routes workflow changes to the maintainer for review.

Residual risk: a workflow change in a PR is reviewed by the same person who
merges it. A second reviewer would strengthen this.

### 5. Unsigned or tampered release artifact

An attacker who can write to the registry or release assets could replace an
artifact. Controls:

- Keyless Sigstore signatures on the image and chart digest.
- SBOM and provenance attestations attached to the artifacts.
- Immutable GitHub releases lock assets and tags after publication.
- Consumers pin digests and verify signatures and attestations.
- `scripts/verify-release.sh` and `docs/release.md` provide the verification
  commands.

Residual risk: consumers who ignore verification remain exposed. Signatures
prove origin and integrity, not that the code is free of vulnerabilities.

### 6. Compromised agent identity

An agent token could push malicious branches or open PRs. Controls:

- Agent tokens have repository contents and pull-request write, no
  administration, workflow, or secret access.
- `main` ruleset requires a PR and green checks; direct pushes fail.
- Tag ruleset prevents tag creation by non-admins.
- Agents cannot read release secrets; keyless signing requires an Actions OIDC
  identity.

Residual risk: an agent can still open a malicious PR that a maintainer might
merge. Human review remains the gate.

## Out of scope

- Runtime exploitation of the gateway or ClickHouse.
- Kubernetes cluster compromise.
- Compromise of GitHub itself or the Sigstore public-good infrastructure.
- Malicious insider maintainers acting with full admin authority.

## Review

Review this threat model when a trust boundary changes: a new publish path, a
new credential type, a new action, a new registry, or a new maintainer. Record
material changes in the pull request that introduces them.
