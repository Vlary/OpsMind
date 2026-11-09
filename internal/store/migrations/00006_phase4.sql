-- +goose Up
-- 四期：容器管理（契约 2.9/4.3/4.9）。
--   1) servers.container_runtime：容器运行时探测缓存（''|none|docker|podman|
--      containerd），由 server_facts / POST test / 容器接口探测写回，用户不可
--      设置；network_device 恒 ''（应用保证，不参与 servers_check——与认证/资产
--      形态无关的纯缓存列）。
--   2) approvals：加 source（chat|console）列；conversation_id/tool_call_id 改
--      可空——console 审批（容器页操作，契约 2.9 约定 3）无对话/工具调用上下文。
--      chat 审批恒非 NULL 由应用保证；UNIQUE(tool_call_id) 保留（PostgreSQL
--      默认 NULLS DISTINCT，多个 NULL 合法）。
--   无新表、无新索引：容器数据不落库（实时 CLI 查询）；console 审批查询走既有
--   approvals(status, expires_at) 与主键。

ALTER TABLE servers ADD COLUMN container_runtime text NOT NULL DEFAULT ''
    CHECK (container_runtime IN ('', 'none', 'docker', 'podman', 'containerd'));

ALTER TABLE approvals ADD COLUMN source text NOT NULL DEFAULT 'chat'
    CHECK (source IN ('chat', 'console'));

ALTER TABLE approvals ALTER COLUMN conversation_id DROP NOT NULL;

ALTER TABLE approvals ALTER COLUMN tool_call_id DROP NOT NULL;

-- +goose Down
-- 回滚前先删 console 审批行（其 conversation_id/tool_call_id 为 NULL，
-- 无法满足恢复后的 NOT NULL）；契约 10.4：Down 仅限测试/评估，需接受数据丢失。
DELETE FROM approvals WHERE source = 'console';
ALTER TABLE approvals ALTER COLUMN tool_call_id SET NOT NULL;
ALTER TABLE approvals ALTER COLUMN conversation_id SET NOT NULL;
ALTER TABLE approvals DROP COLUMN IF EXISTS source;
ALTER TABLE servers DROP COLUMN IF EXISTS container_runtime;
