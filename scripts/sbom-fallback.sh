#!/bin/sh
# Documented SBOM fallback for hosts without syft.
#
# Produces a CycloneDX 1.5 inventory from the Go module graph and the SHA256
# hashes of every chart source file. It is deliberately conservative: package
# licenses and file-level transitive analysis are marked NOASSERTION/unknown.
# Install syft for a full image SBOM with OS and language package metadata.
set -eu

image=${1:?usage: sbom-fallback.sh IMAGE VERSION OUTPUT ROOT}
version=${2:?}
output=${3:?}
root=${4:?}

command -v python3 >/dev/null 2>&1 || {
  echo "sbom-fallback: python3 is required for the CycloneDX fallback" >&2
  exit 1
}
if [ -f "$root/vendor/modules.txt" ]; then
  modules=$(sed -n 's/^# \([^ ]*\) \([^ ]*\)$/\1|\2/p' "$root/vendor/modules.txt")
else
  command -v go >/dev/null 2>&1 || {
    echo "sbom-fallback: go is required when the vendor tree is absent" >&2
    exit 1
  }
  modules=$(cd "$root" && go list -mod=mod -m -f '{{.Path}}|{{.Version}}' all)
fi
chart_dir="$root/deploy/helm/skopos"

MODULES="$modules" CHART_DIR="$chart_dir" IMAGE="$image" VERSION="$version" OUTPUT="$output" \
python3 - <<'PY'
import hashlib
import json
import os
import pathlib
import uuid
from datetime import datetime, timezone

image = os.environ["IMAGE"]
version = os.environ["VERSION"]
output = os.environ["OUTPUT"]
chart_dir = pathlib.Path(os.environ["CHART_DIR"])

components = []
seen = set()
for line in os.environ["MODULES"].splitlines():
    path, _, mod_version = line.partition("|")
    if not path or not mod_version:
        continue
    ref = f"pkg:golang/{path}@{mod_version}"
    if ref in seen:
        continue
    seen.add(ref)
    components.append(
        {
            "type": "library",
            "bom-ref": ref,
            "name": path,
            "version": mod_version,
            "purl": ref,
            "licenses": [{"license": {"name": "NOASSERTION"}}],
        }
    )

for path in sorted(chart_dir.rglob("*")):
    if not path.is_file():
        continue
    rel = path.relative_to(chart_dir).as_posix()
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    components.append(
        {
            "type": "file",
            "bom-ref": f"chart:deploy/helm/skopos/{rel}",
            "name": f"deploy/helm/skopos/{rel}",
            "hashes": [{"alg": "SHA-256", "content": digest}],
        }
    )

document = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "serialNumber": f"urn:uuid:{uuid.uuid4()}",
    "version": 1,
    "metadata": {
        "timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "tools": [{"vendor": "Skopos", "name": "sbom-fallback.sh", "version": "1"}],
        "component": {
            "type": "container",
            "bom-ref": f"container:{image}",
            "name": image,
            "version": version,
        },
        "properties": [
            {"name": "skopos:sbom:generator", "value": "go-module-inventory-fallback"},
            {"name": "skopos:sbom:note", "value": "Install syft for a full image SBOM."},
        ],
    },
    "components": components,
}

with open(output, "w", encoding="utf-8") as handle:
    json.dump(document, handle, indent=2)
    handle.write("\n")
print(f"sbom-fallback: wrote {output} ({len(components)} components)")
PY
