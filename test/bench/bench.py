#!/usr/bin/env python3
"""Measure Skopos gateway latency for representative Prometheus, Tempo, and
Loki API queries.

Uses only the Python standard library. Each query is warmed up, then timed
sequentially over a loopback HTTP connection. The response body is read before
the timer stops, so serialization and transfer time are included.
"""

import argparse
import json
import math
import statistics
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

QUERIES = [
    {
        "name": "prom_instant_count",
        "kind": "prom",
        "path": "/api/v1/query",
        "params": {"query": "sum(rate(http_requests_total[5m]))"},
    },
    {
        "name": "prom_instant_by_service",
        "kind": "prom",
        "path": "/api/v1/query",
        "params": {"query": "sum by (service_name) (rate(http_requests_total[5m]))"},
    },
    {
        "name": "prom_histogram_p95",
        "kind": "prom",
        "path": "/api/v1/query",
        "params": {
            "query": "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_bucket[5m])))"
        },
    },
    {
        "name": "prom_range_6h",
        "kind": "prom_range",
        "path": "/api/v1/query_range",
        "params": {
            "query": "sum(rate(http_requests_total[5m]))",
            "step": "60s",
            "minutes": 360,
        },
    },
    {
        "name": "prom_series",
        "kind": "prom",
        "path": "/api/v1/series",
        "params": {
            "match[]": 'http_requests_total{service_name="service-3"}',
            "minutes": 60,
        },
    },
    {
        "name": "prom_label_values",
        "kind": "prom",
        "path": "/api/v1/label/service_name/values",
        "params": {},
    },
    {
        "name": "tempo_search",
        "kind": "tempo",
        "path": "/api/search",
        "params": {
            "q": '{resource.service.name="service-3"}',
            "limit": "20",
            "minutes": 60,
        },
    },
    {
        "name": "tempo_trace_by_id",
        "kind": "tempo",
        "path": "/api/traces/{trace_id}",
        "params": {},
    },
    {
        "name": "tempo_tags",
        "kind": "tempo",
        "path": "/api/v2/search/tags",
        "params": {"minutes": 60},
    },
    {
        "name": "loki_query_range",
        "kind": "loki",
        "path": "/loki/api/v1/query_range",
        "params": {
            "query": '{service_name="service-3"} |= "failed"',
            "limit": "100",
            "minutes": 60,
        },
    },
]


def get_json(base_url, path, params):
    url = base_url.rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    started = time.perf_counter()
    with urllib.request.urlopen(req, timeout=120) as resp:
        body = resp.read()
    elapsed = time.perf_counter() - started
    return elapsed, len(body), json.loads(body)


def check(kind, payload):
    if payload.get("status") == "error":
        raise RuntimeError(payload.get("error") or "gateway returned an error")
    if kind in ("prom", "prom_range"):
        if payload.get("status") != "success":
            raise RuntimeError("unexpected Prometheus response")
    elif kind == "tempo":
        if "traces" in payload:
            return
        if "scopes" in payload:
            return
        if "resourceSpans" in payload:
            return
        raise RuntimeError("unexpected Tempo response")
    elif kind == "loki":
        if payload.get("status") != "success":
            raise RuntimeError("unexpected Loki response")


def percentile(values, pct):
    if not values:
        return 0.0
    ordered = sorted(values)
    idx = max(0, math.ceil(pct / 100.0 * len(ordered)) - 1)
    return ordered[idx]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:28082")
    parser.add_argument("--iterations", type=int, default=30)
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument("--json-out", default="")
    args = parser.parse_args()

    now = int(time.time())
    trace_id = ""
    try:
        _, _, search = get_json(
            args.base_url,
            "/api/search",
            {
                "q": '{resource.service.name="service-3"}',
                "start": str(now - 3600),
                "end": str(now),
                "limit": "1",
            },
        )
        traces = search.get("traces") or []
        if traces:
            trace_id = traces[0]["traceID"]
    except Exception as exc:  # noqa: BLE001 - benchmark should report the failure
        print(f"bench: could not resolve a trace ID: {exc}", file=sys.stderr)
        return 1
    if not trace_id:
        print("bench: no trace ID available for tempo_trace_by_id", file=sys.stderr)
        return 1

    results = []
    for query in QUERIES:
        path = query["path"].replace("{trace_id}", trace_id)
        params = dict(query["params"])
        minutes = params.pop("minutes", None)
        if minutes is not None:
            params["start"] = str(now - minutes * 60)
            params["end"] = str(now)

        for _ in range(args.warmup):
            get_json(args.base_url, path, params)

        samples = []
        body_bytes = 0
        for _ in range(args.iterations):
            elapsed, body_bytes, payload = get_json(args.base_url, path, params)
            check(query["kind"], payload)
            samples.append(elapsed * 1000.0)

        results.append(
            {
                "name": query["name"],
                "iterations": args.iterations,
                "p50_ms": round(percentile(samples, 50), 2),
                "p95_ms": round(percentile(samples, 95), 2),
                "p99_ms": round(percentile(samples, 99), 2),
                "min_ms": round(min(samples), 2),
                "max_ms": round(max(samples), 2),
                "mean_ms": round(statistics.fmean(samples), 2),
                "response_bytes": body_bytes,
            }
        )

    print("| Query | p50 ms | p95 ms | p99 ms | min ms | max ms | Response bytes |")
    print("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
    for row in results:
        print(
            "| `{name}` | {p50_ms} | {p95_ms} | {p99_ms} | {min_ms} | {max_ms} | {response_bytes} |".format(
                **row
            )
        )

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "base_url": args.base_url,
                    "iterations": args.iterations,
                    "warmup": args.warmup,
                    "trace_id": trace_id,
                    "results": results,
                },
                handle,
                indent=2,
            )
            handle.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
