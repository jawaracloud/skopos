SELECT
    toStartOfInterval(`TimeUnix`, INTERVAL 60 SECOND) AS `ts`,
    avg(`Value`) AS `value`,
    `Attributes`[{service_label:String}] AS `service`
FROM `otel`.`otel_metrics_gauge` FINAL
WHERE (`TimeUnix` >= {__start:DateTime64(9)} AND `TimeUnix` < {__end:DateTime64(9)})
    AND (`MetricName` = {metric:String})
GROUP BY `ts`, `service`
HAVING (avg(`Value`) > {threshold:Float64})
ORDER BY `ts` ASC
LIMIT {__limit:UInt64}
