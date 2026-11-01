#!/bin/sh
set -eu
cd "$(dirname "$0")/../../.."
TEMPO_COMPAT=1 go test ./test/compat/tempo -run TestDifferential -v "$@"
