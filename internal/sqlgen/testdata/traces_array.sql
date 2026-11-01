SELECT
    `TraceId`,
    `SpanName`,
    `Duration`
FROM `otel`.`otel_traces`
WHERE (`Timestamp` >= {__start:DateTime64(9)} AND `Timestamp` < {__end:DateTime64(9)})
    AND (has({services:Array(String)}, `ServiceName`) AND (`SpanAttributes`[{attr_key:String}] = {method:String}))
ORDER BY `Duration` DESC
LIMIT {__limit:UInt64}
