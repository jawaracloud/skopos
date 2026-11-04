# Supply-chain incident response

This runbook covers incidents that affect the build and release pipeline. It is
maintained by the repository admin listed in `.github/CODEOWNERS`.

## First response

1. **Stop publication.** Disable the `release` workflow in GitHub Actions and
   freeze new tags.
2. **Preserve evidence.** Record the workflow run IDs, commit SHAs, tag names,
   image and chart digests, audit-log entries, and any suspicious tokens or
   sessions. Do not delete artifacts before capturing their digests.
3. **Assess scope.** Determine whether the incident affected source, CI,
   dependencies, signing identities, registry credentials, or published
   artifacts.
4. **Notify.** Use the private security advisory channel for reporter and
   maintainer communication. Publish a security advisory when consumers are
   affected.
5. **Recover with a new version.** Do not repair a published immutable release
   in place. Publish a new patch release with a clean tag and a fresh
   verification run.

## Compromised maintainer account

1. Revoke active sessions, PATs, OAuth app authorizations, SSH keys, and deploy
   keys.
2. Rotate the GitHub password and hardware-key/passkey enrollment.
3. Check the organization audit log for repository, ruleset, secret, package,
   and release changes.
4. Review recent tags, releases, workflow runs, and package versions.
5. If a release or image tag was created or changed, treat it as malicious:
   remove the release, withdraw the affected version, and publish a new patch.
6. Review whether rulesets, branch protection, or the action allowlist were
   changed.
7. Require a second maintainer to review the recovery if one is available.

## Compromised agent token

1. Revoke the token or GitHub App installation.
2. Identify every branch, PR, and workflow run created with it.
3. Check whether the token could push workflows, tags, or releases. It should
   not; if it could, treat it as a maintainer-account compromise.
4. Recreate the agent identity with the minimum permissions and a short expiry.
5. Review any PRs opened by the agent during the exposure window.

## Bad release or bad dependency

1. Disable the release workflow and stop new tags.
2. Mark the GitHub release as a security advisory subject; do not silently
   delete evidence.
3. Withdraw the image and chart versions from consumer instructions. If the
   registry allows deletion, remove the affected version; otherwise publish a
   new patch and pin consumers to it.
4. Rebuild from a known-good commit with a fresh signing run.
5. Publish a security advisory with affected versions, fixed versions, and
   verification instructions.
6. If the cause is an upstream dependency, add it to the review notes, update
   `docs/dependencies.md`, and consider a temporary pin or removal.

## Registry or signing incident

1. Stop publishing.
2. Verify the digests of the last known-good image and chart.
3. Check the transparency log and attestations for unexpected signatures or
   subjects.
4. Rotate registry credentials if they exist. The release path uses
   `GITHUB_TOKEN` and OIDC, so there are no long-lived signing keys to rotate.
5. Re-run `scripts/verify-release.sh` against the last known-good release.
6. Publish a new release only after the registry and signing path are verified.

## After the incident

- Write a short postmortem: timeline, root cause, affected artifacts, detection
  gap, and the control that will change.
- Update this runbook, the threat model, and the control matrix.
- Add or adjust an automated check so the same class of incident fails closed.
- Record the incident and the resulting control change in the repository.
