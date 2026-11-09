-- +goose Up
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text NOT NULL UNIQUE CHECK (username = 'admin'),
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    ip           text NOT NULL DEFAULT '',
    user_agent   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE servers (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    host                text NOT NULL CHECK (length(host) BETWEEN 1 AND 255),
    port                int NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
    username            text NOT NULL CHECK (length(username) BETWEEN 1 AND 100),
    auth_method         text NOT NULL CHECK (auth_method IN ('password', 'private_key')),
    password_enc        text NULL,
    private_key_enc     text NULL,
    passphrase_enc      text NULL,
    tags                text[] NOT NULL DEFAULT '{}',
    auto_exec_enabled   boolean NOT NULL DEFAULT false,
    host_key_fingerprint text NOT NULL DEFAULT '',
    host_key_public     text NOT NULL DEFAULT '',
    host_key_verified_at timestamptz NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (auth_method = 'password' AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
        OR
        (auth_method = 'private_key' AND private_key_enc IS NOT NULL AND password_enc IS NULL)
    )
);

CREATE INDEX servers_created_at_idx ON servers (created_at DESC);
CREATE INDEX servers_tags_idx ON servers USING GIN (tags);

CREATE TABLE databases (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    engine        text NOT NULL CHECK (engine IN ('postgres', 'mysql', 'sqlite')),
    host          text NOT NULL DEFAULT '',
    port          int NOT NULL DEFAULT 0 CHECK (port BETWEEN 0 AND 65535),
    database_name text NOT NULL DEFAULT '',
    username      text NOT NULL DEFAULT '',
    password_enc  text NULL,
    sqlite_path   text NOT NULL DEFAULT '',
    ssl_mode      text NOT NULL DEFAULT 'prefer'
                  CHECK (ssl_mode IN ('disable', 'prefer', 'require', 'verify-ca', 'verify-full')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (engine IN ('postgres', 'mysql')
         AND length(host) > 0 AND port > 0 AND length(database_name) > 0 AND length(username) > 0
         AND sqlite_path = '')
        OR
        (engine = 'sqlite'
         AND host = '' AND port = 0 AND database_name = '' AND username = ''
         AND password_enc IS NULL AND sqlite_path LIKE '/%')
    )
);

CREATE INDEX databases_created_at_idx ON databases (created_at DESC);

CREATE TABLE providers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    preset        text NOT NULL CHECK (preset IN (
                      'openai', 'deepseek', 'dashscope', 'moonshot', 'zhipu',
                      'anthropic', 'gemini', 'ollama', 'custom')),
    kind          text NOT NULL CHECK (kind IN ('openai_compatible', 'anthropic')),
    base_url      text NOT NULL CHECK (length(base_url) BETWEEN 1 AND 2048),
    api_key_enc   text NULL,
    api_key_hint  text NOT NULL DEFAULT '',
    default_model text NOT NULL CHECK (length(default_model) > 0),
    models        jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(models) = 'array'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX providers_created_at_idx ON providers (created_at);

CREATE TABLE conversations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title           text NOT NULL DEFAULT '新会话' CHECK (length(title) BETWEEN 1 AND 200),
    provider_id     uuid NULL REFERENCES providers (id) ON DELETE SET NULL,
    model           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_message_at timestamptz NOT NULL DEFAULT now(),
    CHECK (provider_id IS NULL OR length(model) > 0)
);

CREATE INDEX conversations_last_message_at_idx ON conversations (last_message_at DESC);
CREATE INDEX conversations_provider_id_idx ON conversations (provider_id);

CREATE TABLE messages (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq               bigint NOT NULL GENERATED ALWAYS AS IDENTITY UNIQUE,
    conversation_id   uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    turn_id           uuid NOT NULL,
    role              text NOT NULL CHECK (role IN ('user', 'assistant')),
    content           text NOT NULL DEFAULT '',
    reasoning         text NOT NULL DEFAULT '',
    status            text NOT NULL CHECK (status IN ('complete', 'error')),
    error_code        text NOT NULL DEFAULT '',
    model             text NOT NULL DEFAULT '',
    prompt_tokens     int NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens int NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX messages_conversation_seq_idx ON messages (conversation_id, seq DESC);
CREATE INDEX messages_conversation_created_idx ON messages (conversation_id, created_at DESC);
CREATE INDEX messages_turn_id_idx ON messages (turn_id);

CREATE TABLE tool_calls (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq              bigint NOT NULL GENERATED ALWAYS AS IDENTITY UNIQUE,
    conversation_id  uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    turn_id          uuid NOT NULL,
    provider_call_id text NOT NULL,
    name             text NOT NULL,
    arguments        jsonb NOT NULL DEFAULT '{}'::jsonb,
    result           jsonb NULL,
    risk_level       text NOT NULL DEFAULT 'safe'
                     CHECK (risk_level IN ('safe', 'needs_approval', 'forbidden')),
    status           text NOT NULL CHECK (status IN (
                         'started', 'awaiting_approval', 'succeeded', 'failed',
                         'denied', 'expired', 'awaiting_user')),
    error_code       text NOT NULL DEFAULT '',
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz NULL,
    UNIQUE (conversation_id, turn_id, provider_call_id)
);

CREATE INDEX tool_calls_conversation_seq_idx ON tool_calls (conversation_id, seq DESC);
CREATE INDEX tool_calls_turn_id_idx ON tool_calls (turn_id);

CREATE TABLE approvals (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    tool_call_id    uuid NOT NULL UNIQUE REFERENCES tool_calls (id) ON DELETE CASCADE,
    server_id       uuid NULL REFERENCES servers (id) ON DELETE SET NULL,
    command         text NOT NULL,
    risk_level      text NOT NULL CHECK (risk_level IN ('safe', 'needs_approval')),
    reason          text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    requested_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    decided_at      timestamptz NULL,
    decided_by      uuid NULL REFERENCES users (id) ON DELETE SET NULL,
    note            text NOT NULL DEFAULT ''
);

CREATE INDEX approvals_status_expires_idx ON approvals (status, expires_at);
CREATE INDEX approvals_conversation_requested_idx ON approvals (conversation_id, requested_at DESC);

CREATE TABLE audit_logs (
    id              bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    created_at      timestamptz NOT NULL DEFAULT now(),
    actor_type      text NOT NULL CHECK (actor_type IN ('user', 'system', 'model')),
    actor_id        text NOT NULL DEFAULT '',
    action          text NOT NULL,
    resource_type   text NOT NULL DEFAULT '',
    resource_id     text NOT NULL DEFAULT '',
    request_id      text NOT NULL DEFAULT '',
    conversation_id uuid NULL,
    server_id       uuid NULL,
    ip              text NOT NULL DEFAULT '',
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb,
    prev_hash       text NOT NULL CHECK (length(prev_hash) = 64),
    hash            text NOT NULL CHECK (length(hash) = 64)
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_server_created_idx ON audit_logs (server_id, created_at DESC);
CREATE INDEX audit_logs_conversation_created_idx ON audit_logs (conversation_id, created_at DESC);
CREATE INDEX audit_logs_action_created_idx ON audit_logs (action, created_at DESC);
CREATE INDEX audit_logs_resource_created_idx ON audit_logs (resource_type, resource_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_logs_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only';
END $$;
-- +goose StatementEnd

CREATE TRIGGER audit_logs_no_update_delete
BEFORE UPDATE OR DELETE ON audit_logs
FOR EACH ROW EXECUTE FUNCTION audit_logs_immutable();

CREATE TABLE settings (
    key        text PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 100),
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS audit_logs;
DROP FUNCTION IF EXISTS audit_logs_immutable();
DROP TABLE IF EXISTS approvals;
DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS databases;
DROP TABLE IF EXISTS servers;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
