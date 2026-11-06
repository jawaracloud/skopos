SELECT
    `MetricName`,
    `TimeUnix`,
    `Value`
FROM `otel`.`otel_metrics_gauge` FINAL
WHERE (`TimeUnix` >= {__start:DateTime64(9)} AND `TimeUnix` < {__end:DateTime64(9)})
    AND ((`MetricName` = {metric:String}) AND (`Attributes`[{service_label:String}] = {service:String}))
ORDER BY `TimeUnix` ASC
LIMIT {__limit:UInt64}
