#!/bin/sh
# Real Loki differential harness for the LogQL corpus.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
COMPOSE="docker compose -f $DIR/docker-compose.yml"
MODE=${1:-verify}

cleanup() {
	if [ "${KEEP_LOKI:-0}" != "1" ]; then
		$COMPOSE down --volumes --remove-orphans >/dev/null 2>&1 || true
	fi
}

case "$MODE" in
verify)
	$COMPOSE up -d --wait
	trap cleanup EXIT INT TERM
	python3 "$DIR/harness.py" verify
	;;
record)
	$COMPOSE up -d --wait
	trap cleanup EXIT INT TERM
	python3 "$DIR/harness.py" record
	;;
up)
	$COMPOSE up -d --wait
	;;
down)
	$COMPOSE down --volumes --remove-orphans
	;;
*)
	echo "usage: $0 [verify|record|up|down]" >&2
	exit 2
	;;
esac
