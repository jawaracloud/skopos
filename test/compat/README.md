# Compatibility harnesses

This directory is reserved for the M3–M5 black-box comparison harnesses:

- Prometheus compliance tester and a three-way diff against cerberus.
- Loki benchmark corpus replay.
- Tempo reference container replay.

The harnesses run reference implementations as binaries or containers. They do
not link, vendor, or copy code from cerberus, oteldb, Loki, or Tempo.
