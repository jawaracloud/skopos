-- Synthetic, deterministic-shaped dataset for the Skopos query benchmarks.
--
-- Shape: 24 hours ending at load time.
--   otel_metrics_sum:       200 series x 5,760 samples = 1,152,000 rows
--   otel_metrics_gauge:     200 series x 2,880 samples =   576,000 rows
--   otel_metrics_histogram: 160 series x 1,440 samples =   230,400 rows
--   otel_logs:                                                 500,000 rows
--   otel_traces:                                               200,000 rows
--
-- The values are synthetic but deliberately repeat low-cardinality resource
-- attributes and realistic message/body text so that ZSTD compression numbers
-- are representative of a small production-like install, not of random bytes.

INSERT INTO otel.otel_metrics_sum
    (ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
     ScopeDroppedAttrCount, ScopeSchemaUrl, ServiceName, MetricName, MetricDescription,
     MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags,
     AggregationTemporality, IsMonotonic)
SELECT
    map('service.name', service, 'k8s.namespace.name', 'shop',
        'k8s.pod.name', concat('pod-', toString(series % 50)),
        'deployment.environment.name', 'prod') AS ResourceAttributes,
    '', 'skopos/bench', '1.0', map(), 0, '',
    service,
    metric,
    'synthetic monotonic counter',
    '1',
    map('http.method', if(series % 3 = 0, 'GET', 'POST'),
        'http.status_code', if(series % 7 = 0, '500', '200'),
        'instance', concat('pod-', toString(series % 50))),
    toDateTime(now() - INTERVAL 24 HOUR),
    toDateTime(now() - INTERVAL 24 HOUR) + toIntervalSecond(ts * 15),
    toFloat64(ts * 17 + series * 1000),
    0, 2, true
FROM
(
    SELECT
        number % 200 AS series,
        intDiv(number, 200) AS ts,
        concat('service-', toString((number % 200) % 20)) AS service,
        ['http_requests_total', 'http_request_errors_total', 'db_queries_total',
         'cache_hits_total', 'queue_messages_total', 'worker_jobs_total',
         'grpc_requests_total', 'kafka_messages_total', 'redis_calls_total',
         's3_requests_total'][toUInt8(((number % 200) % 10) + 1)] AS metric
    FROM numbers(0, 1152000)
);

INSERT INTO otel.otel_metrics_gauge
    (ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
     ScopeDroppedAttrCount, ScopeSchemaUrl, ServiceName, MetricName, MetricDescription,
     MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags)
SELECT
    map('service.name', service, 'k8s.namespace.name', 'shop',
        'k8s.pod.name', concat('pod-', toString(series % 50)),
        'deployment.environment.name', 'prod') AS ResourceAttributes,
    '', 'skopos/bench', '1.0', map(), 0, '',
    service,
    metric,
    'synthetic gauge',
    'By',
    map('instance', concat('pod-', toString(series % 50)),
        'node', concat('node-', toString(series % 5))),
    toDateTime(now() - INTERVAL 24 HOUR),
    toDateTime(now() - INTERVAL 24 HOUR) + toIntervalSecond(ts * 30),
    toFloat64(64 * 1024 * 1024 + (series * 7919 + ts * 13) % 33554432),
    0
FROM
(
    SELECT
        number % 200 AS series,
        intDiv(number, 200) AS ts,
        concat('service-', toString((number % 200) % 20)) AS service,
        ['process_resident_memory_bytes', 'go_goroutines', 'node_cpu_usage_ratio',
         'queue_depth', 'worker_utilization'][toUInt8(((number % 200) % 5) + 1)] AS metric
    FROM numbers(0, 576000)
);

INSERT INTO otel.otel_metrics_histogram
    (ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
     ScopeDroppedAttrCount, ScopeSchemaUrl, ServiceName, MetricName, MetricDescription,
     MetricUnit, Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts,
     ExplicitBounds, Flags, Min, Max, AggregationTemporality)
SELECT
    map('service.name', service, 'k8s.namespace.name', 'shop',
        'k8s.pod.name', concat('pod-', toString(series % 50))) AS ResourceAttributes,
    '', 'skopos/bench', '1.0', map(), 0, '',
    service,
    'http_request_duration',
    'synthetic request duration histogram',
    's',
    map('http.method', if(series % 2 = 0, 'GET', 'POST'),
        'http.route', route,
        'http.status_code', if(series % 11 = 0, '500', '200')),
    toDateTime(now() - INTERVAL 24 HOUR),
    toDateTime(now() - INTERVAL 24 HOUR) + toIntervalSecond(ts * 60),
    toUInt64(100 + (ts * 7 + series) % 400),
    toFloat64(1000 + (ts * 11 + series) % 5000) / 1000,
    arrayMap(x -> toUInt64(5 + (ts + series + x) % 40), range(12)),
    [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10],
    0, 0.001, 12.5, 2
FROM
(
    SELECT
        number % 160 AS series,
        intDiv(number, 160) AS ts,
        concat('service-', toString((number % 160) % 20)) AS service,
        ['/api/v1/orders', '/api/v1/checkout', '/api/v1/inventory',
         '/api/v1/cart', '/api/v1/search', '/api/v1/users',
         '/healthz', '/metrics'][toUInt8(intDiv(number % 160, 20) + 1)] AS route
    FROM numbers(0, 230400)
);

INSERT INTO otel.otel_logs
    (Timestamp, TraceId, SpanId, TraceFlags, SeverityText, SeverityNumber,
     ServiceName, Body, ResourceSchemaUrl, ResourceAttributes, ScopeSchemaUrl,
     ScopeName, ScopeVersion, ScopeAttributes, LogAttributes, EventName)
SELECT
    toDateTime64(now() - INTERVAL 24 HOUR, 9)
        + toIntervalMillisecond(intDiv(number * 86400000, 500000)),
    concat(lower(hex(cityHash64(number))), lower(hex(cityHash64(number + 500000)))),
    lower(hex(cityHash64(number + 900000))),
    1,
    multiIf(intDiv(number, 20) % 20 = 0, 'error', intDiv(number, 20) % 7 = 0, 'warn', intDiv(number, 20) % 3 = 0, 'debug', 'info'),
    multiIf(intDiv(number, 20) % 20 = 0, toUInt8(17), intDiv(number, 20) % 7 = 0, toUInt8(13), intDiv(number, 20) % 3 = 0, toUInt8(5), toUInt8(9)),
    concat('service-', toString(number % 20)),
    multiIf(
        intDiv(number, 20) % 20 = 0, concat('database query failed: connection reset by peer trace=', lower(hex(cityHash64(number)))),
        intDiv(number, 20) % 20 = 1, concat('upstream timeout after 30s service=payments request_id=', toString(number % 100000)),
        intDiv(number, 20) % 20 = 2, concat('request completed status=200 path=/api/v1/orders duration_ms=', toString(number % 900)),
        intDiv(number, 20) % 20 = 3, concat('user login succeeded user_id=', toString(number % 5000), ' tenant=shop'),
        intDiv(number, 20) % 20 = 4, concat('cache miss key=product:', toString(number % 20000)),
        intDiv(number, 20) % 20 = 5, concat('healthcheck ok component=api version=1.42.0'),
        intDiv(number, 20) % 20 = 6, concat('retrying job id=', toString(number % 10000), ' attempt=', toString(number % 5)),
        intDiv(number, 20) % 20 = 7, concat('received message topic=orders partition=', toString(number % 12), ' offset=', toString(number)),
        intDiv(number, 20) % 20 = 8, concat('order created order_id=ord-', toString(number % 1000000), ' amount=', toString(number % 500)),
        intDiv(number, 20) % 20 = 9, concat('payment authorized payment_id=pay-', toString(number % 1000000)),
        concat('request completed status=', if(number % 17 = 0, '500', '200'), ' path=/api/v1/cart duration_ms=', toString(number % 300))
    ),
    '',
    map('service.name', concat('service-', toString(number % 20)),
        'k8s.namespace.name', 'shop',
        'k8s.pod.name', concat('pod-', toString(number % 50)),
        'deployment.environment.name', 'prod',
        'host.name', concat('node-', toString(number % 5))),
    '',
    'skopos/bench', '1.0', map(),
    map('http.method', if(number % 2 = 0, 'GET', 'POST'),
        'http.route', ['/api/v1/orders', '/api/v1/checkout', '/api/v1/cart'][toUInt8(number % 3 + 1)],
        'duration_ms', toString(number % 900),
        'error.kind', if(intDiv(number, 20) % 20 = 0, 'connection_reset', '')),
    ''
FROM numbers(0, 500000);

INSERT INTO otel.otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, TraceState, SpanName, SpanKind,
     ServiceName, ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes,
     Duration, StatusCode, StatusMessage)
SELECT
    toDateTime64(now() - INTERVAL 24 HOUR, 9)
        + toIntervalMillisecond(intDiv(number * 86400000, 200000)),
    concat(lower(hex(cityHash64(intDiv(number, 5)))),
        lower(hex(cityHash64(intDiv(number, 5) + 1000000)))),
    lower(hex(cityHash64(number))),
    if(number % 5 = 0, '', lower(hex(cityHash64(intDiv(number, 5) * 5)))),
    '',
    ['GET /api/v1/orders', 'SELECT orders', 'redis GET cart',
     'GET /api/v1/inventory', 'POST /api/v1/checkout'][toUInt8(number % 5 + 1)],
    if(number % 5 = 0, 'Server', if(number % 5 = 2, 'Internal', 'Client')),
    concat('service-', toString(intDiv(number, 5) % 20)),
    map('service.name', concat('service-', toString(intDiv(number, 5) % 20)),
        'k8s.namespace.name', 'shop',
        'k8s.pod.name', concat('pod-', toString(intDiv(number, 5) % 50)),
        'deployment.environment.name', 'prod'),
    'skopos/bench', '1.0',
    map('http.method', 'GET',
        'http.status_code', if(number % 17 = 0, '500', '200'),
        'http.route', ['/api/v1/orders', '/api/v1/checkout', '/api/v1/inventory'][toUInt8(number % 3 + 1)],
        'db.system', if(number % 5 = 1, 'postgresql', ''),
        'span.type', if(number % 5 = 2, 'redis', 'http')),
    toUInt64(1000000 + (number % 1000) * 100000),
    if(number % 5 = 0 and number % 17 = 0, 'Error', 'Ok'),
    ''
FROM numbers(0, 200000);

OPTIMIZE TABLE otel.otel_metrics_sum FINAL;
OPTIMIZE TABLE otel.otel_metrics_gauge FINAL;
OPTIMIZE TABLE otel.otel_metrics_histogram FINAL;
OPTIMIZE TABLE otel.otel_logs FINAL;
OPTIMIZE TABLE otel.otel_traces FINAL;
