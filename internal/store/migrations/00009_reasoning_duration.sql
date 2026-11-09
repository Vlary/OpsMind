-- +goose Up
-- 六期打磨 sprint 2：深度思考耗时落库。
--
-- messages.reasoning_duration_ms 是 assistant 消息对应轮次 reasoning 的首/尾
-- delta 服务端时间差（毫秒）；与 messages.reasoning 同一条 INSERT 写入。
-- NULL = 无 reasoning（或 00009 之前的旧行，无计时数据）；0 = 有 reasoning
-- 但不足 1ms。列可空而非 DEFAULT 0：旧数据必须能区分「未计时」与「<1ms」，
-- 前端据此渲染「深度思考 · 耗时 —」/「耗时 0s」（契约 8.6 第 13 条）。

ALTER TABLE messages
    ADD COLUMN reasoning_duration_ms bigint NULL
    CHECK (reasoning_duration_ms IS NULL OR reasoning_duration_ms >= 0);

-- +goose Down
-- Down 仅限测试/评估；旧二进制无该列消费者，可直接回退。
ALTER TABLE messages DROP COLUMN IF EXISTS reasoning_duration_ms;
