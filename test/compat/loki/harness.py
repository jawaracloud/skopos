#!/usr/bin/env python3
"""Deterministic LogQL differential harness.

The corpus in test/corpus/logql/corpus.json is the shared contract. This
script pushes its entries to a real Loki container and compares Loki's
normalized results with the corpus expectations. The Go corpus test compares
the Skopos Loki head with the same expectations.

Set GATEWAY_URL to additionally compare a running, seeded Skopos gateway with
Loki. See README.md for the exact commands.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import time
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
CORPUS = os.path.join(ROOT, "test", "corpus", "logql", "corpus.json")
DROP_LABELS = {"detected_level", "__error__", "__error_details__"}


def http_json(url, data=None, method=None, timeout=30):
    headers = {}
    body = None
    if data is not None:
        body = json.dumps(data).encode()
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=body, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        payload = response.read()
        if not payload:
            return None
        return json.loads(payload)


def wait_ready(base, timeout=90):
    deadline = time.time() + timeout
    url = base.rstrip("/") + "/ready"
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:
            pass
        time.sleep(1)
    raise SystemExit(f"Loki did not become ready at {url}")


def parse_rfc3339(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def to_nanos(value):
    parsed = parse_rfc3339(value)
    return int(parsed.timestamp() * 1_000_000_000)


def ns_to_rfc3339(value):
    seconds, nanos = divmod(int(value), 1_000_000_000)
    stamp = dt.datetime.fromtimestamp(seconds, tz=dt.timezone.utc)
    return stamp.strftime("%Y-%m-%dT%H:%M:%S") + f".{nanos:09d}Z"


def push_entries(base, corpus):
    streams = {}
    for entry in corpus["entries"]:
        labels = dict(entry["labels"])
        key = tuple(sorted(labels.items()))
        streams.setdefault(key, {"stream": labels, "values": []})
        streams[key]["values"].append([str(to_nanos(entry["timestamp"])), entry["body"]])
    payload = {"streams": list(streams.values())}
    http_json(base.rstrip("/") + "/loki/api/v1/push", payload)


def request_params(case):
    params = {
        "query": case["query"],
        "start": case["start"],
        "end": case["end"],
        "limit": "5000",
        "direction": "forward",
    }
    if case.get("step"):
        params["step"] = case["step"]
    return params


def query_loki(base, case):
    params = request_params(case)
    if case["kind"] == "log" or case.get("step"):
        path = "/loki/api/v1/query_range"
    else:
        path = "/loki/api/v1/query"
        params["time"] = case["end"]
        params.pop("start", None)
        params.pop("end", None)
        params.pop("step", None)
        params.pop("direction", None)
        params.pop("limit", None)
    url = base.rstrip("/") + path + "?" + urllib.parse.urlencode(params)
    return http_json(url)


def query_gateway(base, case):
    params = request_params(case)
    if case["kind"] == "log" or case.get("step"):
        path = "/loki/api/v1/query_range"
    else:
        path = "/loki/api/v1/query"
        params = {"query": case["query"], "time": case["end"]}
    url = base.rstrip("/") + path + "?" + urllib.parse.urlencode(params)
    return http_json(url)


def normalized_labels(labels):
    return {key: value for key, value in labels.items() if key not in DROP_LABELS}


def normalize_streams(response):
    result = response.get("data", {}).get("result", [])
    lines = []
    for stream in result:
        labels = normalized_labels(stream.get("stream", {}))
        for value in stream.get("values", []):
            lines.append({"timestamp": int(value[0]), "body": value[1], "labels": labels})
    lines.sort(key=lambda line: line["timestamp"])
    return lines


def normalize_metric(response):
    data = response.get("data", {})
    result = data.get("result", [])
    series = []
    for item in result:
        metric = normalized_labels(item.get("metric", {}))
        if "values" in item:
            values = [[float(pair[0]), str(pair[1])] for pair in item["values"]]
        else:
            values = [[float(item["value"][0]), str(item["value"][1])]]
        values.sort(key=lambda pair: pair[0])
        series.append({"metric": metric, "values": values})
    series.sort(key=lambda item: json.dumps(item["metric"], sort_keys=True))
    return series


def compare_log(case, response):
    expected = case.get("expected_logs", [])
    actual = normalize_streams(response)
    if len(actual) != len(expected):
        return f"matched {len(actual)} lines, want {len(expected)}: {actual}"
    expected.sort(key=lambda item: to_nanos(item["timestamp"]))
    for index, want in enumerate(expected):
        want_ns = to_nanos(want["timestamp"])
        if actual[index]["timestamp"] != want_ns:
            return f"line {index} timestamp {ns_to_rfc3339(actual[index]['timestamp'])}, want {want['timestamp']}"
        for name, value in want.get("labels", {}).items():
            if actual[index]["labels"].get(name) != value:
                return f"line {index} label {name}={actual[index]['labels'].get(name)!r}, want {value!r}"
    return None


def compare_metric(case, response):
    expected = []
    for item in case.get("expected", []):
        values = [[float(pair[0]), str(pair[1])] for pair in item.get("values", [])]
        values.sort(key=lambda pair: pair[0])
        expected.append({"metric": normalized_labels(item.get("metric", {})), "values": values})
    expected.sort(key=lambda item: json.dumps(item["metric"], sort_keys=True))
    actual = normalize_metric(response)
    if actual != expected:
        return f"metric result\n  got:  {json.dumps(actual, sort_keys=True)}\n  want: {json.dumps(expected, sort_keys=True)}"
    return None


def compare(case, response):
    if case["kind"] == "log":
        return compare_log(case, response)
    return compare_metric(case, response)


def record_case(case, response):
    if case["kind"] == "log":
        actual = normalize_streams(response)
        case["expected_logs"] = [
            {"timestamp": ns_to_rfc3339(line["timestamp"]), "labels": line["labels"]}
            for line in actual
        ]
        return
    actual = normalize_metric(response)
    expected = []
    for item in actual:
        values = []
        for timestamp, value in item["values"]:
            values.append([int(timestamp) if timestamp.is_integer() else timestamp, value])
        expected.append({"metric": item["metric"], "values": values})
    case["expected"] = expected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["verify", "record"], nargs="?", default="verify")
    parser.add_argument("--corpus", default=CORPUS)
    parser.add_argument("--loki-url", default=os.environ.get("LOKI_URL", "http://127.0.0.1:13100"))
    parser.add_argument("--gateway-url", default=os.environ.get("GATEWAY_URL", ""))
    args = parser.parse_args()

    with open(args.corpus, encoding="utf-8") as handle:
        corpus = json.load(handle)

    wait_ready(args.loki_url)
    push_entries(args.loki_url, corpus)
    time.sleep(2)

    failures = []
    for case in corpus["cases"]:
        response = query_loki(args.loki_url, case)
        if args.mode == "record":
            record_case(case, response)
            continue
        problem = compare(case, response)
        if problem:
            failures.append((case["name"], problem))
            continue
        if args.gateway_url:
            gateway_response = query_gateway(args.gateway_url, case)
            gateway_problem = compare(case, gateway_response)
            if gateway_problem:
                failures.append((case["name"] + " (gateway)", gateway_problem))

    if args.mode == "record":
        with open(args.corpus, "w", encoding="utf-8") as handle:
            json.dump(corpus, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
        print(f"recorded {len(corpus['cases'])} cases from Loki into {args.corpus}")
        return

    if failures:
        for name, problem in failures:
            print(f"FAIL {name}: {problem}", file=sys.stderr)
        raise SystemExit(1)
    suffix = " and the Skopos gateway" if args.gateway_url else ""
    print(f"ok: {len(corpus['cases'])} LogQL cases match Loki{suffix}")


if __name__ == "__main__":
    main()
