-- +goose Up
-- 走查第 4 轮（high）：审计哈希链精度重锚。
--
-- 00014 之前写入侧用纳秒时间戳计算哈希（audit/hash.go 的
-- CreatedAt.UTC()），但 audit_logs.created_at 是 timestamptz，PostgreSQL
-- 只保留微秒。校验侧用读回的微秒值复算，必然对不上——全新库也会被
-- GET /api/audit/verify 判成 AUDIT_CHAIN_BROKEN（broken_at=1）。
--
-- 写入侧已改为在 Prepare/computeHash 之前把 CreatedAt 截断到微秒；历史
-- 行的纳秒输入已被数据库丢弃、无法从库内值恢复，因此一次性标记
-- legacy_hash=true：校验时只校验 prev_hash 链接，跳过哈希复算（无法
-- 证明「未被篡改」，但也不再把正常历史判成断裂）。新行必须完整复算。
--
-- 用「加列默认 true，再把默认改回 false」给存量行回填：audit_logs 有
-- BEFORE UPDATE 的 append-only 触发器（00001_init.sql:202），UPDATE 回填
-- 会抛异常；加列默认值只改元数据，不触发该触发器。
ALTER TABLE audit_logs
    ADD COLUMN legacy_hash boolean NOT NULL DEFAULT true;
ALTER TABLE audit_logs
    ALTER COLUMN legacy_hash SET DEFAULT false;

COMMENT ON COLUMN audit_logs.legacy_hash IS
    '00014 前存量行（纳秒哈希输入不可恢复）为 true：校验只核对链接，不复算哈希；新行恒 false。应用不得写入 true。';

-- +goose Down
ALTER TABLE audit_logs DROP COLUMN IF EXISTS legacy_hash;
