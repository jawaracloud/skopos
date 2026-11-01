# OpenSSF Scorecard

This page records how Skopos is scanned, what the current results mean, and
which findings need a maintainer action. It is an evidence log, not a badge
claim.

## Private repository and GHAS limitation

`github.com/jawaracloud/skopos` is a **private repository on a GitHub Free
organization**. The official `ossf/scorecard-action` supports private
repositories only when GitHub Advanced Security (GHAS) is enabled, because the
action uploads SARIF to GitHub code scanning. This repository does not have
GHAS, so the action's SARIF upload cannot work here.

The supported path is therefore:

- `.github/workflows/scorecard.yml` runs the pinned Scorecard **CLI** with a
  read-only `GITHUB_TOKEN` and uploads JSON as a workflow artifact. It needs no
  GHAS and no write permission.
- `.github/workflows/scorecard-ghas.yml` is the optional official action path.
  Its job is skipped unless the repository variable
  `ENABLE_GHAS_SCORECARD=true` is set, so normal CI never fails because GHAS is
  absent. Do not enable it until GHAS (or a public repository) is available.
- `make scorecard` / `scripts/scorecard.sh` runs the same pinned CLI locally.

The CLI in v5.6.0 supports `default`, `json`, `probe`, and `intoto` output, not
SARIF. SARIF is produced by the official action, which is why the private-repo
workflow stores JSON. Do not convert JSON to SARIF by hand and present it as a
code-scanning result.

## Run locally

The token only needs read access to the repository. The script accepts
`GITHUB_AUTH_TOKEN`, `GITHUB_TOKEN`, or `GH_TOKEN`; it fails with an explicit
message if none is set.

```sh
GITHUB_AUTH_TOKEN="$(gh auth token)" make scorecard
```

Outputs are written under `dist/` (git-ignored):

- `dist/scorecard/results.json`
- `dist/scorecard/scorecard.log`
- `dist/tools/scorecard-v5.6.0-<os>-<arch>` (checksum-verified download)

The CLI version is pinned to **v5.6.0** (commit
`61e84743356275f6ffca23b1b2f2cb945e19eb96`) in
[`scripts/scorecard.sh`](../scripts/scorecard.sh). The script verifies the
release tarball against the published SHA-256 checksum before running it. To
update, change the version and all four platform checksums together.

## CI workflow

`.github/workflows/scorecard.yml` runs weekly and on manual dispatch. It grants
only read permissions (`contents`, `issues`, `pull-requests`, `checks`) and
pins `actions/checkout` and `actions/upload-artifact` by commit SHA. The JSON
artifact is retained for 1 day (the repository artifact-retention limit on the current plan). It does not publish results to the public
Scorecard API.

The official action path, once GHAS is available:

1. Enable GitHub Advanced Security for the repository (or make the repository
   public).
2. Set the repository variable `ENABLE_GHAS_SCORECARD=true`.
3. Run **Actions -> scorecard-ghas -> Run workflow** manually.

That job uses `publish_results: false` and keeps results private. A badge must
not be added until a public, published Scorecard result exists.

## Snapshots

The pre-merge baseline was **4.1** at commit `0d6841772f61b13585dfc42fc9115a53e76f7aec`.
After the distributed, S3, full-text, and OpenSSF changes were merged and the
workflow actions were pinned, the same pinned CLI reports **6.5** at commit
`4391dbb` (workflow run `41281845302`). Both numbers are diagnostics, not a
badge.

| Check | Score | What the latest run found | Evidence / next action |
| --- | --- | --- | --- |
| Binary-Artifacts | 10 | No binaries found. | `bin/` and `dist/` are git-ignored; the Docker build produces the binary outside the source tree. |
| Branch-Protection | 0 | Branch protection not enabled on `main`. | The GitHub API returns 403: "Upgrade to GitHub Pro or make this repository public to enable this feature." A Free private repository has no branch protection or rulesets. A maintainer must make the repository public or upgrade the plan, then protect `main` (require PRs, one review, status checks, no force push). |
| CI-Tests | -1 | No pull request found. | The project currently commits directly to `main`. Once changes go through PRs, Scorecard can evaluate CI on PRs. |
| CII-Best-Practices | 0 | No OpenSSF Best Practices badge effort detected. | The application draft is in [openssf-best-practices.md](openssf-best-practices.md). The `repo_public` criterion is not met while the repository is private, so a badge cannot be awarded yet. |
| Code-Review | 0 | 0/30 approved changesets. | Agent commits were pushed directly to `main`. Require PRs and at least one human approval; this also unblocks CI-Tests and part of Branch-Protection. |
| Contributors | 10 | Five contributing organizations detected. | Informational; derived from commit metadata. Keep commit identities accurate. |
| Dangerous-Workflow | 10 | No dangerous workflow patterns. | Workflows do not use `pull_request_target` with untrusted checkout, script injection, or similar patterns. Re-check after workflow changes. |
| Dependency-Update-Tool | 10 | Dependabot detected. | `.github/dependabot.yml` covers the root Go module, `test/compat/prometheus`, `test/e2e/obi/app`, and GitHub Actions. |
| Fuzzing | 10 | Project is fuzzed. | `FuzzEmitNoUserValueLeak` runs as a blocking 30-second CI gate in `internal/sqlgen`. |
| License | 10 | License file detected. | Apache-2.0 in `LICENSE`; the README license section is consistent. |
| Maintained | 0 | Repository was created within the last 90 days. | This is a repository-age warning, not evidence of abandonment. Keep weekly activity; the check should improve as the repository ages. |
| Packaging | -1 | No packaging workflow detected. | `release.yml` builds artifacts, but no image, chart, or GitHub release has been published. Publish the alpha image/chart and create a release. |
| Pinned-Dependencies | 10 | All GitHub Actions and container images are pinned by commit SHA or digest. | `ci.yml`, `release.yml`, and both Scorecard workflows are SHA-pinned; the chart images are digest-pinned. Re-check after every action bump. |
| SAST | 0 | No SAST tool detected. | `go vet`, `govulncheck`, and the fuzz gate run, but Scorecard does not recognize them as SAST. CodeQL requires GHAS on a private repository. A free alternative such as `gosec` or `staticcheck` would need review and a new CI job; it is not added here. |
| Security-Policy | 10 | Security policy file detected and linked. | `SECURITY.md` links the private advisory form and states response targets. |
| Signed-Releases | -1 | No releases found. | Release signing is scaffolded but optional and unused. Publish a release and attach signatures (`.sig`, `.sigstore`, or `.intoto.jsonl`); see [release.md](release.md). |
| Token-Permissions | 10 | Workflow tokens follow least privilege. | `release.yml` keeps `contents: read` at the top level and scopes write permissions to the release job. `ci.yml` and the Scorecard workflows are read-only. |
| Vulnerabilities | 8 | OSV reports `GO-2026-6860` and `GO-2026-6866` for `golang.org/x/net` v0.60.0. | The module is present in `go.sum` but not in `vendor/modules.txt`; `govulncheck v1.9.0 ./...` reports "No vulnerabilities found" (no reachable vulnerable symbols). The fixed version is `golang.org/x/net` v0.62.0; update it in a separate human-reviewed dependency PR. |
| Webhooks | N/A | No repository webhooks configured. | Verified with the repository hooks API on 2026-11-01. Re-check if a webhook is added; it must use a secret. |

The current CLI score is **6.5**. It is a diagnostic number, not a badge, and it
will change after the findings above are addressed. Do not quote it as a
security rating.

## What a maintainer must do manually

These actions cannot be done from a pull request:

1. **Repository visibility.** Make `jawaracloud/skopos` public, or upgrade the
   organization to a plan with branch protection/rulesets and GHAS. Without
   this, Branch-Protection, CII-Best-Practices, and the public reporting parts
   of the Best Practices badge cannot pass.
2. **Branch protection/rulesets.** Protect `main`: require a pull request, at
   least one human approval, passing `ci`, no force push, and no deletion. If
   using a Free private repository, this feature is unavailable.
3. **PR-based review.** Stop pushing agent commits directly to `main`. Use PRs
   so Code-Review and CI-Tests have data.
4. **GHAS or public scanning.** For the official Scorecard action, enable GHAS
   and set `ENABLE_GHAS_SCORECARD=true`; for CodeQL, the same GHAS requirement
   applies on a private repository. For a public repository, enable the default
   CodeQL setup and the official Scorecard action.
5. **Release and signing.** Publish the alpha image and chart, create a GitHub
   release with release notes, and attach signatures and SBOMs. Set
   `COSIGN_SIGNING=true` and the key/OIDC material described in
   [release.md](release.md).
6. **Best Practices badge.** After the repository is public and the release and
   reporting gaps are addressed, submit the application at
   <https://www.bestpractices.dev/> using
   [openssf-best-practices.md](openssf-best-practices.md).

## Badges

No OpenSSF Scorecard badge or Best Practices badge is added to `README.md` by
this workstream. There is no published, public Scorecard result and no approved
Best Practices application, so a badge would be decorative and misleading.

- **Scorecard badge:** only after the official action runs with
  `publish_results: true` on a public repository. Use the official snippet from
  that successful run.
- **Best Practices badge:** only after bestpractices.dev approves the
  application and issues the project badge URL.

The integrator owns `README.md`; any badge addition should be a separate,
evidence-backed change.
