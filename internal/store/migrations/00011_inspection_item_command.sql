-- +goose Up
-- 六期打磨 sprint 1：巡检报告「命令列」复制（契约 8.7 ⑧）。
--
-- 巡检报告明细此前只有 raw_output（`===命令===` 分段拼接的输出），没有
-- 结构化命令清单——报告抽屉无法提供「复制命令」入口（实机 .opsmind-copy-button
-- 仅命中「复制原始输出」1 个）。
--
-- 本列存模板展开后的命令清单（换行分隔，执行顺序），由 runner 在 inspectOne
-- 时按 os/模板/容器运行时装配并落库；历史行 DEFAULT ''（前端空串不渲染复制按钮）。

ALTER TABLE inspection_report_items
    ADD COLUMN command text NOT NULL DEFAULT ''
    CHECK (length(command) <= 4096);

-- +goose Down
-- Down 仅限测试/评估。
ALTER TABLE inspection_report_items DROP COLUMN IF EXISTS command;
