# Release process

The release path is intentionally separate from normal CI. Pull requests run
lint, tests, the ClickHouse integration test, and Helm rendering; they do not
need registry credentials or signing keys.

## What a release produces

[`scripts/release.sh`](../scripts/release.sh) writes to `dist/release/`:

- `skopos-<version>.tgz`: the packaged Helm chart, with the image version in
  `appVersion`.
- The gateway image `ghcr.io/<owner>/skopos-gateway:<version>`.
- `skopos-gateway-<version>.spdx.json` when `syft` is installed, otherwise
  `skopos-gateway-<version>.cdx.json` from the documented fallback.
- `skopos-chart-<version>.spdx.json` when `syft` is installed.
- `release-metadata.txt` and `SHA256SUMS`.
- Optional `.sig` files when `SIGN=1`.

The fallback SBOM is a CycloneDX 1.5 inventory built from the vendored Go module
graph and the SHA256 of every chart source file. It is deliberately conservative
and exists so a release is never blocked on a missing scanner. Install
[syft](https://github.com/anchore/syft) for a full image SBOM.

## Local dry run

```sh
make release VERSION=0.1.0-alpha.1          # builds image + chart + SBOM locally
make release-dry-run VERSION=0.1.0-alpha.1  # same, explicitly no push/sign
```

`release.sh` uses `docker build` and `helm package`. It does not push unless
`PUSH=1`, and it does not sign unless `SIGN=1`.

## GitHub release

Create and push a tag:

```sh
git tag v0.1.0-alpha.1
git push origin v0.1.0-alpha.1
```

`.github/workflows/release.yml` then:

1. Builds and packages the image and chart.
2. Pushes the image to GHCR and records its digest.
3. Generates SBOMs with pinned syft.
4. Optionally signs with cosign.
5. Uploads the workflow artifacts.
6. Creates a GitHub release with the chart, SBOMs, metadata, and checksums.

Manual runs use **Actions -> release -> Run workflow** with a version and an
optional push flag. A manual run without `push` still produces downloadable
artifacts, so maintainers can inspect a release before publishing.

## Optional signing

Signing is off by default. To enable it, set the repository variable
`COSIGN_SIGNING=true` and choose one of:

- **Keyless:** leave `COSIGN_PRIVATE_KEY` unset. The workflow uses the GitHub
  Actions OIDC identity (`id-token: write` is already set).
- **Key-based:** add the encrypted private key as the `COSIGN_PRIVATE_KEY`
  secret and its password as `COSIGN_PASSWORD`.

The script signs the pushed image digest and the chart package. Normal CI does
not require either secret.

## Published image digest

Release values must pin the gateway image by digest. Use the
`image_digest` line from `release-metadata.txt`:

```sh
helm install skopos ./deploy/helm/skopos \
  --namespace observability \
  --set gateway.image.repository=ghcr.io/<owner>/skopos-gateway \
  --set gateway.image.tag=0.1.0-alpha.1 \
  --set gateway.image.digest=sha256:...
```

The chart's default tag is for local development only. The release workflow
does not modify the chart's default values; the integrator or a follow-up
version bump owns that change.
