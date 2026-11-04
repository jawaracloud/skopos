# Governance

Skopos is an alpha-stage project maintained in the `jawaracloud` GitHub
organization. This document describes the current, deliberately small process;
it is not a claim of foundation or multi-vendor governance.

## Maintainer

The maintainer of record is the account listed in `.github/CODEOWNERS`. That
account has final responsibility for repository access, releases, security
reports, and changes to this document. The organization owner controls
repository and organization settings.

## Decisions

- Day-to-day changes use pull requests and the review rules in
  [CONTRIBUTING.md](CONTRIBUTING.md).
- Architectural or cross-cutting decisions are recorded as ADRs under
  [`docs/adr/`](docs/adr/). An accepted ADR is the reference for later changes
  unless a newer ADR supersedes it.
- The maintainer seeks consensus in issues and pull requests, but makes the
  final call when consensus is not reached. Disagreement is recorded in the
  relevant issue or ADR rather than in private channels.

## Human review gates

The following changes require explicit human review before merge, as required by
CONTRIBUTING.md and ADR 0001:

- The SQL emitter and escaping.
- Helm `securityContext`, capabilities, and NetworkPolicy changes.
- Every dependency, image, and GitHub Actions version change.
- The release and signing pipeline.

Agent-generated changes do not bypass these gates.

## Release authority

Releases are created only by pushing an annotated `vX.Y.Z` tag whose commit is
already on `main`. The tag ruleset restricts tag creation to repository admins.
There is no manual publish workflow and no long-lived signing key.

The `main` ruleset requires a pull request, green required checks, and linear
history, and it applies to admins. Zero approvals are required because the
project currently has one maintainer; the gate is the PR and the checks, not a
fake second reviewer.

The admin can edit the rulesets as a break-glass action. Any use of break-glass
must be recorded in the pull request or incident notes and reviewed when a
second maintainer is available.

## Agent access

Automation and agents must use a dedicated non-admin identity with repository
contents and pull-request write access. The identity must not have repository
administration, workflow-file write, or secret access. Agents propose changes
through pull requests; they do not merge to `main`, create release tags, or run
releases.

## Becoming a maintainer

There is no fixed contribution quota. A contributor may be invited to become a
maintainer after sustained, high-quality contributions and demonstrated care
with the project's security and review rules. The current maintainer makes the
decision and records it in this file or in a repository issue.

## Changes to governance

Changes to this document use the normal pull-request process and require
approval from the maintainer of record. Material changes should explain the
reason and any effect on existing contributors.

## Conduct and security

- Code of conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
- Security reports: [SECURITY.md](SECURITY.md). Do not open a public issue for a
  vulnerability.
