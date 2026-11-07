# PromQL compliance suite provenance

The compliance gate runs the official Prometheus PromQL test scripts from a
pinned source archive. The script verifies the archive checksum before use.

| Field | Value |
| --- | --- |
| Project | [prometheus/prometheus](https://github.com/prometheus/prometheus) |
| Release | `3.15.1` |
| Tag | `v3.15.1` |
| Git commit | `bae3eced0a5369e050e83ea73bc381d7c4b1a719` |
| Archive | `https://github.com/prometheus/prometheus/archive/bae3eced0a5369e050e83ea73bc381d7c4b1a719.tar.gz` |
| Archive SHA-256 | `4636cc523bda30bdf92bf7b05b4491bb3d3038d89ebb3223c7b9a71f23e9e9c9` |
| Suite path | `promql/promqltest/testdata/*.test` |
| License | Apache-2.0 |

The Go module `github.com/prometheus/prometheus` is pinned to `v0.315.1` in
`test/compat/prometheus/go.mod`; that module version corresponds to release
3.15.1, so the harness and the test scripts come from the same tag.

The fixed variables are defined at the top of
`test/compat/prometheus/compliance.sh`. Update all of them together when
bumping the suite; `compliance.sh` refuses to use a cached checkout whose
`VERSION` does not match the pinned release.
