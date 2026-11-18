-- +goose Up
-- 00012：多用户 RBAC（docs/CONTRACT.md 第 12 节；api/openapi.yaml users/auth）。
--
-- 设计要点：
--   * users 去掉 username='admin' 硬约束，加 role/enabled/display_name/email/
--     must_change_password/created_by/last_login_at；每用户恰一个角色。
--   * conversations 加 user_id（会话私有；历史行回填唯一 admin）。
--   * approvals 加 requested_by（发起者）与 legacy（迁移前历史行）、terminal/policy
--     grant 列；source 扩 terminal/policy；执行层从此只使用 command，不再读
--     edited_command。迁移时把 pending + edited_command<>'' 全部置 expired。
--   * batch_jobs.created_by 回填后 NOT NULL；diagnosis_sessions/inspection_jobs 加
--     created_by，供 owner-or-admin 写守卫。
--   * 新增 user_tokens（invite/reset 一次性 token，经独立通道投递）与 audit_outbox
--     （状态变更审计与业务同事务落 outbox，独立 worker 串行写哈希链）。
--   * users_last_admin_guard：commit 时断言至少一个 enabled admin。
--
-- 不修改任何已应用的 00001-00011 迁移。

-- ---------------------------------------------------------------- users
ALTER TABLE users
    ADD COLUMN role text NOT NULL DEFAULT 'operator'
        CHECK (role IN ('admin', 'operator', 'approver', 'auditor')),
    ADD COLUMN enabled boolean NOT NULL DEFAULT true,
    ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK (length(display_name) <= 100),
    ADD COLUMN email text NOT NULL DEFAULT ''
        CHECK (email = '' OR email ~ '^[^@[:space:]]+@[^@[:space:]]+$'),
    ADD COLUMN must_change_password boolean NOT NULL DEFAULT false,
    ADD COLUMN created_by uuid NULL REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN last_login_at timestamptz NULL;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_username_check;
ALTER TABLE users
    ADD CONSTRAINT users_username_format_check
    CHECK (username ~ '^[a-z0-9][a-z0-9._-]{0,63}$');

-- 存量行：唯一用户 admin 即管理员；display_name 回填用户名。
UPDATE users SET role = 'admin', enabled = true WHERE username = 'admin';
UPDATE users SET display_name = username WHERE display_name = '';

-- ------------------------------------------------------------ user_tokens
-- invite：POST /api/users 已创建 enabled=false 的用户行，token 行 user_id 为 NULL，
--         带 username/email/role，兑换时按 username 找到并启用该行；
-- reset：目标已存在（user_id 非空）。token 只存 SHA-256，明文只在投递通道出现。
CREATE TABLE user_tokens (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text NOT NULL CHECK (kind IN ('invite', 'reset')),
    user_id          uuid NULL REFERENCES users (id) ON DELETE CASCADE,
    username         text NOT NULL DEFAULT '',
    email            text NOT NULL DEFAULT '',
    role             text NULL CHECK (role IS NULL OR role IN ('admin', 'operator', 'approver', 'auditor')),
    token_hash       bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expires_at       timestamptz NOT NULL,
    used_at          timestamptz NULL,
    created_by       uuid NULL REFERENCES users (id) ON DELETE SET NULL,
    creator_ip       text NOT NULL DEFAULT '',
    delivered_channel text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (kind = 'invite' AND user_id IS NULL AND username <> '' AND email <> '' AND role IS NOT NULL)
        OR
        (kind = 'reset' AND user_id IS NOT NULL)
    )
);

CREATE INDEX user_tokens_user_kind_idx ON user_tokens (user_id, kind);
CREATE INDEX user_tokens_expires_idx ON user_tokens (expires_at);

-- -------------------------------------------------------- conversations
ALTER TABLE conversations
    ADD COLUMN user_id uuid NULL REFERENCES users (id) ON DELETE RESTRICT;

UPDATE conversations
SET user_id = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE user_id IS NULL;

ALTER TABLE conversations ALTER COLUMN user_id SET NOT NULL;

CREATE INDEX conversations_user_last_message_idx
    ON conversations (user_id, last_message_at DESC, id DESC);

-- ------------------------------------------------------------- batch_jobs
-- 批量审批的 requested_by 依赖 created_by；先回填再收紧。
UPDATE batch_jobs
SET created_by = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE created_by IS NULL;

ALTER TABLE batch_jobs DROP CONSTRAINT IF EXISTS batch_jobs_created_by_fkey;
ALTER TABLE batch_jobs ALTER COLUMN created_by SET NOT NULL;
ALTER TABLE batch_jobs
    ADD CONSTRAINT batch_jobs_created_by_fkey
    FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE RESTRICT;

-- ---------------------------------------------- diagnosis / inspection owner
ALTER TABLE diagnosis_sessions
    ADD COLUMN created_by uuid NULL REFERENCES users (id) ON DELETE RESTRICT;
UPDATE diagnosis_sessions
SET created_by = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE created_by IS NULL;
CREATE INDEX diagnosis_sessions_created_by_idx
    ON diagnosis_sessions (created_by, created_at DESC);

ALTER TABLE inspection_jobs
    ADD COLUMN created_by uuid NULL REFERENCES users (id) ON DELETE RESTRICT;
UPDATE inspection_jobs
SET created_by = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE created_by IS NULL;
CREATE INDEX inspection_jobs_created_by_idx
    ON inspection_jobs (created_by, created_at DESC);

-- -------------------------------------------------------------- approvals
ALTER TABLE approvals
    ADD COLUMN requested_by uuid NULL REFERENCES users (id) ON DELETE RESTRICT,
    ADD COLUMN legacy boolean NOT NULL DEFAULT false,
    ADD COLUMN grant_kind text NULL CHECK (grant_kind IN ('term', 'rdp')),
    ADD COLUMN grant_expires_at timestamptz NULL,
    ADD COLUMN consumed_at timestamptz NULL,
    ADD COLUMN policy_from text NULL CHECK (policy_from IS NULL OR policy_from IN ('all', 'standard', 'high', 'none')),
    ADD COLUMN policy_to text NULL CHECK (policy_to IS NULL OR policy_to IN ('all', 'standard', 'high', 'none')),
    ADD COLUMN break_glass boolean NOT NULL DEFAULT false;

-- 迁移前存量行全部 legacy=true：只用于豁免历史自审/历史改写；应用不得写 true。
UPDATE approvals SET legacy = true;

-- 历史 pending 改写行直接作废，防止升级瞬间按 edited_command 执行。
UPDATE approvals
SET status = 'expired', note = 'cancelled:pre_rbac_edit'
WHERE status = 'pending' AND edited_command <> '';

-- requested_by 回填：chat 取会话 owner；batch 取 job created_by；其余取唯一 admin。
UPDATE approvals a
SET requested_by = c.user_id
FROM conversations c
WHERE a.conversation_id = c.id AND a.requested_by IS NULL;

UPDATE approvals a
SET requested_by = b.created_by
FROM batch_results r
JOIN batch_jobs b ON b.id = r.batch_job_id
WHERE r.approval_id = a.id AND a.requested_by IS NULL;

UPDATE approvals
SET requested_by = (SELECT id FROM users WHERE username = 'admin' LIMIT 1)
WHERE requested_by IS NULL;

ALTER TABLE approvals ALTER COLUMN requested_by SET NOT NULL;

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_source_check;
ALTER TABLE approvals
    ADD CONSTRAINT approvals_source_check
    CHECK (source IN ('chat', 'console', 'batch', 'terminal', 'policy'));

ALTER TABLE approvals
    ADD CONSTRAINT approvals_sod_check
    CHECK (legacy OR decided_by IS NULL OR requested_by <> decided_by);
ALTER TABLE approvals
    ADD CONSTRAINT approvals_edited_command_legacy_check
    CHECK (edited_command = '' OR legacy);
ALTER TABLE approvals
    ADD CONSTRAINT approvals_terminal_grant_check
    CHECK (source <> 'terminal' OR (grant_kind IS NOT NULL AND grant_expires_at IS NOT NULL));
ALTER TABLE approvals
    ADD CONSTRAINT approvals_policy_change_check
    CHECK (source <> 'policy' OR (policy_from IS NOT NULL AND policy_to IS NOT NULL));

CREATE INDEX approvals_requested_by_status_idx
    ON approvals (requested_by, status);

-- ------------------------------------------------------------ audit_outbox
-- 业务状态变更与 outbox 行同事务提交；独立 worker 角色串行追加 audit_logs。
CREATE TABLE audit_outbox (
    id           bigserial PRIMARY KEY,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz NULL,
    attempts     int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error   text NOT NULL DEFAULT ''
);

CREATE INDEX audit_outbox_pending_idx
    ON audit_outbox (created_at)
    WHERE processed_at IS NULL;

-- ------------------------------------------------- last enabled admin guard
-- 应用层先 pg_advisory_xact_lock(hashtext('rbac.admin_guard')) 做友好 409；
-- 本 trigger 是 commit 时的硬兜底（PostgreSQL 不允许 SELECT count(*) FOR UPDATE）。
-- goose 的 SQL 解析器按分号切分；PL/pgSQL 函数体必须显式括起。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION users_last_admin_guard() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin' AND enabled) THEN
        RAISE EXCEPTION 'last enabled admin cannot be removed'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER users_last_admin_guard
    AFTER UPDATE OR DELETE ON users
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION users_last_admin_guard();

-- +goose Down
-- Down 仅限测试/评估；生产回滚只允许发布旧二进制并手工评估数据丢失（契约 10.4）。
DROP TRIGGER IF EXISTS users_last_admin_guard ON users;
DROP FUNCTION IF EXISTS users_last_admin_guard();
DROP TABLE IF EXISTS audit_outbox;
DROP TABLE IF EXISTS user_tokens;

DROP INDEX IF EXISTS diagnosis_sessions_created_by_idx;
ALTER TABLE diagnosis_sessions DROP COLUMN IF EXISTS created_by;
DROP INDEX IF EXISTS inspection_jobs_created_by_idx;
ALTER TABLE inspection_jobs DROP COLUMN IF EXISTS created_by;

ALTER TABLE batch_jobs DROP CONSTRAINT IF EXISTS batch_jobs_created_by_fkey;
ALTER TABLE batch_jobs ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE batch_jobs
    ADD CONSTRAINT batch_jobs_created_by_fkey
    FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;

-- terminal/policy 行无法容纳于旧 source CHECK，先删除（测试库）。
DELETE FROM approvals WHERE source IN ('terminal', 'policy');
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_sod_check;
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_edited_command_legacy_check;
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_terminal_grant_check;
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_policy_change_check;
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_requested_by_fkey;
ALTER TABLE approvals DROP COLUMN IF EXISTS requested_by;
ALTER TABLE approvals DROP COLUMN IF EXISTS legacy;
ALTER TABLE approvals DROP COLUMN IF EXISTS grant_kind;
ALTER TABLE approvals DROP COLUMN IF EXISTS grant_expires_at;
ALTER TABLE approvals DROP COLUMN IF EXISTS consumed_at;
ALTER TABLE approvals DROP COLUMN IF EXISTS policy_from;
ALTER TABLE approvals DROP COLUMN IF EXISTS policy_to;
ALTER TABLE approvals DROP COLUMN IF EXISTS break_glass;
DROP INDEX IF EXISTS approvals_requested_by_status_idx;
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_source_check;
ALTER TABLE approvals
    ADD CONSTRAINT approvals_source_check
    CHECK (source IN ('chat', 'console', 'batch'));

DROP INDEX IF EXISTS conversations_user_last_message_idx;
ALTER TABLE conversations DROP COLUMN IF EXISTS user_id;

-- 恢复 username='admin' 前必须清理非 admin 用户；conversations/approvals 的
-- user 引用已在上方删除/解除。
DELETE FROM users WHERE username <> 'admin';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_username_format_check;
ALTER TABLE users ADD CONSTRAINT users_username_check CHECK (username = 'admin');
ALTER TABLE users DROP COLUMN IF EXISTS role;
ALTER TABLE users DROP COLUMN IF EXISTS enabled;
ALTER TABLE users DROP COLUMN IF EXISTS display_name;
ALTER TABLE users DROP COLUMN IF EXISTS email;
ALTER TABLE users DROP COLUMN IF EXISTS must_change_password;
ALTER TABLE users DROP COLUMN IF EXISTS created_by;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;
