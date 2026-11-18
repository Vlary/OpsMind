-- +goose Up
-- 第 10 轮走查 medium：terminal_sessions 记录创建时消费的 terminal grant。
--
-- 此前 grant 在 POST /api/term/sessions 消费，但会话行不保存 approval id：
-- WS 升级后 SSH 拨号失败时 handler 无从知道该回滚哪一条审批，一次不可达
-- 目标就会永久烧掉他人刚批准的授权（契约 12.3 第 12 条要求「消费后会话
-- Open 失败必须回滚 consumed_at 并写 terminal.grant_released」）。
--
-- 可空：00015 之前的历史行没有来源授权；新行创建时必定写入。
ALTER TABLE terminal_sessions
    ADD COLUMN grant_id uuid NULL REFERENCES approvals (id) ON DELETE SET NULL;

COMMENT ON COLUMN terminal_sessions.grant_id IS
    '创建会话时消费的 terminal grant（approvals.id）；WS PTY 拨号失败时据此回滚 consumed_at。NULL=00015 前历史行。';

-- +goose Down
ALTER TABLE terminal_sessions DROP COLUMN IF EXISTS grant_id;
