SELECT
    `Timestamp`,
    `Body`,
    `SeverityText`
FROM `otel`.`otel_logs`
WHERE (`Timestamp` >= {__start:DateTime64(9)} AND `Timestamp` < {__end:DateTime64(9)})
    AND ((`ServiceName` = {service:String}) AND match(`Body`, {pattern:String}))
ORDER BY `Timestamp` DESC
LIMIT {__limit:UInt64}
