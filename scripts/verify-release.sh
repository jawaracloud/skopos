#!/bin/sh
# Verify a published Skopos release.
#
# Usage:
#   VERSION=0.1.0-alpha.2 ./scripts/verify-release.sh
#   ./scripts/verify-release.sh 0.1.0-alpha.2
#
# Requires: cosign, gh (authenticated), helm.
set -eu

version=${VERSION:-${1:-}}
if [ -z "$version" ]; then
  echo "usage: VERSION=<version> $0 [version]" >&2
  exit 1
fi
version=${version#v}

repo=${REPO:-jawaracloud/skopos}
image_repository=${IMAGE_REPOSITORY:-ghcr.io/jawaracloud/skopos-gateway}
chart_ref=${CHART_REF:-ghcr.io/jawaracloud/charts/skopos}
values_file=${VALUES_FILE:-}

for tool in cosign gh helm; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "verify-release: missing required command: $tool" >&2
    exit 1
  }
done

identity="https://github.com/${repo}/.github/workflows/release.yml@refs/tags/v${version}"
issuer="https://token.actions.githubusercontent.com"

# Prefer immutable digests when the caller has them. Fall back to the release
# tag for a quick check.
image_ref="${image_repository}:${version}"
if [ -n "${IMAGE_DIGEST:-}" ]; then
  image_ref="${image_repository}@${IMAGE_DIGEST}"
fi
chart_ref_full="${chart_ref}:${version}"
if [ -n "${CHART_DIGEST:-}" ]; then
  chart_ref_full="${chart_ref}@${CHART_DIGEST}"
fi

echo "verify-release: image ${image_ref}"
cosign verify "$image_ref" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer"

echo "verify-release: chart ${chart_ref_full}"
cosign verify "$chart_ref_full" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer"

echo "verify-release: image provenance"
gh attestation verify "oci://${image_ref}" \
  --repo "$repo" \
  --signer-workflow "${repo}/.github/workflows/release.yml"

echo "verify-release: chart provenance"
gh attestation verify "oci://${chart_ref_full}" \
  --repo "$repo" \
  --signer-workflow "${repo}/.github/workflows/release.yml"

if [ -n "$values_file" ]; then
  echo "verify-release: render chart with ${values_file}"
  helm template skopos "oci://${chart_ref}" \
    --version "$version" \
    --namespace observability \
    -f "$values_file" >/dev/null
else
  echo "verify-release: render chart with default values"
  helm template skopos "oci://${chart_ref}" \
    --version "$version" \
    --namespace observability >/dev/null
fi

echo "verify-release: ok"
