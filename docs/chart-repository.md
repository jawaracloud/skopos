# Helm chart distribution

Skopos publishes its Helm chart as an OCI artifact in GitHub Container Registry:

```text
oci://ghcr.io/jawaracloud/charts/skopos
```

The chart is released from the same `vX.Y.Z` tag as the gateway image and is
signed with keyless Sigstore. The chart tag is immutable in practice because
each release uses a new version and consumers should pin the OCI digest.

## Use the chart

```sh
helm install skopos oci://ghcr.io/jawaracloud/charts/skopos \
  --version 0.1.0-alpha.2 \
  -f values.release.yaml \
  --namespace observability \
  --create-namespace
```

Download `values.release.yaml` from the matching GitHub release. It enables the
gateway and pins the image digest.

Verify the chart before installing:

```sh
cosign verify ghcr.io/jawaracloud/charts/skopos:0.1.0-alpha.2 \
  --certificate-identity 'https://github.com/jawaracloud/skopos/.github/workflows/release.yml@refs/tags/v0.1.0-alpha.2' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Release a new chart version

1. Change the chart in `deploy/helm/skopos`.
2. Set `version` and `appVersion` in `Chart.yaml` to the next `vX.Y.Z` version.
3. Merge through a pull request with all required checks.
4. Create and push the annotated tag as described in
   [release.md](release.md).
5. The release workflow packages, pushes, signs, and attests the chart.

There is no chart release from a push to `main` and no `gh-pages` publishing
job.

## Legacy GitHub Pages repository

`https://jawaracloud.github.io/skopos/` is the legacy chart repository created by
`skopos-0.1.0-alpha.1`. It predates the OCI release process and is not covered
by the current signing and provenance guarantees. Do not add it to new
installations. The old `gh-pages` branch is kept read-only for historical
reference and is not updated.

## Why OCI

- The image and chart come from the same tag and commit.
- The chart is addressed by an immutable digest.
- Signing happens before the GitHub release is published.
- The release workflow uses one registry and one credential model.
- There is no separate `gh-pages` write path or chart-releaser action.

## Security notes

- The release workflow signs the OCI chart digest with keyless Sigstore and
  records the signature in the transparency log.
- The chart SBOM and build provenance are attached with GitHub artifact
  attestations.
- Consumers should pin a version and verify the signature. Do not use a mutable
  `latest` reference.
