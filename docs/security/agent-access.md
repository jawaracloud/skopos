# Agent and automation access

Agents and automation must not use the maintainer's admin credential. The
repository rulesets and release workflow assume that only a human admin can
create a `v*` tag or edit the rulesets.

## Required identity

Create a dedicated fine-grained token or GitHub App installation for agents:

- Repository access: only `jawaracloud/skopos`.
- Contents: read and write (branches and commits).
- Pull requests: read and write.
- Metadata: read.
- No repository administration.
- No workflow-file write permission.
- No secrets permission.
- No packages write permission.
- Short expiry, for example 90 days.

A fine-grained token is the simplest option for local agents. Use a GitHub App
installation token if the automation runs in CI and the token can be minted per
run.

## Configure agents

Point the agent at the dedicated token, not the maintainer's `gh` session:

```sh
export GH_TOKEN=...        # the agent token
export GITHUB_TOKEN=...    # if the tool reads this name
```

Do not run `gh auth login` with the maintainer account in an agent environment.

## Verify the boundary

Run these checks before trusting the setup:

```sh
# Should not include "admin": true.
gh api repos/jawaracloud/skopos --jq '.permissions'

# Should fail.
git push origin HEAD:main
git tag v0.0.0-agent-test
git push origin v0.0.0-agent-test
```

The `main` and `release-tags` rulesets are the enforcement point. The token
permissions are defense in depth.

## Rotation

- Rotate the agent token on a fixed schedule and after any suspected exposure.
- Revoke the old token before issuing a replacement.
- Review branches, pull requests, and workflow runs created during the exposure
  window.
- Record the rotation in the repository or incident notes.

## Maintainer account

The human maintainer account is the release authority. It should use a hardware
security key or passkey for 2FA, store recovery codes offline, and avoid
long-lived PATs when a GitHub App or short-lived token is possible. Account
compromise is the highest-ranked threat in the
[supply-chain threat model](threat-model.md); the token split above is what makes
the rulesets meaningful.
