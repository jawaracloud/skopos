#!/usr/bin/env python3
"""Seed the corpus entries into the ClickHouse otel_logs table.

This is only needed for the optional gateway half of the Loki differential
harness: push the same deterministic entries into ClickHouse, run the Skopos
gateway, then set GATEWAY_URL when invoking the harness.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
CORPUS = os.path.join(ROOT, "test", "corpus", "logql", "corpus.json")


def clickhouse_timestamp(value):
    match = re.fullmatch(r"(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.(\d+))?Z", value)
    if not match:
        raise SystemExit(f"unsupported corpus timestamp {value!r}")
    fraction = (match.group(3) or "").ljust(9, "0")[:9]
    return f"{match.group(1)} {match.group(2)}.{fraction}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--clickhouse-url", default=os.environ.get("CLICKHOUSE_URL", "http://127.0.0.1:8123"))
    parser.add_argument("--database", default=os.environ.get("CLICKHOUSE_DATABASE", "otel"))
    parser.add_argument("--table", default=os.environ.get("LOGS_TABLE", "otel_logs"))
    parser.add_argument("--truncate", action="store_true", help="truncate the logs table before inserting")
    args = parser.parse_args()

    with open(CORPUS, encoding="utf-8") as handle:
        corpus = json.load(handle)
    rows = []
    for entry in corpus["entries"]:
        labels = entry["labels"]
        rows.append(json.dumps({
            "Timestamp": clickhouse_timestamp(entry["timestamp"]),
            "Body": entry["body"],
            "ServiceName": labels.get("service_name", ""),
            "SeverityText": labels.get("severity_text", ""),
            # Keep the fixture labels identical to the corpus. A non-zero
            # SeverityNumber would add a severity_number stream label that the
            # Loki push fixture does not have.
            "SeverityNumber": 0,
            "TraceId": "",
            "SpanId": "",
            "EventName": "",
            "LogAttributes": {},
            "ResourceAttributes": {},
        }))
    body = "INSERT INTO `" + args.database + "`.`" + args.table + "` FORMAT JSONEachRow\n" + "\n".join(rows)
    if args.truncate:
        truncate = "TRUNCATE TABLE `" + args.database + "`.`" + args.table + "`"
        request = urllib.request.Request(args.clickhouse_url, data=truncate.encode(), method="POST")
        urllib.request.urlopen(request, timeout=15).read()
    request = urllib.request.Request(args.clickhouse_url, data=body.encode(), method="POST")
    urllib.request.urlopen(request, timeout=15).read()
    print(f"inserted {len(rows)} corpus entries into {args.database}.{args.table}")


if __name__ == "__main__":
    main()
