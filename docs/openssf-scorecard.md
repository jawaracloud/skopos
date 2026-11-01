# OpenSSF Scorecard

This page records how Skopos is scanned, what the current results mean, and
which findings need a maintainer action. It is an evidence log, not a badge
claim.

## Public repository and workflows

`github.com/jawaracloud/skopos` became public on 2026-11-01. The official
`ossf/scorecard-action` now runs on every push to `main`, weekly, and on manual
dispatch. It publishes results to the public Scorecard API and uploads SARIF to
GitHub code scanning; no GHAS is required for a public repository.

- `.github/workflows/scorecard.yml` is the official action path.
- `make scorecard` / `scripts/scorecard.sh` remains the local CLI fallback
  (pinned v5.6.0, checksum-verified).
- The old private-repo CLI workflow and the GHAS-only workflow were removed.

The public result is also the source of the badge in `README.md`. Do not point
the badge at a different or fabricated result.

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

`.github/workflows/scorecard.yml` runs the official action with least-privilege
permissions (`contents: read`, `actions: read`, `security-events: write`,
`id-token: write`), publishes results, and uploads SARIF to code scanning. The
SARIF artifact is retained for 1 day, matching the current repository limit.

The action uses `secrets.SCORECARD_TOKEN` when it exists. That optional
fine-grained PAT needs **Administration: read-only**; without it, the action's
default `GITHUB_TOKEN` cannot read classic branch protection and the
Branch-Protection check returns an internal error (`-1`). The branch protection
itself is configured on `main`; the token is only needed for the check to see
it. Create the secret in repository settings, then re-run the workflow.

## Snapshots

The pre-merge baseline was **4.1** at commit
`0d6841772f61b13585dfc42fc9115a53e76f7aec`. After the distributed, S3,
full-text, and OpenSSF changes were merged, actions were pinned, and the
repository became public, the official action reports **7.1** at commit
`7f429ae` (run `41282506996`, published 2026-11-01T06:51:59Z). The number is a
diagnostic, not a badge.

| Check | Score | What the latest run found | Evidence / next action |
| --- | --- | --- | --- |
| Binary-Artifacts | 10 | No binaries found. | `bin/` and `dist/` are git-ignored; the Docker build produces the binary outside the source tree. |
| Branch-Protection | -1 | The action token cannot read classic branch protection rules. | Branch protection **is configured** on `main`: PR required, one approval, six required status checks, linear history, no force push/deletion, conversation resolution. Add a fine-grained `SCORECARD_TOKEN` secret with **Administration: read-only** and pass it as `repo_token`; without that token Scorecard cannot score this check. `enforce_admins` is currently false so the maintainer can recover from a broken workflow; enable it when a second maintainer/PR process exists. |
| CI-Tests | -1 | No pull request found. | The project has not yet used PRs. Once changes go through PRs, Scorecard can evaluate CI on PRs. |
| CII-Best-Practices | 0 | No badge effort detected. | The application draft is in [openssf-best-practices.md](openssf-best-practices.md). The repository is now public; the remaining step is to submit at bestpractices.dev and answer the human-attestation criteria. |
| Code-Review | 0 | 0/30 approved changesets. | Agent commits were pushed directly to `main`. Use PRs with at least one human approval; branch protection now requires it for non-admin contributors. |
| Contributors | 10 | Five contributing organizations detected. | Informational; derived from commit metadata. Keep commit identities accurate. |
| Dangerous-Workflow | 10 | No dangerous workflow patterns. | Workflows do not use `pull_request_target` with untrusted checkout, script injection, or similar patterns. Re-check after workflow changes. |
| Dependency-Update-Tool | 10 | Dependabot detected. | `.github/dependabot.yml` covers the root Go module, `test/compat/prometheus`, `test/e2e/obi/app`, and GitHub Actions. |
| Fuzzing | 10 | Project is fuzzed. | `FuzzEmitNoUserValueLeak` runs as a blocking 30-second CI gate in `internal/sqlgen`. |
| License | 10 | License file detected. | Apache-2.0 in `LICENSE`; the README license section is consistent. |
| Maintained | 0 | Repository was created within the last 90 days. | This is a repository-age warning, not evidence of abandonment. Keep weekly activity; the check should improve as the repository ages. |
| Packaging | -1 | No packaging workflow detected. | `release.yml` builds artifacts, but no image, chart, or GitHub release has been published. Publish the alpha image/chart and create a release. |
| Pinned-Dependencies | 10 | All GitHub Actions and container images are pinned by commit SHA or digest. | `ci.yml`, `release.yml`, and the Scorecard workflow are SHA-pinned; the chart images are digest-pinned. Re-check after every action bump. |
| SAST | 0 | No SAST tool detected in the last published scan. | Default CodeQL setup for Go was enabled on 2026-11-01 and its first analysis passed; the next Scorecard run should pick it up. `go vet`, `govulncheck`, and the fuzz gate also run. |
| Security-Policy | 10 | Security policy file detected and linked. | `SECURITY.md` links the private advisory form and states response targets; private vulnerability reporting is enabled. |
| Signed-Releases | -1 | No releases found. | Release signing is scaffolded but optional and unused. Publish a release and attach signatures (`.sig`, `.sigstore`, or `.intoto.jsonl`); see [release.md](release.md). |
| Token-Permissions | 10 | Workflow tokens follow least privilege. | `release.yml` keeps `contents: read` at the top level and scopes write permissions to the release job. `ci.yml` and the Scorecard workflow are read-only or scoped. |
| Vulnerabilities | 8 | OSV reports `GO-2026-6860` and `GO-2026-6866` for `golang.org/x/net` v0.60.0. | The module is present in `go.sum` but not in `vendor/modules.txt`; `govulncheck v1.9.0 ./...` reports "No vulnerabilities found" (no reachable vulnerable symbols). The fixed version is `golang.org/x/net` v0.62.0; update it in a separate human-reviewed dependency PR. |
| Webhooks | N/A | No repository webhooks configured. | Verified with the repository hooks API on 2026-11-01. Re-check if a webhook is added; it must use a secret. |

The current official score is **7.1**. It is a diagnostic number, not a badge,
and it will change after the findings above are addressed. Do not quote it as
a security rating.

## What a maintainer must do manually

These actions cannot be done from a pull request:

1. **Scorecard token.** Create a fine-grained PAT with **Administration:
   read-only** for this repository, store it as the `SCORECARD_TOKEN` secret,
   and re-run the workflow. This lets the Branch-Protection check read the
   configured rules.
2. **Branch protection.** The protection is configured; review it after adding
   maintainers. Set `enforce_admins=true` when the maintainer no longer needs
   the emergency bypass.
3. **PR-based review.** Use PRs for future changes so Code-Review and CI-Tests
   have data. Direct admin pushes bypass the rules today.
4. **CodeQL.** Default CodeQL setup for Go is enabled; keep it on `main` and
   review new code-scanning findings.
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
