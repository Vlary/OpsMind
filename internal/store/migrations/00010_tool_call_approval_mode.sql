-- +goose Up
-- 六期打磨 sprint 1：工具卡门限徽标持久化。
--
-- 此前 tool_result 的 approval_mode 只在 SSE 流内存在（SseToolResult 字段），
-- tool_calls 表无对应列——刷新/重新进入会话后「在 <mode> 门限下执行」的
-- 徽标直接消失（实机：流式中 Tag 序列含「免审批」，刷新后仅剩「需审批」
-- 「高风险」）。
--
-- 本迁移只加可空列，历史行为 NULL：
--   * NULL  = 非命令执行类工具（不适用）或 00010 之前的旧行（未记录）；
--   * 值   = all|standard|high|none（判定时的有效门限快照，
--            与 approvals.approval_mode 同枚举）。
-- CHECK 只约束非 NULL 值，历史行不受影响（DEFAULT 不设，避免把旧行伪造成
-- standard——前端对 null 不渲染徽标，语义如实）。

ALTER TABLE tool_calls
    ADD COLUMN approval_mode text NULL
    CHECK (approval_mode IN ('all', 'standard', 'high', 'none'));

-- +goose Down
-- Down 仅限测试/评估；旧二进制无该列消费者，可直接回退。
ALTER TABLE tool_calls DROP COLUMN IF EXISTS approval_mode;
