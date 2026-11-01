#!/bin/sh
# Run the pinned OpenSSF Scorecard CLI against a private GitHub repository.
#
# The official ossf/scorecard-action only supports private repositories with
# GitHub Advanced Security (GHAS), because it uploads SARIF to code scanning.
# This repository is private on a GitHub Free organization, so this script uses
# the Scorecard CLI instead and stores JSON results under dist/ (git-ignored).
#
# Usage:
#   GITHUB_AUTH_TOKEN=... make scorecard
#   GITHUB_AUTH_TOKEN=... ./scripts/scorecard.sh
#
# The token only needs read access to the repository. A missing or unauthorized
# token makes GitHub return 404 for the private repository, which is why the
# explicit token check below matters.
#
# Pinned release: https://github.com/ossf/scorecard/releases/tag/v5.6.0
# To update, change SCORECARD_VERSION and every checksum together.

set -eu

SCORECARD_VERSION="v5.6.0"
SCORECARD_REPO="${SCORECARD_REPO:-github.com/jawaracloud/skopos}"
OUT_DIR="${SCORECARD_OUT_DIR:-dist/scorecard}"
TOOLS_DIR="${SCORECARD_TOOLS_DIR:-dist/tools}"

token="${GITHUB_AUTH_TOKEN:-${GITHUB_TOKEN:-${GH_TOKEN:-}}}"
if [ -z "$token" ]; then
	cat >&2 <<'EOF'
scorecard: no GitHub token found.

Set GITHUB_AUTH_TOKEN (preferred) to a token with read access to the private
repository, for example:

  GITHUB_AUTH_TOKEN="$(gh auth token)" make scorecard

GITHUB_TOKEN and GH_TOKEN are also accepted as fallbacks. Without a token,
GitHub returns 404 for the private repository and Scorecard cannot run.
EOF
	exit 1
fi
export GITHUB_AUTH_TOKEN="$token"

case "$(uname -s)-$(uname -m)" in
Linux-x86_64)
	os=linux
	arch=amd64
	sha256=19bde07129450cb65a95c6e3b9f34e034341579e6f0e241546ec3ae6efb2dd9a
	;;
Linux-aarch64 | Linux-arm64)
	os=linux
	arch=arm64
	sha256=3eacaceb3fdee6d0ba5df6ede45d1ada73321949029921bb63633802e8ec5951
	;;
Darwin-x86_64)
	os=darwin
	arch=amd64
	sha256=6eb92048bc537810d383d7e28233b2f118e78b6fef9e47b2c427c4d01fc096ad
	;;
Darwin-arm64)
	os=darwin
	arch=arm64
	sha256=1537183c0fcc6d8d1572aabc7a949091a36a2ee6be5bcb65f487cbb0a7b40381
	;;
*)
	echo "scorecard: unsupported platform: $(uname -s) $(uname -m)" >&2
	exit 1
	;;
esac

sha256_verify() {
	expected="$1"
	file="$2"
	if command -v sha256sum >/dev/null 2>&1; then
		printf '%s  %s\n' "$expected" "$file" | sha256sum -c - >/dev/null
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$file" | awk '{print $1}')"
		if [ "$actual" != "$expected" ]; then
			echo "scorecard: checksum mismatch for $file" >&2
			return 1
		fi
	else
		echo "scorecard: need sha256sum or shasum to verify the download" >&2
		return 1
	fi
}

command -v curl >/dev/null 2>&1 || {
	echo "scorecard: curl is required to download the pinned CLI" >&2
	exit 1
}

bin="$TOOLS_DIR/scorecard-${SCORECARD_VERSION}-${os}-${arch}"
if [ ! -x "$bin" ]; then
	mkdir -p "$TOOLS_DIR"
	archive="$TOOLS_DIR/scorecard-${SCORECARD_VERSION}-${os}-${arch}.tar.gz"
	url="https://github.com/ossf/scorecard/releases/download/${SCORECARD_VERSION}/scorecard_${SCORECARD_VERSION#v}_${os}_${arch}.tar.gz"
	echo "scorecard: downloading $url" >&2
	curl -fsSL "$url" -o "$archive"
	sha256_verify "$sha256" "$archive"
	tar -xzf "$archive" -C "$TOOLS_DIR" scorecard
	mv "$TOOLS_DIR/scorecard" "$bin"
	chmod +x "$bin"
	rm -f "$archive"
fi

mkdir -p "$OUT_DIR"
json="$OUT_DIR/results.json"
log="$OUT_DIR/scorecard.log"

echo "scorecard: scanning $SCORECARD_REPO with $SCORECARD_VERSION" >&2
if ! "$bin" --repo="$SCORECARD_REPO" --format=json --output="$json" \
	--show-details >"$log" 2>&1; then
	cat "$log" >&2
	echo "scorecard: run failed; log: $log" >&2
	exit 1
fi
cat "$log" >&2

# The CLI can exit 0 after reporting an unreachable private repository. Treat a
# missing result or a 404 as a token/access failure instead of a valid scan.
if [ ! -s "$json" ] || grep -q 'repo unreachable' "$log"; then
	echo "scorecard: no results for $SCORECARD_REPO; check token access" >&2
	exit 1
fi

printf 'scorecard JSON: %s\n' "$json"
if command -v jq >/dev/null 2>&1; then
	jq -r \
		'"scorecard " + .scorecard.version + " overall score: " + (.score | tostring),
		(.checks[] | "  " + .name + ": " + (.score | tostring) + "  " + .reason)' \
		"$json"
fi
