-- +goose Up
-- 工具调用失败落库底层错误消息：历史会话的工具卡与 SSE 实时事件
-- （tool_result.error.message）保持一致，刷新后不只显示错误码。
ALTER TABLE tool_calls ADD COLUMN error_message text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tool_calls DROP COLUMN IF EXISTS error_message;
