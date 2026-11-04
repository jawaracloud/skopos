#!/bin/sh
# Build release artifacts for Skopos:
#   - gateway container image (local build only)
#   - packaged Helm chart
#   - SBOMs (syft when available, documented Go/chart inventory fallback)
#   - SHA256SUMS and release metadata
#
# Publishing, signing, and attestation happen in .github/workflows/release.yml.
#
# Usage:
#   VERSION=0.1.0-alpha.1 ./scripts/release.sh
#   VERSION=0.1.0-alpha.1 OUTPUT_DIR=dist/release ./scripts/release.sh
#
# This script never pushes to a registry and never reads signing keys.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
chart="$root/deploy/helm/skopos"

version=${VERSION:-}
if [ -z "$version" ]; then
  version=$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
fi
version=${version#v}
chart_version=${CHART_VERSION:-$version}
image_repository=${IMAGE_REPOSITORY:-ghcr.io/jawaracloud/skopos-gateway}
image="$image_repository:$version"
output_dir=${OUTPUT_DIR:-$root/dist/release}

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
image_id=$(docker image inspect --format '{{.Id}}' "$image")

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
image_id=$image_id
commit=$(git -C "$root" rev-parse HEAD 2>/dev/null || echo unknown)
created_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

# Checksums cover the chart, SBOMs, and metadata. The image tar and signatures
# are produced later by the release workflow and are intentionally excluded.
(
  cd "$output_dir"
  sha256sum ./*.tgz ./*.spdx.json ./*.cdx.json ./release-metadata.txt 2>/dev/null >SHA256SUMS || true
)
[ -s "$output_dir/SHA256SUMS" ] || fail "failed to write SHA256SUMS"

echo "release: artifacts in $output_dir"
ls -l "$output_dir"
echo "release: done"
