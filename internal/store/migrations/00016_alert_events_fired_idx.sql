-- +goose Up
-- 五期 M1 图表聚合 / 下钻：纯 fired_at 范围扫描的专用索引。
--
-- 现有 alert_events_status_fired_idx(status, fired_at DESC) 的前导列是
-- status；GET /api/monitor/trend 的 severity_timeline、GET /api/alert-events
-- 的 fired_after/fired_before 下钻都是「时间窗内全部状态」，用不上该索引
-- 的前导列。本索引与分页排序 (fired_at DESC, id DESC) 同序，范围扫描后
-- 天然有序，无需额外排序。
CREATE INDEX alert_events_fired_idx ON alert_events(fired_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS alert_events_fired_idx;
