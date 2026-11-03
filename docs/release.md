# Release process

Skopos uses one annotated `vX.Y.Z` tag to release both the gateway image and
the Helm chart. There is no manual publish path. Re-running a failed release
means creating a new patch tag; published releases are immutable.

## What a release produces

- `ghcr.io/<owner>/skopos-gateway:<version>` with an immutable digest.
- `oci://ghcr.io/<owner>/charts/skopos:<version>` with an immutable digest.
- `skopos-<version>.tgz`: the packaged Helm chart.
- `skopos-gateway-<version>.spdx.json` and
  `skopos-chart-<version>.spdx.json`: SBOMs.
- `values.release.yaml`: the chart values that pin the released image digest.
- `release-metadata.txt` and `SHA256SUMS`.
- Cosign Sigstore bundles for the image and chart.
- GitHub artifact attestations for provenance and SBOMs.

## Before tagging

1. Update `deploy/helm/skopos/Chart.yaml`:
   - `version` must equal the tag without `v`.
   - `appVersion` must equal the tag without `v`.
2. Update the default image tag if needed. Release installs use
   `values.release.yaml`, not the chart default.
3. Merge the version bump through a pull request with all required checks.
4. Confirm that the commit is on `main` and CI is green.

## Create the release

```sh
git switch main
git pull --ff-only
git tag -a v0.1.0-alpha.2 -m "Skopos v0.1.0-alpha.2"
git push origin v0.1.0-alpha.2
```

`.github/workflows/release.yml` then:

1. Verifies that the tag is annotated, is an ancestor of `main`, and that the
   tag, release, and image tag do not already exist.
2. Runs `make release-check` and `go test -race ./...`.
3. Builds the image, chart, and SBOMs in a read-only job.
4. Pushes the image, signs it with keyless Sigstore, and attests provenance and
   SBOM.
5. Pushes the chart to GHCR OCI, signs it, and attests provenance and SBOM.
6. Generates `values.release.yaml` with the image digest.
7. Creates a draft GitHub release, attaches every asset, and publishes it as an
   immutable release.

If any signature or attestation verification fails, the GitHub release is not
created.

## Verify a release

```sh
VERSION=0.1.0-alpha.2 ./scripts/verify-release.sh
```

Or manually:

```sh
cosign verify ghcr.io/jawaracloud/skopos-gateway:0.1.0-alpha.2 \
  --certificate-identity 'https://github.com/jawaracloud/skopos/.github/workflows/release.yml@refs/tags/v0.1.0-alpha.2' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

cosign verify ghcr.io/jawaracloud/charts/skopos:0.1.0-alpha.2 \
  --certificate-identity 'https://github.com/jawaracloud/skopos/.github/workflows/release.yml@refs/tags/v0.1.0-alpha.2' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify oci://ghcr.io/jawaracloud/skopos-gateway:0.1.0-alpha.2 \
  --repo jawaracloud/skopos \
  --signer-workflow jawaracloud/skopos/.github/workflows/release.yml
```

Do not use `--insecure-ignore-tlog`. The release workflow always records
signatures in the transparency log.

## Install a release

The gateway is disabled by default. A release install uses the attached
`values.release.yaml`, which enables the gateway and pins its digest:

```sh
helm install skopos oci://ghcr.io/jawaracloud/charts/skopos \
  --version 0.1.0-alpha.2 \
  -f values.release.yaml \
  --namespace observability \
  --create-namespace
```

Download `values.release.yaml` from the GitHub release. Verify the OCI chart
before installing when your policy requires it.

## Local dry run

`make release VERSION=0.1.0-alpha.2` builds the image, chart, SBOMs, metadata,
and checksums locally. It does not push, sign, or create a release.

The repository no longer supports key-based signing or manual dispatch of a
release. Do not add either back.
