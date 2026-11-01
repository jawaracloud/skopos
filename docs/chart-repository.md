# Helm chart repository

Skopos publishes its Helm chart through a GitHub Pages chart repository:

```text
https://jawaracloud.github.io/skopos/
```

The repository is generated from `deploy/helm/skopos` by the
`.github/workflows/chart-repo.yml` workflow. The published `gh-pages` branch
contains `index.yaml` and the packaged `.tgz` charts. **Do not edit that branch
by hand.** Change the chart in `main`, bump `Chart.yaml` `version`, merge, and
let the workflow publish it.

## Use the repository

```sh
helm repo add skopos https://jawaracloud.github.io/skopos/
helm repo update
helm search repo skopos --devel
```

Install the data plane first. The gateway image is not published yet, so the
default chart install needs either `gateway.enabled=false` or a locally built
gateway image:

```sh
helm install skopos skopos/skopos \
  --devel \
  --namespace observability \
  --create-namespace \
  --set gateway.enabled=false
```

When the gateway image is published, omit `gateway.enabled=false` and set the
image repository, tag, and digest from the release.

Upgrade an existing install with the normal Helm flow:

```sh
helm repo update
helm upgrade skopos skopos/skopos --namespace observability
```

## Release a new chart version

1. Change the chart in `deploy/helm/skopos`.
2. Bump `version` in `deploy/helm/skopos/Chart.yaml`. Chart-releaser uses this
   version as the release and package name.
3. Update `appVersion` only when the application version changes.
4. Run `helm lint deploy/helm/skopos` and `helm template` locally.
5. Merge to `main`. The `chart-repo` workflow packages the chart, creates the
   chart release, and updates the `gh-pages` index.
6. Verify the repository:

   ```sh
   helm repo update
   helm search repo skopos --devel --versions
   ```

## Why a gh-pages branch and not a separate chart source repo

The chart is tightly coupled to the gateway configuration, ClickHouse schema,
and the same release cadence. Keeping the source in this repository avoids a
second schema/values copy drifting from the application. `gh-pages` is a
**publishing surface**, not a second source of truth.

The workflow uses `helm/chart-releaser-action` pinned to a commit. It publishes
charted packages with `packages_with_index: true`, so `helm repo add` can fetch
`.tgz` files directly from Pages. Release assets are also created by the
action.

## Security notes

- The workflow token only has `contents: write`; it does not need packages or
  id-token permissions.
- The chart release asset is keyless-signed with cosign. The release includes
  a detached `.sig` and the Fulcio `.pem` certificate. Verify it with:

  ```sh
  cosign verify-blob \
    --signature skopos-0.1.0-alpha.1.tgz.sig \
    --certificate skopos-0.1.0-alpha.1.tgz.pem \
    --certificate-identity-regexp 'https://github.com/jawaracloud/skopos/.github/workflows/chart-repo.yml@.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --insecure-ignore-tlog \
    skopos-0.1.0-alpha.1.tgz
  ```

  The signature skips the Rekor transparency log so re-running the chart
  workflow does not conflict with an existing equivalent log entry. The Fulcio
  certificate still binds the signature to the workflow identity.

  The signature covers the release asset; the chart repository serves the same
  package bytes from `gh-pages`.
- The repository URL is public and read-only. It contains no secrets.
