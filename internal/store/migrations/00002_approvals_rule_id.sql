-- +goose Up
-- 审批补充命中的风险规则 ID（如 RM_PROTECTED、FILE_WRITE），供前端按
-- 结构化规则渲染中文风险说明；历史行为空串，展示时回退原始 reason。
ALTER TABLE approvals ADD COLUMN rule_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE approvals DROP COLUMN IF EXISTS rule_id;
