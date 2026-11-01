SELECT
    `MetricName`,
    `TimeUnix`,
    `Value`
FROM `otel`.`otel_metrics_gauge`
WHERE `TimeUnix` >= {__start:DateTime64(9)} AND `TimeUnix` < {__end:DateTime64(9)}
ORDER BY `TimeUnix` ASC
LIMIT {__limit:UInt64}
