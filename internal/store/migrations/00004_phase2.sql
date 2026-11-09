-- +goose Up
-- 二期：Windows 执行通道（servers.os + WinRM 认证）、数据库引擎扩展
-- （mssql/redis）、堡垒机式终端会话与 RDP 网关会话表。

-- ---------------------------------------------------------------------------
-- servers：os 路由 + WinRM 认证
--   os='linux'  → 仅 password / private_key（走 sshx）
--   os='windows' → password / private_key（Windows OpenSSH，走 sshx）
--                 或 password_ntlm / password_basic（走 winrmx，端口 winrm_port）
-- ---------------------------------------------------------------------------

ALTER TABLE servers ADD COLUMN os text NOT NULL DEFAULT 'linux'
    CHECK (os IN ('linux', 'windows'));

ALTER TABLE servers ADD COLUMN winrm_port int NOT NULL DEFAULT 5985
    CHECK (winrm_port BETWEEN 1 AND 65535);

ALTER TABLE servers DROP CONSTRAINT servers_auth_method_check;
ALTER TABLE servers ADD CONSTRAINT servers_auth_method_check
    CHECK (auth_method IN ('password', 'private_key', 'password_ntlm', 'password_basic'));

ALTER TABLE servers DROP CONSTRAINT servers_check;
ALTER TABLE servers ADD CONSTRAINT servers_check CHECK (
    ((os = 'linux' AND auth_method IN ('password', 'private_key'))
     OR (os = 'windows'))
    AND
    ((auth_method IN ('password', 'password_ntlm', 'password_basic')
      AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
     OR
     (auth_method = 'private_key' AND private_key_enc IS NOT NULL AND password_enc IS NULL))
);

-- ---------------------------------------------------------------------------
-- databases：engine 扩展 mssql（SQL Server）与 redis
--   mssql：host/port/instance_name 二选一直连（instance 非空时 port=0，
--          走 SQL Server Browser 解析），database_name、username、password 必填
--   redis：host/port 必填，redis_db 0..1023，username/password 可空
-- ---------------------------------------------------------------------------

ALTER TABLE databases ADD COLUMN instance_name text NOT NULL DEFAULT ''
    CHECK (length(instance_name) BETWEEN 0 AND 100);

ALTER TABLE databases ADD COLUMN redis_db int NOT NULL DEFAULT 0
    CHECK (redis_db BETWEEN 0 AND 1023);

ALTER TABLE databases DROP CONSTRAINT databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'mysql', 'sqlite', 'mssql', 'redis'));

ALTER TABLE databases DROP CONSTRAINT databases_check;
ALTER TABLE databases ADD CONSTRAINT databases_check CHECK (
    (engine IN ('postgres', 'mysql')
     AND length(host) > 0 AND port > 0 AND length(database_name) > 0 AND length(username) > 0
     AND sqlite_path = '' AND instance_name = '' AND redis_db = 0)
    OR
    (engine = 'sqlite'
     AND host = '' AND port = 0 AND database_name = '' AND username = ''
     AND password_enc IS NULL AND sqlite_path LIKE '/%'
     AND instance_name = '' AND redis_db = 0)
    OR
    (engine = 'mssql'
     AND length(host) > 0 AND length(database_name) > 0 AND length(username) > 0
     AND password_enc IS NOT NULL AND sqlite_path = '' AND redis_db = 0
     AND ((instance_name = '' AND port BETWEEN 1 AND 65535)
          OR (instance_name <> '' AND port = 0)))
    OR
    (engine = 'redis'
     AND length(host) > 0 AND port BETWEEN 1 AND 65535
     AND database_name = '' AND username = '' AND sqlite_path = ''
     AND instance_name = '' AND redis_db BETWEEN 0 AND 1023)
);

-- ---------------------------------------------------------------------------
-- terminal_sessions：堡垒机式终端（SSH PTY + asciinema v2 录制）
-- ---------------------------------------------------------------------------

CREATE TABLE terminal_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id       uuid NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed')),
    cols            int NOT NULL CHECK (cols BETWEEN 10 AND 500),
    rows            int NOT NULL CHECK (rows BETWEEN 4 AND 200),
    ended_reason    text NOT NULL DEFAULT '' CHECK (ended_reason IN
                       ('', 'client_closed', 'idle_timeout', 'max_duration',
                        'server_shutdown', 'error')),
    recording_file  text NOT NULL DEFAULT '' CHECK (recording_file IN ('') OR recording_file ~ '^[0-9a-f-]{36}\.cast$'),
    bytes_written   bigint NOT NULL DEFAULT 0 CHECK (bytes_written >= 0),
    started_at      timestamptz NOT NULL DEFAULT now(),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX terminal_sessions_created_idx ON terminal_sessions (created_at DESC);
CREATE INDEX terminal_sessions_server_idx ON terminal_sessions (server_id, created_at DESC);
CREATE INDEX terminal_sessions_status_activity_idx ON terminal_sessions (status, last_activity_at);

-- ---------------------------------------------------------------------------
-- rdp_sessions：堡垒机式 RDP 网关（guacd 边车 + 凭据注入）
-- ---------------------------------------------------------------------------

CREATE TABLE rdp_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id       uuid NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed')),
    width           int NOT NULL CHECK (width BETWEEN 640 AND 3840),
    height          int NOT NULL CHECK (height BETWEEN 480 AND 2160),
    dpi             int NOT NULL DEFAULT 96 CHECK (dpi BETWEEN 72 AND 120),
    ended_reason    text NOT NULL DEFAULT '' CHECK (ended_reason IN
                       ('', 'client_closed', 'idle_timeout', 'max_duration',
                        'server_shutdown', 'error')),
    recording_file  text NOT NULL DEFAULT '' CHECK (recording_file IN ('') OR recording_file ~ '^[0-9a-f-]{36}\.guac$'),
    bytes_written   bigint NOT NULL DEFAULT 0 CHECK (bytes_written >= 0),
    started_at      timestamptz NOT NULL DEFAULT now(),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX rdp_sessions_created_idx ON rdp_sessions (created_at DESC);
CREATE INDEX rdp_sessions_server_idx ON rdp_sessions (server_id, created_at DESC);
CREATE INDEX rdp_sessions_status_activity_idx ON rdp_sessions (status, last_activity_at);

-- +goose Down
-- 回滚前必须清掉旧约束无法容纳的数据（二期引擎的库、Windows/WinRM 服务器
-- 及其级联的会话行）；契约 10.4：Down 仅限测试/评估，需接受数据丢失。
DROP TABLE IF EXISTS rdp_sessions;
DROP TABLE IF EXISTS terminal_sessions;
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_check;
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
DELETE FROM databases WHERE engine IN ('mssql', 'redis');
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'mysql', 'sqlite'));
ALTER TABLE databases ADD CONSTRAINT databases_check CHECK (
    (engine IN ('postgres', 'mysql')
     AND length(host) > 0 AND port > 0 AND length(database_name) > 0 AND length(username) > 0
     AND sqlite_path = '')
    OR
    (engine = 'sqlite'
     AND host = '' AND port = 0 AND database_name = '' AND username = ''
     AND password_enc IS NULL AND sqlite_path LIKE '/%')
);
ALTER TABLE databases DROP COLUMN IF EXISTS redis_db;
ALTER TABLE databases DROP COLUMN IF EXISTS instance_name;
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_check;
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_auth_method_check;
DELETE FROM servers WHERE os = 'windows' OR auth_method IN ('password_ntlm', 'password_basic');
ALTER TABLE servers ADD CONSTRAINT servers_auth_method_check
    CHECK (auth_method IN ('password', 'private_key'));
ALTER TABLE servers ADD CONSTRAINT servers_check CHECK (
    (auth_method = 'password' AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
    OR
    (auth_method = 'private_key' AND private_key_enc IS NOT NULL AND password_enc IS NULL)
);
ALTER TABLE servers DROP COLUMN IF EXISTS winrm_port;
ALTER TABLE servers DROP COLUMN IF EXISTS os;
