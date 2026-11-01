#!/bin/sh
# Generate load against the e2e target. OBI must already be attached.
set -eu

base=${SKOPOS_E2E_APP_URL:-http://127.0.0.1:18080}
requests=${SKOPOS_E2E_REQUESTS:-120}
workers=${SKOPOS_E2E_WORKERS:-8}
interval=${SKOPOS_E2E_REQUEST_INTERVAL:-0.75}

case "$requests" in
  ''|*[!0-9]*) echo "load: SKOPOS_E2E_REQUESTS must be a positive integer" >&2; exit 1 ;;
esac
if [ "$requests" -lt 1 ]; then
  echo "load: SKOPOS_E2E_REQUESTS must be a positive integer" >&2
  exit 1
fi

echo "load: sending $requests requests to $base with $workers workers"
# Spread the load over several OBI metric export intervals (5s each) so rate()
# queries have at least two samples to work with.
seq 1 "$requests" | xargs -P "$workers" -I{} sh -c '
  if [ $(( {} % 5 )) -eq 0 ]; then
    curl -fsS "$1/work?ms=10" >/dev/null
  else
    curl -fsS "$1/hello?name=user{}" >/dev/null
  fi
  sleep "$2"
' sh "$base" "$interval"
echo "load: done"
