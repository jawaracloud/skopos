#!/usr/bin/env python3
"""Measure Skopos gateway latency for representative Prometheus, Tempo, and
Loki API queries.

Uses only the Python standard library. In the default sequential mode each
query is warmed up, then timed over a loopback HTTP connection. In concurrent
mode a pool of clients continuously issues a mixed PromQL/LogQL/TraceQL
workload for a fixed duration (or a number of passes). The response body is
read before the timer stops, so serialization and transfer time are included.
"""

import argparse
import json
import math
import re
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import Counter

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

DOCKER_STATS_FORMAT = "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"
DOCKER_STATS_RE = re.compile(r"^([0-9.]+)\s*([A-Za-z]+)$")
SIZE_UNITS = {
    "B": 1,
    "kB": 1000,
    "KB": 1000,
    "MB": 1000**2,
    "GB": 1000**3,
    "KiB": 1024,
    "MiB": 1024**2,
    "GiB": 1024**3,
    "TiB": 1024**4,
}


def get_json(base_url, path, params, timeout):
    url = base_url.rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = resp.read()
    return len(body), json.loads(body)


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


def build_request(query, trace_id, now):
    path = query["path"].replace("{trace_id}", trace_id)
    params = dict(query["params"])
    minutes = params.pop("minutes", None)
    if minutes is not None:
        params["start"] = str(now - minutes * 60)
        params["end"] = str(now)
    return path, params


def measure(base_url, query, path, params, timeout):
    """Issue one request and return (elapsed_ms, response_bytes, error)."""
    started = time.perf_counter()
    response_bytes = 0
    error = None
    try:
        response_bytes, payload = get_json(base_url, path, params, timeout)
        check(query["kind"], payload)
    except Exception as exc:  # noqa: BLE001 - the benchmark records failures
        error = exc
    return (time.perf_counter() - started) * 1000.0, response_bytes, error


def resolve_trace_id(base_url, timeout):
    now = int(time.time())
    _, search = get_json(
        base_url,
        "/api/search",
        {
            "q": '{resource.service.name="service-3"}',
            "start": str(now - 3600),
            "end": str(now),
            "limit": "1",
        },
        timeout,
    )
    traces = search.get("traces") or []
    return traces[0]["traceID"] if traces else ""


def latency_row(samples, requests, errors):
    return {
        "requests": requests,
        "errors": errors,
        "error_rate_pct": round(100.0 * errors / requests, 2) if requests else 0.0,
        "p50_ms": round(percentile(samples, 50), 2),
        "p95_ms": round(percentile(samples, 95), 2),
        "p99_ms": round(percentile(samples, 99), 2),
        "min_ms": round(min(samples), 2) if samples else 0.0,
        "max_ms": round(max(samples), 2) if samples else 0.0,
        "mean_ms": round(statistics.fmean(samples), 2) if samples else 0.0,
    }


def run_sequential(args, trace_id, now):
    results = []
    for query in QUERIES:
        path, params = build_request(query, trace_id, now)

        for _ in range(args.warmup):
            get_json(args.base_url, path, params, args.timeout)

        samples = []
        body_bytes = 0
        for _ in range(args.iterations):
            elapsed, body_bytes, error = measure(
                args.base_url, query, path, params, args.timeout
            )
            if error is not None:
                raise error
            samples.append(elapsed)

        results.append(
            {
                "name": query["name"],
                "kind": query["kind"],
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
                    "mode": "sequential",
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


class Recorder:
    def __init__(self, names):
        self._lock = threading.Lock()
        self._samples = {name: [] for name in names}
        self._requests = Counter()
        self._errors = Counter()
        self._error_messages = Counter()

    def record(self, name, elapsed_ms, error):
        with self._lock:
            self._samples[name].append(elapsed_ms)
            self._requests[name] += 1
            if error is not None:
                self._errors[name] += 1
                message = f"{type(error).__name__}: {error}"
                if len(message) > 160:
                    message = message[:157] + "..."
                self._error_messages[message] += 1

    def counts(self):
        with self._lock:
            return sum(self._requests.values()), sum(self._errors.values())

    def snapshot(self, names):
        with self._lock:
            samples = {name: list(self._samples[name]) for name in names}
            requests = {name: self._requests[name] for name in names}
            errors = {name: self._errors[name] for name in names}
            messages = Counter(self._error_messages)
        return samples, requests, errors, messages


def parse_size(text):
    text = text.strip().split("/")[0].strip()
    match = DOCKER_STATS_RE.match(text)
    if not match:
        return 0.0
    value, unit = match.groups()
    return float(value) * SIZE_UNITS.get(unit, 1)


class DockerStatsSampler(threading.Thread):
    """Sample CPU and memory for the benchmark containers once per interval."""

    def __init__(self, containers, interval):
        super().__init__(daemon=True)
        self._containers = list(containers)
        self._interval = max(0.2, interval)
        self._stop_event = threading.Event()
        self.samples = {}

    def run(self):
        while not self._stop_event.is_set():
            started = time.monotonic()
            try:
                proc = subprocess.run(
                    [
                        "docker",
                        "stats",
                        "--no-stream",
                        "--format",
                        DOCKER_STATS_FORMAT,
                        *self._containers,
                    ],
                    capture_output=True,
                    text=True,
                    timeout=15,
                    check=False,
                )
            except (OSError, subprocess.SubprocessError):
                proc = None
            if proc is not None and proc.returncode == 0:
                for line in proc.stdout.splitlines():
                    fields = line.split("\t")
                    if len(fields) != 3:
                        continue
                    name, cpu, memory = fields
                    try:
                        cpu_cores = float(cpu.rstrip("%")) / 100.0
                    except ValueError:
                        continue
                    self.samples.setdefault(name, []).append(
                        (cpu_cores, parse_size(memory))
                    )
            self._stop_event.wait(
                max(0.0, self._interval - (time.monotonic() - started))
            )

    def stop(self):
        self._stop_event.set()


def summarize_resources(samples):
    resources = []
    for name in sorted(samples):
        points = samples[name]
        if not points:
            continue
        cpu = [point[0] for point in points]
        memory = [point[1] for point in points]
        resources.append(
            {
                "container": name,
                "samples": len(points),
                "cpu_avg_cores": round(statistics.fmean(cpu), 2),
                "cpu_peak_cores": round(max(cpu), 2),
                "memory_avg_bytes": int(statistics.fmean(memory)),
                "memory_peak_bytes": int(max(memory)),
            }
        )
    return resources


def human_bytes(value):
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{value:.0f} {unit}"
        value /= 1024
    return f"{value:.1f} GiB"


def print_resources(resources):
    if not resources:
        return
    print()
    print("| Container | Samples | CPU avg (cores) | CPU peak (cores) | Memory avg | Memory peak |")
    print("| --- | ---: | ---: | ---: | ---: | ---: |")
    for row in resources:
        print(
            "| `{container}` | {samples} | {cpu_avg_cores} | {cpu_peak_cores} | {memory_avg} | {memory_peak} |".format(
                memory_avg=human_bytes(row["memory_avg_bytes"]),
                memory_peak=human_bytes(row["memory_peak_bytes"]),
                **row,
            )
        )


def run_concurrent(args, trace_id, now):
    names = [query["name"] for query in QUERIES]
    specs = []
    for query in QUERIES:
        path, params = build_request(query, trace_id, now)
        specs.append((query, path, params))

    # Warm the gateway and ClickHouse once before starting the client pool.
    for query, path, params in specs:
        for _ in range(args.warmup):
            get_json(args.base_url, path, params, args.timeout)

    recorder = Recorder(names)
    stop = threading.Event()
    deadline = time.monotonic() + args.duration if args.duration > 0 else None

    def worker(index):
        offset = index % len(specs)
        passes = 0
        while not stop.is_set():
            if deadline is not None and time.monotonic() >= deadline:
                return
            for step in range(len(specs)):
                if stop.is_set():
                    return
                if deadline is not None and time.monotonic() >= deadline:
                    return
                query, path, params = specs[(offset + step) % len(specs)]
                elapsed, _, error = measure(
                    args.base_url, query, path, params, args.timeout
                )
                recorder.record(query["name"], elapsed, error)
            passes += 1
            if deadline is None and passes >= args.iterations:
                return

    sampler = None
    if args.docker_stats and args.docker_containers:
        sampler = DockerStatsSampler(args.docker_containers, args.stats_interval)
        sampler.start()

    threads = [
        threading.Thread(target=worker, args=(index,), daemon=True)
        for index in range(args.concurrency)
    ]
    started = time.monotonic()
    for thread in threads:
        thread.start()

    next_progress = started + args.progress_interval
    try:
        while any(thread.is_alive() for thread in threads):
            time.sleep(0.2)
            if args.progress_interval > 0 and time.monotonic() >= next_progress:
                requests, errors = recorder.counts()
                print(
                    "bench: progress {:.0f}s requests={} errors={}".format(
                        time.monotonic() - started, requests, errors
                    ),
                    file=sys.stderr,
                )
                next_progress += args.progress_interval
    except KeyboardInterrupt:
        stop.set()
        for thread in threads:
            thread.join(timeout=10)
        raise

    for thread in threads:
        thread.join()
    elapsed = time.monotonic() - started
    stop.set()

    if sampler is not None:
        sampler.stop()
        sampler.join(timeout=20)

    samples, requests, errors, messages = recorder.snapshot(names)
    all_samples = [value for name in names for value in samples[name]]
    total_requests = sum(requests.values())
    total_errors = sum(errors.values())
    overall = latency_row(all_samples, total_requests, total_errors)
    overall["throughput_rps"] = (
        round(total_requests / elapsed, 2) if elapsed > 0 else 0.0
    )

    rows = []
    for query in QUERIES:
        row = latency_row(
            samples[query["name"]], requests[query["name"]], errors[query["name"]]
        )
        row["name"] = query["name"]
        row["kind"] = query["kind"]
        rows.append(row)

    print(
        "Concurrent mixed workload: {} clients, {} requests in {:.1f}s ({:.2f} req/s)".format(
            args.concurrency,
            total_requests,
            elapsed,
            overall["throughput_rps"],
        )
    )
    print(
        "Errors: {} ({:.2f}%)".format(total_errors, overall["error_rate_pct"])
    )
    print()
    print("| Scope | Requests | Errors | Error rate | p50 ms | p95 ms | p99 ms | mean ms |")
    print("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    print(
        "| overall | {requests} | {errors} | {error_rate_pct}% | {p50_ms} | {p95_ms} | {p99_ms} | {mean_ms} |".format(
            **overall
        )
    )
    print()
    print(
        "| Query | Kind | Requests | Errors | Error rate | p50 ms | p95 ms | p99 ms | mean ms |"
    )
    print("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    for row in rows:
        print(
            "| `{name}` | {kind} | {requests} | {errors} | {error_rate_pct}% | {p50_ms} | {p95_ms} | {p99_ms} | {mean_ms} |".format(
                **row
            )
        )

    resources = summarize_resources(sampler.samples) if sampler is not None else []
    print_resources(resources)

    if messages:
        print()
        print("Top errors:", file=sys.stderr)
        for message, count in messages.most_common(5):
            print(f"  {count}x {message}", file=sys.stderr)

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "base_url": args.base_url,
                    "mode": "concurrent",
                    "concurrency": args.concurrency,
                    "duration_seconds": args.duration,
                    "iterations": args.iterations if args.duration <= 0 else None,
                    "warmup": args.warmup,
                    "trace_id": trace_id,
                    "elapsed_seconds": round(elapsed, 2),
                    "total_requests": total_requests,
                    "errors": total_errors,
                    "error_rate_pct": overall["error_rate_pct"],
                    "throughput_rps": overall["throughput_rps"],
                    "overall": overall,
                    "queries": rows,
                    "resources": resources,
                    "errors_by_message": dict(messages),
                },
                handle,
                indent=2,
            )
            handle.write("\n")
    return 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:28082")
    parser.add_argument("--iterations", type=int, default=30)
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument(
        "--concurrency",
        type=int,
        default=1,
        help="number of concurrent mixed-workload clients (default: 1)",
    )
    parser.add_argument(
        "--duration",
        type=float,
        default=0.0,
        help="run the mixed workload for this many seconds; 0 uses --iterations passes per client",
    )
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument(
        "--progress-interval",
        type=float,
        default=10.0,
        help="seconds between progress lines (0 disables)",
    )
    parser.add_argument(
        "--docker-stats",
        action="store_true",
        help="sample CPU and memory with `docker stats` during concurrent mode",
    )
    parser.add_argument(
        "--docker-containers",
        nargs="+",
        default=[],
        help="container IDs or names to sample with --docker-stats",
    )
    parser.add_argument(
        "--stats-interval",
        type=float,
        default=1.0,
        help="seconds between docker stats samples",
    )
    parser.add_argument("--json-out", default="")
    args = parser.parse_args()

    if args.concurrency < 1:
        parser.error("--concurrency must be at least 1")
    if args.duration < 0:
        parser.error("--duration must not be negative")
    if args.warmup < 0:
        parser.error("--warmup must not be negative")
    if args.iterations < 1:
        parser.error("--iterations must be at least 1")

    now = int(time.time())
    try:
        trace_id = resolve_trace_id(args.base_url, args.timeout)
    except Exception as exc:  # noqa: BLE001 - benchmark should report the failure
        print(f"bench: could not resolve a trace ID: {exc}", file=sys.stderr)
        return 1
    if not trace_id:
        print("bench: no trace ID available for tempo_trace_by_id", file=sys.stderr)
        return 1

    if args.concurrency == 1 and args.duration <= 0:
        return run_sequential(args, trace_id, now)
    return run_concurrent(args, trace_id, now)


if __name__ == "__main__":
    raise SystemExit(main())
