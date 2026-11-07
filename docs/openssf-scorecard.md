# OpenSSF Scorecard

This page records how Skopos is scanned, what the current results mean, and
which findings need a maintainer action. It is an evidence log, not a badge
claim.

> **Status note:** the last published Scorecard result is **6.8** at commit
> `06319585f2e1fcbbeaf8247e6d81c067b019d139` (published 2026-11-03T23:12:54Z).
> That scan predates the `golang.org/x/net` fix in this change, so its
> Vulnerabilities score still reflects two now-fixed OSV entries. The result
> refreshes on the next official workflow run after merge. Do not quote 6.8 as
> the post-fix posture.

## Current evidence (2026-11-07)

| Area | Status | Evidence |
| --- | --- | --- |
| Code scanning | Clean | `gh api 'repos/jawaracloud/skopos/code-scanning/alerts?state=open'` returns `[]` (0 open, 0 dismissed). Default CodeQL setup is configured for Go with the default query suite and weekly schedule. The latest analysis on `refs/heads/development` (commit `75dbcf6`, CodeQL 2.27.4) completed with **0 results** across 34 rules. |
| OSV / `golang.org/x/net` | Fixed | `GO-2026-6860` (CVE-2026-78661) and `GO-2026-6866` (CVE-2026-97045) affect `golang.org/x/net` before v0.62.0. The root module and `test/compat/prometheus` now pin **v0.62.0**. An OSV API query for v0.62.0 returns no vulnerabilities. |
| `govulncheck` | Clean | `govulncheck v1.9.0 ./...` reports `No vulnerabilities found.` |
| Tests | Pass | `go test ./...`, `go test -race ./...`, `go test -cover ./...`, and `go vet ./...` pass after the dependency update. `go mod verify` reports `all modules verified`. |
| Branch ruleset | Enforced on `main` | `main-protection` (ruleset `26493931`) requires a pull request, green required checks, linear history, and blocks force pushes and deletion. It has no bypass actors and applies to admins. It requires **zero approving reviews** by design. |
| Review | Accepted exception, not fully met | The project has one maintainer. The PR and required checks are the gate; there is no fake second reviewer. The residual risk is recorded below. |
| Scorecard | Last published 6.8; local post-fix checks improve | The published API snapshot still shows Vulnerabilities 8. A local CLI v5.6.0 `--local=.` run after the dependency bump reports Vulnerabilities 10 ("0 existing vulnerabilities detected") and Packaging 10. Local mode does not evaluate GitHub rulesets or Code-Review, so its overall score is not comparable with the published result. |

## Public repository and workflows

`github.com/jawaracloud/skopos` became public on 2026-11-01. The official
`ossf/scorecard-action` runs on every push to `main`, weekly, and on manual
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

To check the working tree without publishing, use the pinned CLI directly:

```sh
./dist/tools/scorecard-v5.6.0-linux-amd64 --local=. --show-details
```

`--local` skips GitHub-specific checks such as Branch-Protection, Code-Review,
and CI-Tests. It can also see ignored local build outputs, so its overall score
is diagnostic only.

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

To score Branch-Protection, create a fine-grained `SCORECARD_TOKEN` secret with
**Administration: read-only** and uncomment the `repo_token` line in the
workflow. Without it, the action's default `GITHUB_TOKEN` cannot read the
ruleset and the Branch-Protection check returns an internal error (`-1`) or a
partial score. The ruleset itself is configured on `main`; the token is only
needed for the check to see it.

## Current check snapshot

Published scores are from the public API result at commit `0631958`
(2026-11-03). The "Current evidence" column records the state after the
2026-11-07 dependency change.

| Check | Published | Current evidence / next action |
| --- | --- | --- |
| Binary-Artifacts | 10 | No committed binaries. `bin/` and `dist/` are git-ignored; the Docker build produces the binary outside the source tree. |
| Branch-Protection | 4 | `main` has the enforced ruleset described above. Scorecard penalizes zero required approvals, disabled stale-review dismissal, no Code Owners or last-push approval, and the fact that `development` is not covered by a ruleset. The review exception is intentional; the zero-score components are residual risk, not an oversight. |
| CI-Tests | 0 | Published snapshot is stale. A local scan on 2026-11-07 sees 6 of 22 merged PRs checked by CI (score 2). Keep merging through PRs so every change is measured. |
| CII-Best-Practices | 0 | No badge effort detected. The application draft is in [openssf-best-practices.md](openssf-best-practices.md). The remaining step is to submit at <https://www.bestpractices.dev/> and answer the human-attestation criteria. |
| Code-Review | 0 | 0/30 approved changesets. This is the accepted solo-maintainer exception: PR plus required checks, zero approvals, no fake reviewer. It is **not** full two-person review and is not claimed as such. |
| Contributors | 10 | Five contributing organizations detected. Informational; derived from commit metadata. |
| Dangerous-Workflow | 10 | No dangerous workflow patterns. Workflows do not use `pull_request_target` with untrusted checkout, script injection, or similar patterns. |
| Dependency-Update-Tool | 10 | Dependabot detected. `.github/dependabot.yml` covers the root Go module, `test/compat/prometheus`, `test/e2e/obi/app`, and GitHub Actions. |
| Fuzzing | 10 | `FuzzEmitNoUserValueLeak` runs as a blocking 30-second CI gate in `internal/sqlgen`. |
| License | 10 | Apache-2.0 in `LICENSE`; the README license section is consistent. |
| Maintained | 0 | Repository-age warning, not abandonment. Recent commits and CI activity continue through 2026-11-06. |
| Packaging | -1 | Published snapshot predates detection. The local 2026-11-07 scan detects `.github/workflows/release.yml` and reports Packaging 10. The first image/chart release under the current process is still pending. |
| Pinned-Dependencies | 10 | GitHub Actions and container images are pinned by commit SHA or digest; the chart images are digest-pinned. Re-check after every action bump. |
| SAST | 0 | Scorecard's SAST check does not recognize GitHub's default CodeQL setup, so it reports 0. CodeQL is configured and clean: 0 open code-scanning alerts, latest analysis 0 results. Treat code scanning as the source of truth; this is a measurement gap, not a missing control. |
| Security-Policy | 10 | `SECURITY.md` links the private advisory form and states response targets; private vulnerability reporting is enabled. |
| Signed-Releases | 8 | The legacy chart release has a signed artifact. The tag-only workflow signs the image and OCI chart and attaches SBOM/provenance attestations; this row will change after `v0.1.0-alpha.2` is published. |
| Token-Permissions | 10 | `release.yml` keeps `contents: read` at the top level and scopes write permissions to the release job. `ci.yml` and the Scorecard workflow are read-only or scoped. |
| Vulnerabilities | 8 | Published snapshot predates the fix. `golang.org/x/net` v0.62.0 is now pinned in the root module and `test/compat/prometheus`; the OSV API returns no findings for v0.62.0 and `govulncheck` is clean. The local Scorecard Vulnerabilities check reports 10. |
| Webhooks | N/A | No repository webhooks configured. Verified with the repository hooks API on 2026-11-01; re-check if a webhook is added. It must use a secret. |

The last published official score is **6.8**. It is a diagnostic number, not a
badge, and it is stale relative to this change. Do not quote it as the post-fix
security rating.

## Review exception: one maintainer

The repository currently has one maintainer. The `main-protection` ruleset
therefore requires a pull request and green required checks but **zero approving
reviews**. This is an accepted exception, not full compliance with a two-person
review requirement. We do not add a fake second reviewer and do not claim the
Code-Review check is satisfied.

Residual risks:

- There is no independent human review before merge. The maintainer reviews
  their own changes against automated gates.
- A compromised admin account can edit the ruleset, disable a workflow, or use
  the release-tag bypass. The ruleset reduces mistakes; it does not defend
  against a compromised admin.
- `development` is the default integration branch and currently has no ruleset.
  PR and green-check discipline there is process, not enforcement.
- Stale-review dismissal, Code Owners review, and last-push approval are off
  because there is no second reviewer to satisfy them.

When a second maintainer joins, require at least one approval, enable stale
review dismissal and last-push approval, require Code Owners review, and record
the change in [GOVERNANCE.md](../GOVERNANCE.md).

## What a maintainer must do manually

These actions cannot be done from a pull request:

1. **Scorecard token (optional).** A fine-grained PAT with **Administration:
   read-only** lets the Branch-Protection check read the ruleset. This is a
   measurement improvement, not a security control; do not add a long-lived
   admin token just for the score.
2. **Rulesets.** `main` and `v*` tag rulesets are the enforced path. Decide
   separately whether to extend the PR/check ruleset to `development`; this
   change does not modify repository settings.
3. **PR-based review.** Continue using PRs. The solo-maintainer exception is
   documented above; do not fake a second approval. Add the approval
   requirements when a second maintainer joins.
4. **CodeQL.** Default CodeQL setup for Go is enabled; keep it on `main` and
   `development` and review new code-scanning findings. `actionlint` and
   `zizmor` cover workflow security. An explicit CodeQL workflow would improve
   the Scorecard SAST measurement, but default setup is the current control.
5. **Release and signing.** Publish `v0.1.0-alpha.2` with the tag-only workflow,
   immutable release, keyless signatures, SBOMs, and attestations. Follow
   [release.md](release.md).
6. **Best Practices badge.** After the first compliant release and reporting
   history exist, submit the application at <https://www.bestpractices.dev/>
   using [openssf-best-practices.md](openssf-best-practices.md).

## Badges

A public Scorecard result now exists, but this workstream does not edit
`README.md`. Add the official badge in a separate, evidence-backed change after
the next official workflow run. Use the snippet from that successful run; do
not point the badge at an older or fabricated result.

A Best Practices badge is added only after bestpractices.dev approves the
application and issues the project badge URL.
