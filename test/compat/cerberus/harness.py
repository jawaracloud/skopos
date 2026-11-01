#!/usr/bin/env python3
"""Three-way differential comparison for the Skopos gateway.

The corpus is deterministic and lives in the repository:

* PromQL: test/compat/cerberus/promql.json, evaluated against a real
  Prometheus TSDB built from test/compat/cerberus/metrics.json.
* LogQL: test/corpus/logql/corpus.json, with reference values recorded from
  grafana/loki 3.5.7.
* TraceQL: test/corpus/traceql/corpus.json, with reference values recorded
  from grafana/tempo 2.7.2 plus trace summaries computed from the same spans.

The runner compares the reference, cerberus, and Skopos. Presentation-only
differences are normalized as documented in README.md; everything else is
printed as an exact unified diff.

Exit status:
  0  Skopos agrees with the reference on every case, no undeclared cerberus
     divergence exists, and every baseline entry still fails as recorded.
  1  Skopos diverges from the reference, a cerberus divergence is missing from
     known-cerberus-gaps.json, a documented gap now passes, or a reference/
     corpus check fails.
  1  with --strict: any documented cerberus gap also fails.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import difflib
import json
import math
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
TRACEQL_CORPUS = os.path.join(ROOT, "test", "corpus", "traceql", "corpus.json")
LOGQL_CORPUS = os.path.join(ROOT, "test", "corpus", "logql", "corpus.json")
PROMQL_CORPUS = os.path.join(ROOT, "test", "compat", "cerberus", "promql.json")
KNOWN_GAPS = os.path.join(ROOT, "test", "compat", "cerberus", "known-cerberus-gaps.json")

# Reference Loki synthesizes these labels; Skopos and cerberus do not always
# agree on them, and they are not query inputs in the corpus.
LOG_DROP_LABELS = {"detected_level", "__error__", "__error_details__"}

HTTP_TIMEOUT = 60


# ---------------------------------------------------------------------------
# HTTP and canonicalisation helpers
# ---------------------------------------------------------------------------


def http_json(url: str, timeout: int = HTTP_TIMEOUT):
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read()
            return response.status, json.loads(body) if body else None
    except urllib.error.HTTPError as error:
        body = error.read()
        try:
            parsed = json.loads(body)
        except Exception:
            parsed = None
        return error.code, parsed


def num(value) -> str:
    """Canonical numeric representation with presentation-only float noise gone."""
    if isinstance(value, str):
        value = float(value)
    if math.isnan(value):
        return "NaN"
    if math.isinf(value):
        return "Inf" if value > 0 else "-Inf"
    if value == 0:
        return "0"
    return f"{value:.12g}"


def canonical(value) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def unified(a, b, left="reference", right="gateway") -> str:
    return "\n".join(
        difflib.unified_diff(
            json.dumps(a, indent=2, sort_keys=True).splitlines(),
            json.dumps(b, indent=2, sort_keys=True).splitlines(),
            fromfile=left,
            tofile=right,
            lineterm="",
        )
    )


def equals(a, b) -> bool:
    return canonical(a) == canonical(b)


# ---------------------------------------------------------------------------
# PromQL
# ---------------------------------------------------------------------------


def prom_query(base: str, case: dict, is_range: bool):
    if is_range:
        params = {
            "query": case["query"],
            "start": str(case["start"]),
            "end": str(case["end"]),
            "step": str(case["step"]),
        }
        path = "/api/v1/query_range"
    else:
        params = {"query": case["query"], "time": str(case["instantTime"])}
        path = "/api/v1/query"
    return http_json(base.rstrip("/") + path + "?" + urllib.parse.urlencode(params))


def prom_payload(status: int, body):
    if status != 200:
        return ("http", status, (body or {}).get("error") if isinstance(body, dict) else None)
    if not isinstance(body, dict) or body.get("status") != "success":
        return ("error", body.get("errorType") if isinstance(body, dict) else None, body.get("error") if isinstance(body, dict) else None)
    data = body.get("data", {})
    result_type = data.get("resultType")
    if result_type == "scalar":
        value = data.get("result")
        return (result_type, ((int(round(float(value[0]))), num(value[1])),))
    series = []
    for item in data.get("result", []):
        metric = tuple(sorted(item.get("metric", {}).items()))
        values = []
        if "values" in item:
            for timestamp, value in item["values"]:
                values.append((int(round(float(timestamp))), num(value)))
        elif "value" in item:
            timestamp, value = item["value"]
            values.append((int(round(float(timestamp))), num(value)))
        series.append((metric, tuple(sorted(values))))
    return (result_type, tuple(sorted(series)))


def check_promql(prometheus_url: str, skopos_url: str, cerberus_url: str):
    with open(PROMQL_CORPUS, encoding="utf-8") as handle:
        corpus = json.load(handle)
    instant_time = corpus["instantTime"]
    results = []

    def run(case: dict, is_range: bool):
        ref_status, ref_body = prom_query(prometheus_url, case, is_range)
        skopos_status, skopos_body = prom_query(skopos_url, case, is_range)
        cerberus_status, cerberus_body = prom_query(cerberus_url, case, is_range)
        ref = prom_payload(ref_status, ref_body)
        skopos = prom_payload(skopos_status, skopos_body)
        cerberus = prom_payload(cerberus_status, cerberus_body)
        problems = []
        if not equals(ref, skopos):
            problems.append(("skopos", unified(ref, skopos, "reference", "skopos")))
        if not equals(ref, cerberus):
            problems.append(("cerberus", unified(ref, cerberus, "reference", "cerberus")))
        results.append(make_result("promql", case["name"], problems))

    for case in corpus["instant"]:
        case = dict(case, instantTime=instant_time)
        run(case, False)
    for case in corpus["range"]:
        run(case, True)
    return results


# ---------------------------------------------------------------------------
# LogQL
# ---------------------------------------------------------------------------


def log_request(base: str, case: dict):
    params = {
        "query": case["query"],
        "start": case["start"],
        "end": case["end"],
        "limit": "5000",
        "direction": "forward",
    }
    if case.get("step"):
        params["step"] = case["step"]
    if case["kind"] == "log" or case.get("step"):
        path = "/loki/api/v1/query_range"
    else:
        path = "/loki/api/v1/query"
        params = {"query": case["query"], "time": case["end"]}
    return http_json(base.rstrip("/") + path + "?" + urllib.parse.urlencode(params))


def log_streams(status: int, body):
    if status != 200:
        return ("http", status)
    if not isinstance(body, dict) or body.get("status") != "success":
        return ("error", body)
    lines = []
    for stream in body.get("data", {}).get("result", []):
        labels = tuple(sorted((k, v) for k, v in stream.get("stream", {}).items() if k not in LOG_DROP_LABELS))
        for timestamp, line in stream.get("values", []):
            lines.append((int(timestamp), line, labels))
    return ("ok", tuple(sorted(lines, key=lambda item: (item[0], item[1], canonical(item[2])))))


def log_metric(status: int, body):
    if status != 200:
        return ("http", status)
    if not isinstance(body, dict) or body.get("status") != "success":
        return ("error", body)
    result_type = body.get("data", {}).get("resultType")
    series = []
    for item in body.get("data", {}).get("result", []):
        metric = tuple(sorted((k, v) for k, v in item.get("metric", {}).items() if k not in LOG_DROP_LABELS))
        values = []
        if "values" in item:
            values = [(int(round(float(t))), num(v)) for t, v in item["values"]]
        elif "value" in item:
            t, v = item["value"]
            values = [(int(round(float(t))), num(v))]
        series.append((metric, tuple(sorted(values))))
    return (result_type, tuple(sorted(series)))


def timestamp_nanos(value: str) -> int:
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    return int(parsed.timestamp() * 1_000_000_000)


def log_expected(case: dict):
    if case["kind"] == "log":
        expected = []
        for line in case.get("expected_logs", []):
            expected.append((timestamp_nanos(line["timestamp"]), line.get("labels", {})))
        return sorted(expected, key=lambda item: item[0])
    series = []
    for item in case.get("expected", []):
        metric = tuple(sorted(item.get("metric", {}).items()))
        values = tuple((int(round(float(t))), num(v)) for t, v in item.get("values", []))
        series.append((metric, values))
    return tuple(sorted(series))


def check_log_expected(case: dict, normalized) -> str:
    if case["kind"] == "log":
        if normalized[0] != "ok":
            return f"expected streams, got {normalized}"
        actual = normalized[1]
        expected = log_expected(case)
        if len(actual) != len(expected):
            return f"matched {len(actual)} lines, want {len(expected)}"
        for index, (timestamp, labels) in enumerate(expected):
            got_timestamp, _, got_labels = actual[index]
            if got_timestamp != timestamp:
                return f"line {index} timestamp {got_timestamp}, want {timestamp}"
            for name, value in labels.items():
                if dict(got_labels).get(name) != value:
                    return f"line {index} label {name}={dict(got_labels).get(name)!r}, want {value!r}"
        return ""
    expected = log_expected(case)
    if normalized[0] == "http":
        return f"expected a metric result, got HTTP {normalized[1]}"
    actual = normalized[1]
    if not equals(actual, expected):
        return unified(expected, actual, "corpus", "gateway")
    return ""


def check_logql(skopos_url: str, cerberus_url: str):
    with open(LOGQL_CORPUS, encoding="utf-8") as handle:
        corpus = json.load(handle)
    results = []
    for case in corpus["cases"]:
        sk = log_streams(*log_request(skopos_url, case)) if case["kind"] == "log" else log_metric(*log_request(skopos_url, case))
        ce = log_streams(*log_request(cerberus_url, case)) if case["kind"] == "log" else log_metric(*log_request(cerberus_url, case))
        problems = []
        sk_problem = check_log_expected(case, sk)
        ce_problem = check_log_expected(case, ce)
        if sk_problem:
            problems.append(("skopos", sk_problem))
        if ce_problem:
            problems.append(("cerberus", ce_problem))
        if not sk_problem and not ce_problem and not equals(sk, ce):
            # Both match the recorded reference fields, but their full label
            # sets differ. Attribute it to cerberus unless Skopos is the one
            # that diverges from the reference shape.
            problems.append(("cerberus", unified(sk, ce, "skopos", "cerberus")))
        results.append(make_result("logql", case["name"], problems))
    return results


# ---------------------------------------------------------------------------
# TraceQL
# ---------------------------------------------------------------------------


def trace_search(base: str, query: str, start: int, end: int):
    params = {"q": query, "start": str(start), "end": str(end), "limit": "100", "spss": "100"}
    return http_json(base.rstrip("/") + "/api/search?" + urllib.parse.urlencode(params))


def trace_search_payload(status: int, body):
    if status != 200:
        return ("error", status)
    if not isinstance(body, dict):
        return ("error", body)
    traces = []
    for trace in body.get("traces", []):
        trace_id = str(trace.get("traceID", "")).lower()
        span_ids = set()
        for set_name in ("spanSet", "spanSets"):
            value = trace.get(set_name)
            sets = value if isinstance(value, list) else ([value] if isinstance(value, dict) else [])
            for span_set in sets:
                for span in span_set.get("spans", []):
                    span_ids.add(str(span.get("spanID", "")).lower())
        summary = (
            trace_id,
            trace.get("rootServiceName", ""),
            trace.get("rootTraceName", ""),
            int(trace.get("startTimeUnixNano") or 0),
            float(trace.get("durationMs") or 0),
        )
        traces.append((summary, tuple(sorted(span_ids))))
    traces.sort(key=lambda item: item[0][0])
    return ("ok", tuple(traces))


def corpus_traces(corpus: dict):
    base_ns = int(corpus["baseTimeUnixNano"])
    traces = {}
    for trace in corpus["traces"]:
        spans = []
        for span in trace["spans"]:
            start = base_ns + int(span["startOffsetNanos"])
            end = start + int(span["durationNanos"])
            spans.append((span, start, end))
        traces[trace["traceID"]] = spans
    return traces, base_ns


def trace_summary(corpus_trace, trace_id: str):
    spans = corpus_trace
    start = min(item[1] for item in spans)
    end = max(item[2] for item in spans)
    roots = [item for item in spans if not item[0].get("parentSpanID")]
    root = min(roots, key=lambda item: item[1]) if roots else None
    return (
        trace_id,
        root[0].get("service", "") if root else "",
        root[0].get("name", "") if root else "",
        start,
        (end - start) / 1_000_000,
    )


def reference_search(corpus: dict, case: dict):
    if case.get("expectError"):
        return ("error", 400)
    traces, _ = corpus_traces(corpus)
    trace_ids = sorted(case.get("expectTraceIDs", []))
    matched = case.get("expectMatchedSpans") or {}
    out = []
    for trace_id in trace_ids:
        spans = traces[trace_id]
        if trace_id in matched:
            span_ids = tuple(sorted(matched[trace_id]))
        else:
            span_ids = tuple(sorted(span["spanID"] for span, _, _ in spans))
        out.append((trace_summary(spans, trace_id), span_ids))
    out.sort(key=lambda item: item[0][0])
    return ("ok", tuple(out))


def split_search(value):
    if value[0] != "ok":
        return value, value
    core = tuple((summary[0], span_ids) for summary, span_ids in value[1])
    summary = tuple(item[0] for item in value[1])
    return core, summary


def trace_tags(base: str, start: int, end: int):
    return http_json(base.rstrip("/") + "/api/v2/search/tags?" + urllib.parse.urlencode({"start": start, "end": end}))


def trace_tag_values(base: str, tag: str, start: int, end: int):
    path = "/api/v2/search/tag/" + urllib.parse.quote(tag, safe="") + "/values"
    return http_json(base.rstrip("/") + path + "?" + urllib.parse.urlencode({"start": start, "end": end}))


def trace_tag_payload(status: int, body):
    if status != 200 or not isinstance(body, dict):
        return ("http", status)
    return ("ok", {scope.get("name", ""): tuple(sorted(scope.get("tags", []))) for scope in body.get("scopes", [])})


def trace_tag_values_payload(status: int, body):
    if status != 200 or not isinstance(body, dict):
        return ("http", status)
    values = []
    for item in body.get("tagValues", []):
        value = item.get("value")
        if value is not None:
            values.append(str(value).lower())
    return ("ok", tuple(sorted(values)))


def decode_id(value: str) -> str:
    if not value:
        return ""
    raw = str(value)
    if len(raw) in (16, 32) and all(ch in "0123456789abcdefABCDEF" for ch in raw):
        return raw.lower()
    try:
        decoded = base64.b64decode(raw, validate=True)
        if decoded:
            return decoded.hex()
    except Exception:
        pass
    return raw.lower()


def normalize_kind(value) -> str:
    if isinstance(value, int):
        return {0: "unspecified", 1: "internal", 2: "server", 3: "client", 4: "producer", 5: "consumer"}.get(value, str(value))
    raw = str(value).lower()
    for prefix in ("span_kind_",):
        if raw.startswith(prefix):
            raw = raw[len(prefix):]
    return raw or "unspecified"


def normalize_status(value) -> str:
    if isinstance(value, int):
        return {0: "unset", 1: "ok", 2: "error"}.get(value, str(value))
    raw = str(value).lower()
    for prefix in ("status_code_",):
        if raw.startswith(prefix):
            raw = raw[len(prefix):]
    return raw or "unset"


def attr_map(value) -> dict:
    if isinstance(value, dict):
        return {str(k): str(v).lower() if isinstance(v, bool) else str(v) for k, v in value.items()}
    out = {}
    for item in value or []:
        key = item.get("key")
        raw = item.get("value", {})
        if key is None:
            continue
        if isinstance(raw, dict):
            for candidate in ("stringValue", "intValue", "boolValue", "doubleValue"):
                if candidate in raw:
                    out[str(key)] = str(raw[candidate]).lower() if isinstance(raw[candidate], bool) else str(raw[candidate])
                    break
        else:
            out[str(key)] = str(raw)
    return out


def trace_by_id(base: str, trace_id: str):
    return http_json(base.rstrip("/") + "/api/v2/traces/" + trace_id, timeout=HTTP_TIMEOUT)


def trace_by_id_payload(status: int, body):
    if status != 200 or not isinstance(body, dict):
        return ("http", status)
    trace = body.get("trace", body)
    spans = []
    for resource_span in trace.get("resourceSpans", []):
        resource = attr_map(resource_span.get("resource", {}).get("attributes", []))
        service = resource.get("service.name", "")
        for scope_span in resource_span.get("scopeSpans", []):
            for span in scope_span.get("spans", []):
                start = int(span.get("startTimeUnixNano") or 0)
                end = int(span.get("endTimeUnixNano") or 0)
                spans.append(
                    {
                        "id": decode_id(span.get("spanId", "")),
                        "parent": decode_id(span.get("parentSpanId", "")),
                        "name": span.get("name", ""),
                        "start": start,
                        "duration": end - start,
                        "kind": normalize_kind(span.get("kind")),
                        "status": normalize_status((span.get("status") or {}).get("code")),
                        "statusMessage": (span.get("status") or {}).get("message", ""),
                        "resource": resource,
                        "attributes": attr_map(span.get("attributes", [])),
                        "events": tuple(sorted(event.get("name", "") for event in span.get("events", []))),
                        "links": tuple(sorted(decode_id(link.get("traceId", "")) for link in span.get("links", []))),
                        "service": service,
                    }
                )
    spans.sort(key=lambda item: item["id"])
    return ("ok", tuple(spans))


def root_span(spans) -> str:
    roots = [span for span in spans if not span["parent"]]
    if not roots:
        return ""
    return min(roots, key=lambda span: span["start"])["id"]


def check_traceql(skopos_url: str, cerberus_url: str):
    with open(TRACEQL_CORPUS, encoding="utf-8") as handle:
        corpus = json.load(handle)
    base_ns = int(corpus["baseTimeUnixNano"])
    start = base_ns // 1_000_000_000 - 60
    end = base_ns // 1_000_000_000 + 600
    results = []

    # Search cases.
    for case in corpus["queries"]:
        reference = reference_search(corpus, case)
        sk = trace_search_payload(*trace_search(skopos_url, case["query"], start, end))
        ce = trace_search_payload(*trace_search(cerberus_url, case["query"], start, end))
        problems = []
        ref_core, ref_summary = split_search(reference)
        sk_core, sk_summary = split_search(sk)
        ce_core, ce_summary = split_search(ce)
        if not equals(ref_core, sk_core):
            problems.append(("skopos", unified(ref_core, sk_core, "reference-core", "skopos-core")))
        if not equals(ref_summary, sk_summary):
            problems.append(("skopos", unified(ref_summary, sk_summary, "reference-summary", "skopos-summary")))
        if not equals(ref_core, ce_core):
            problems.append(("cerberus", unified(ref_core, ce_core, "reference-core", "cerberus-core")))
        if not equals(ref_summary, ce_summary):
            problems.append(("cerberus", unified(ref_summary, ce_summary, "reference-summary", "cerberus-summary")))
        results.append(make_result("traceql/search", case["name"], problems))

    # Tag-name scopes.
    expected_scopes = {test["scope"]: tuple(sorted(test["expect"])) for test in corpus["tagNameTests"]}
    sk_scopes = trace_tag_payload(*trace_tags(skopos_url, start, end))
    ce_scopes = trace_tag_payload(*trace_tags(cerberus_url, start, end))
    problems = []
    if sk_scopes[0] != "ok" or not scopes_match(expected_scopes, sk_scopes[1], exact=True):
        problems.append(("skopos", unified(expected_scopes, sk_scopes, "reference", "skopos")))
    if ce_scopes[0] != "ok" or not scopes_match(expected_scopes, ce_scopes[1], exact=False):
        problems.append(("cerberus", unified(expected_scopes, ce_scopes, "reference", "cerberus")))
    results.append(make_result("traceql/tags", "tag_name_scopes", problems))

    # Tag values.
    for test in corpus["tagValueTests"]:
        expected = tuple(sorted(value.lower() for value in test["expect"]))
        sk = trace_tag_values_payload(*trace_tag_values(skopos_url, test["tag"], start, end))
        ce = trace_tag_values_payload(*trace_tag_values(cerberus_url, test["tag"], start, end))
        problems = []
        if sk[0] != "ok" or sk[1] != expected:
            problems.append(("skopos", unified(expected, sk, "reference", "skopos")))
        if ce[0] != "ok" or ce[1] != expected:
            problems.append(("cerberus", unified(expected, ce, "reference", "cerberus")))
        results.append(make_result("traceql/tag-values", test["name"], problems))

    # Trace by ID.
    for test in corpus["traceTests"]:
        trace_id = test["traceID"]
        sk = trace_by_id_payload(*trace_by_id(skopos_url, trace_id))
        ce = trace_by_id_payload(*trace_by_id(cerberus_url, trace_id))
        problems = []
        problems.extend(check_trace_reference(test, sk, "skopos"))
        problems.extend(check_trace_reference(test, ce, "cerberus"))
        if sk[0] == "ok" and ce[0] == "ok":
            sk_common = trace_common(sk[1])
            ce_common = trace_common(ce[1])
            if not equals(sk_common, ce_common):
                problems.append(("cerberus", unified(sk_common, ce_common, "skopos", "cerberus")))
        results.append(make_result("traceql/trace-by-id", trace_id, problems))
    return results


def scopes_match(expected, actual, exact: bool) -> bool:
    if actual is None:
        return False
    if exact:
        return {name: tuple(values) for name, values in expected.items()} == {name: tuple(values) for name, values in actual.items()}
    for name, values in expected.items():
        if name not in actual or not set(values).issubset(set(actual[name])):
            return False
    return True


def trace_common(spans):
    return tuple(
        (
            span["id"],
            span["parent"],
            span["name"],
            span["start"],
            span["duration"],
            span["kind"],
            span["status"],
            tuple(sorted(span["resource"].items())),
            tuple(sorted(span["attributes"].items())),
        )
        for span in spans
    )


def check_trace_reference(test: dict, payload, source: str):
    if payload[0] != "ok":
        return [(source, f"expected trace {test['traceID']}, got {payload}")]
    spans = payload[1]
    problems = []
    ids = [span["id"] for span in spans]
    names = [span["name"] for span in spans]
    if len(spans) != test["spanCount"]:
        problems.append((source, f"{source} span count {len(spans)}, want {test['spanCount']}"))
    if ids != sorted(test["spanIDs"]):
        problems.append((source, f"{source} span IDs {ids}, want {sorted(test['spanIDs'])}"))
    if sorted(names) != sorted(test["names"]):
        problems.append((source, f"{source} span names {sorted(names)}, want {sorted(test['names'])}"))
    if root_span(spans) != test["rootSpanID"]:
        problems.append((source, f"{source} root {root_span(spans)}, want {test['rootSpanID']}"))
    if sorted(event for span in spans for event in span["events"]) != sorted(test.get("eventNames", [])):
        problems.append((source, f"{source} event names {sorted(event for span in spans for event in span['events'])}, want {sorted(test.get('eventNames', []))}"))
    if sorted(link for span in spans for link in span["links"]) != sorted(test.get("linkTraceIDs", [])):
        problems.append((source, f"{source} link trace IDs {sorted(link for span in spans for link in span['links'])}, want {sorted(test.get('linkTraceIDs', []))}"))
    return problems


# ---------------------------------------------------------------------------
# Reporting
# ---------------------------------------------------------------------------


def make_result(signal: str, name: str, problems):
    if not problems:
        return {"signal": signal, "name": name, "status": "PASS", "problems": []}
    kinds = {kind for kind, _ in problems}
    status = "SKOPOS_DIFF" if "skopos" in kinds else "CERBERUS_GAP"
    return {"signal": signal, "name": name, "status": status, "problems": problems}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prometheus-url", default=os.environ.get("PROMETHEUS_URL", "http://127.0.0.1:18093"))
    parser.add_argument("--skopos-url", default=os.environ.get("SKOPOS_URL", "http://127.0.0.1:18088"))
    parser.add_argument("--cerberus-url", default=os.environ.get("CERBERUS_URL", "http://127.0.0.1:18092"))
    parser.add_argument("--strict", action="store_true", help="fail on known cerberus gaps too")
    parser.add_argument("--verbose", action="store_true", help="print every case, not just failures")
    args = parser.parse_args()

    with open(KNOWN_GAPS, encoding="utf-8") as handle:
        baseline = set(json.load(handle).get("gaps", []))

    results = []
    results.extend(check_promql(args.prometheus_url, args.skopos_url, args.cerberus_url))
    results.extend(check_logql(args.skopos_url, args.cerberus_url))
    results.extend(check_traceql(args.skopos_url, args.cerberus_url))

    # A cerberus gap is expected only when the exact case is documented in the
    # baseline. Anything else is a regression in the comparison, not a known
    # limitation.
    for result in results:
        key = f"{result['signal']}/{result['name']}"
        if result["status"] == "CERBERUS_GAP" and key not in baseline:
            result["status"] = "CERBERUS_REGRESSION"
    found = {f"{result['signal']}/{result['name']}" for result in results if result["status"] == "CERBERUS_GAP"}
    fixed = sorted(baseline - found)
    for key in fixed:
        results.append({"signal": "baseline", "name": key, "status": "CERBERUS_FIXED", "problems": [("baseline", "documented gap now passes; remove it from known-cerberus-gaps.json")]})

    counts = {"PASS": 0, "CERBERUS_GAP": 0, "CERBERUS_REGRESSION": 0, "CERBERUS_FIXED": 0, "SKOPOS_DIFF": 0}
    for result in results:
        counts[result["status"]] += 1

    for result in results:
        if result["status"] == "PASS" and not args.verbose:
            continue
        print(f"{result['status']:<18} {result['signal']}/{result['name']}")
        for kind, detail in result["problems"]:
            print(f"  [{kind}]")
            for line in detail.splitlines():
                print("    " + line)
        print()

    total = len([result for result in results if result["status"] != "CERBERUS_FIXED"])
    print(
        f"summary: {total} cases; {counts['PASS']} pass; "
        f"{counts['CERBERUS_GAP']} known cerberus gaps; "
        f"{counts['CERBERUS_REGRESSION']} cerberus regressions; "
        f"{counts['CERBERUS_FIXED']} fixed baseline entries; "
        f"{counts['SKOPOS_DIFF']} Skopos divergences"
    )
    if counts["SKOPOS_DIFF"]:
        print("FAIL: Skopos diverges from the reference; see the diffs above", file=sys.stderr)
        return 1
    if counts["CERBERUS_REGRESSION"]:
        print("FAIL: a cerberus divergence is not in known-cerberus-gaps.json", file=sys.stderr)
        return 1
    if counts["CERBERUS_FIXED"]:
        print("FAIL: known-cerberus-gaps.json is stale; remove the fixed entries above", file=sys.stderr)
        return 1
    if args.strict and counts["CERBERUS_GAP"]:
        print("FAIL (--strict): cerberus diverges; see the gaps above", file=sys.stderr)
        return 1
    print("ok: Skopos matches the reference on every corpus case")
    return 0


if __name__ == "__main__":
    sys.exit(main())
