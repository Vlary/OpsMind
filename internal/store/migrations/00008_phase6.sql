-- +goose Up
-- 六期：交互深化（契约 2.11 / 4.6 / 4.9 / 4.11）——审批门限、命令改写。
--
--   1) 全局审批门限 settings['approval.mode']：JSON 字符串
--      "all"|"standard"|"high"|"none"，缺省 standard（2.11 约定 1）。
--      幂等插入；读取路径对键缺失同样兜底 standard（防手工删键）。
--   2) conversations.approval_mode：会话级覆盖（NULL=跟随全局）。
--      有效门限解析顺序：会话覆盖 > settings 全局缺省（2.11 约定 1）。
--   3) approvals.approval_mode：创建审批时对「有效门限」的快照
--      （SSE approval_required 与审批列表据此展示；历史行 DEFAULT 回填
--      'standard'——一至五期的行为即 standard 档，语义不变）。
--   4) approvals.edited_*：六期命令改写（2.11 约定 3）。空串=未改写。
--      decide 端点对改写命令重新 Classify，forbidden 拒绝整个 decide
--      （不落库），故 edited_risk_level 落库值只可能 safe|needs_approval。
--      PostgreSQL text 本身不允许 NUL 字节（插入即报错），NUL 防线在
--      应用层（binding 校验），迁移只约束长度。

INSERT INTO settings (key, value) VALUES ('approval.mode', '"standard"')
ON CONFLICT (key) DO NOTHING;

ALTER TABLE conversations
    ADD COLUMN approval_mode text NULL
    CHECK (approval_mode IN ('all', 'standard', 'high', 'none'));

ALTER TABLE approvals
    ADD COLUMN approval_mode text NOT NULL DEFAULT 'standard'
    CHECK (approval_mode IN ('all', 'standard', 'high', 'none'));

ALTER TABLE approvals
    ADD COLUMN edited_command text NOT NULL DEFAULT ''
    CHECK (length(edited_command) BETWEEN 0 AND 2000);

ALTER TABLE approvals
    ADD COLUMN edited_risk_level text NOT NULL DEFAULT ''
    CHECK (edited_risk_level IN ('', 'safe', 'needs_approval'));

ALTER TABLE approvals
    ADD COLUMN edited_rule_id text NOT NULL DEFAULT '';

-- +goose Down
-- Down 仅限测试/评估（契约 10.4）。settings 键与新增列在旧二进制下均无
-- 消费者，可直接回退；G8：Down 再 Up 必须可用（无数据依赖，成立）。
ALTER TABLE approvals DROP COLUMN IF EXISTS edited_rule_id;
ALTER TABLE approvals DROP COLUMN IF EXISTS edited_risk_level;
ALTER TABLE approvals DROP COLUMN IF EXISTS edited_command;
ALTER TABLE approvals DROP COLUMN IF EXISTS approval_mode;
ALTER TABLE conversations DROP COLUMN IF EXISTS approval_mode;
DELETE FROM settings WHERE key = 'approval.mode';
