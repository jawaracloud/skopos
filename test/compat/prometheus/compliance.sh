#!/usr/bin/env bash
# Run the official Prometheus PromQL compliance suite against the Skopos
# evaluator. The upstream source is fetched once into a cache directory and
# verified against the pinned commit and archive checksum below.
set -euo pipefail

PROMETHEUS_RELEASE="3.15.1"
PROMETHEUS_COMMIT="bae3eced0a5369e050e83ea73bc381d7c4b1a719"
PROMETHEUS_SHA256="4636cc523bda30bdf92bf7b05b4491bb3d3038d89ebb3223c7b9a71f23e9e9c9"
PROMETHEUS_ARCHIVE="prometheus-${PROMETHEUS_COMMIT}.tar.gz"
PROMETHEUS_URL="https://github.com/prometheus/prometheus/archive/${PROMETHEUS_COMMIT}.tar.gz"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE_ROOT="${SKOPOS_PROMQL_COMPLIANCE_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/skopos/promql-compliance}"
SRC_DIR="${CACHE_ROOT}/prometheus-${PROMETHEUS_COMMIT}"
REPORT="${CACHE_ROOT}/compliance-report.md"
RESULTS="${CACHE_ROOT}/compliance-go-test.json"
FILES=""

usage() {
  cat <<EOF
Usage: $0 [--files file1.test,file2.test] [--report path.md]

Runs the official Prometheus ${PROMETHEUS_RELEASE} PromQL compliance scripts
(commit ${PROMETHEUS_COMMIT}) against internal/prom. Without --files the full
suite runs. --report writes a Markdown summary (default: ${REPORT}).

Environment:
  SKOPOS_PROMQL_COMPLIANCE_CACHE  override the downloaded-source cache
  PROMETHEUS_COMPLIANCE_FILES     same as --files
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --files)
      FILES="${2:?--files needs a value}"
      shift 2
      ;;
    --report)
      REPORT="${2:?--report needs a value}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done
FILES="${FILES:-${PROMETHEUS_COMPLIANCE_FILES:-}}"

sha256_check() {
  local file="$1" expected="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "${expected}" "${file}" | sha256sum -c -
  else
    local actual
    actual="$(shasum -a 256 "${file}" | awk '{print $1}')"
    [[ "${actual}" == "${expected}" ]] || {
      echo "checksum mismatch for ${file}: got ${actual}, want ${expected}" >&2
      return 1
    }
    echo "${file}: OK"
  fi
}

fetch_source() {
  mkdir -p "${CACHE_ROOT}"
  local tmp
  tmp="$(mktemp -d "${CACHE_ROOT}/download.XXXXXX")"
  echo "downloading Prometheus ${PROMETHEUS_RELEASE} (${PROMETHEUS_COMMIT})" >&2
  curl -fsSL --retry 3 --retry-delay 2 -o "${tmp}/${PROMETHEUS_ARCHIVE}" "${PROMETHEUS_URL}"
  sha256_check "${tmp}/${PROMETHEUS_ARCHIVE}" "${PROMETHEUS_SHA256}"
  tar -xzf "${tmp}/${PROMETHEUS_ARCHIVE}" -C "${tmp}"
  local extracted="${tmp}/prometheus-${PROMETHEUS_COMMIT}"
  [[ -d "${extracted}/promql/promqltest/testdata" ]] || {
    echo "archive does not contain promql/promqltest/testdata" >&2
    exit 1
  }
  [[ "$(cat "${extracted}/VERSION")" == "${PROMETHEUS_RELEASE}" ]] || {
    echo "archive VERSION does not match ${PROMETHEUS_RELEASE}" >&2
    exit 1
  }
  rm -rf "${SRC_DIR}"
  mv "${extracted}" "${SRC_DIR}"
  rm -rf "${tmp}"
}

if [[ ! -d "${SRC_DIR}/promql/promqltest/testdata" ]]; then
  fetch_source
fi

[[ "$(cat "${SRC_DIR}/VERSION")" == "${PROMETHEUS_RELEASE}" ]] || {
  echo "cached source VERSION mismatch under ${SRC_DIR}; remove it and retry" >&2
  exit 1
}

echo "Prometheus ${PROMETHEUS_RELEASE} @ ${PROMETHEUS_COMMIT}" >&2
echo "suite: ${SRC_DIR}/promql/promqltest/testdata" >&2

cd "${SCRIPT_DIR}"
set +e
PROMETHEUS_COMPLIANCE_TESTDATA="${SRC_DIR}/promql/promqltest/testdata" \
PROMETHEUS_COMPLIANCE_FILES="${FILES}" \
  go test ./compliance -run '^TestOfficialPromQLCompliance$' -count=1 -json \
  2> >(tee "${CACHE_ROOT}/compliance-stderr.log" >&2) \
  > "${RESULTS}"
test_status=$?
set -e

go run ./compliance/cmd/compliance-report \
  -input "${RESULTS}" \
  -policy ./compliance/exceptions.json \
  -markdown "${REPORT}" || report_status=$?
report_status="${report_status:-0}"

if [[ ${test_status} -ne 0 && ${report_status} -eq 0 ]]; then
  # The report only exits 0 when every failing case is an expected
  # unsupported case; the non-zero go test status is then expected.
  :
fi
echo "report: ${REPORT}" >&2
echo "raw results: ${RESULTS}" >&2
exit "${report_status}"
