#!/bin/sh
# Build release artifacts for Skopos:
#   - gateway container image (optionally pushed)
#   - packaged Helm chart
#   - SBOMs (syft when available, documented Go/chart inventory fallback)
#   - SHA256SUMS and release metadata
#   - optional cosign signatures
#
# Usage:
#   VERSION=0.1.0-alpha.1 ./scripts/release.sh
#   VERSION=0.1.0-alpha.1 PUSH=1 IMAGE_REPOSITORY=ghcr.io/me/skopos-gateway ./scripts/release.sh
#   VERSION=0.1.0-alpha.1 SIGN=1 COSIGN_KEY=/path/cosign.key ./scripts/release.sh
#
# Exit codes are non-zero on any failed verification. Normal CI does not run
# this script; the release workflow invokes it only for tags or manual runs.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
chart="$root/deploy/helm/skopos"

version=${VERSION:-}
if [ -z "$version" ]; then
  version=$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
fi
version=${version#v}
chart_version=${CHART_VERSION:-$version}
image_repository=${IMAGE_REPOSITORY:-ghcr.io/mrofisr/skopos-gateway}
image="$image_repository:$version"
output_dir=${OUTPUT_DIR:-$root/dist/release}
push=${PUSH:-0}
sign=${SIGN:-0}

fail() {
  echo "release: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

case "$version" in
  ''|.|..|-*|.*|*[!0-9A-Za-z_.-]*)
    fail "VERSION must be a Docker-tag-safe version: $version"
    ;;
esac
case "$chart_version" in
  ''|*[!0-9A-Za-z_.+-]*)
    fail "CHART_VERSION must contain only Helm-safe characters: $chart_version"
    ;;
esac

need docker
need helm
need sha256sum
[ -f "$chart/Chart.yaml" ] || fail "chart not found at $chart"

mkdir -p "$output_dir"

echo "release: version=$version chart_version=$chart_version image=$image"

echo "release: linting and rendering the chart"
helm lint "$chart"
helm template skopos "$chart" --namespace observability >/dev/null

echo "release: packaging chart"
helm package "$chart" \
  --version "$chart_version" \
  --app-version "$version" \
  --destination "$output_dir"
chart_package="$output_dir/skopos-$chart_version.tgz"
[ -f "$chart_package" ] || fail "helm package did not produce $chart_package"

echo "release: building gateway image"
docker build \
  --build-arg "VERSION=$version" \
  -t "$image" \
  "$root"

digest=""
if [ "$push" = "1" ]; then
  echo "release: pushing $image"
  push_output=$(docker push "$image")
  printf '%s\n' "$push_output"
  digest=$(printf '%s\n' "$push_output" | sed -n 's/^digest: //p' | tail -n 1)
  if [ -z "$digest" ]; then
    digest=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" \
      | grep "^$image_repository@sha256:" | head -n 1 | cut -d@ -f2 || true)
  fi
  [ -n "$digest" ] || fail "could not determine the pushed image digest"
else
  digest=$(docker image inspect --format '{{.Id}}' "$image")
fi

echo "release: generating SBOMs"
if command -v syft >/dev/null 2>&1; then
  syft "$image" -o "spdx-json=$output_dir/skopos-gateway-$version.spdx.json"
  syft "dir:$chart" -o "spdx-json=$output_dir/skopos-chart-$chart_version.spdx.json"
else
  echo "release: syft not found; using the documented Go/chart inventory fallback"
  "$root/scripts/sbom-fallback.sh" \
    "$image" "$version" "$output_dir/skopos-gateway-$version.cdx.json" "$root"
fi

cat >"$output_dir/release-metadata.txt" <<EOF
name=skopos
version=$version
chart_version=$chart_version
image=$image
image_digest=$digest
pushed=$push
signed=$sign
commit=$(git -C "$root" rev-parse HEAD 2>/dev/null || echo unknown)
created_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

if [ "$sign" = "1" ]; then
  command -v cosign >/dev/null 2>&1 || fail "SIGN=1 requires cosign"
  [ "$push" = "1" ] || fail "SIGN=1 requires PUSH=1 so the image has a digest"
  [ -n "$digest" ] || fail "SIGN=1 requires an image digest"

  if [ -n "${COSIGN_KEY:-}" ]; then
    [ -f "$COSIGN_KEY" ] || fail "COSIGN_KEY does not exist: $COSIGN_KEY"
    echo "release: signing image and chart with key $COSIGN_KEY"
    cosign sign --yes --key "$COSIGN_KEY" "$image_repository@$digest"
    cosign sign-blob --yes --key "$COSIGN_KEY" \
      --output-signature "$chart_package.sig" \
      "$chart_package"
  else
    echo "release: signing image and chart keylessly (ambient OIDC required)"
    cosign sign --yes "$image_repository@$digest"
    cosign sign-blob --yes \
      --output-signature "$chart_package.sig" \
      "$chart_package"
  fi
fi

# Checksums cover the chart and every SBOM. Signatures are produced after the
# artifacts they cover and are intentionally not checksummed themselves.
(
  cd "$output_dir"
  sha256sum ./*.tgz ./*.spdx.json ./*.cdx.json ./release-metadata.txt 2>/dev/null >SHA256SUMS || true
)
[ -s "$output_dir/SHA256SUMS" ] || fail "failed to write SHA256SUMS"

echo "release: artifacts in $output_dir"
ls -l "$output_dir"
echo "release: done"
