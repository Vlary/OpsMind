-- +goose Up
-- 三期：网络设备接入（servers：device_kind/netdev_brand/access_proto/
-- enable_secret_enc，os 枚举扩 'network'）与 MongoDB 引擎（databases：
-- auth_db 列 + engine 扩 'mongodb'）。无新表、无新索引（servers/databases
-- 列表全量查询，现有索引足够）。

-- ---------------------------------------------------------------------------
-- servers：网络设备三态扩展
--   device_kind='server'        → os ∈ (linux, windows)，netdev_brand 固定 ''，
--                                 access_proto 固定 'ssh'（二期行为不变）
--   device_kind='network_device' → os='network'，netdev_brand ∈ 五值，
--                                 access_proto ∈ (ssh, telnet)，auth_method
--                                 仅 'password'（密钥认证 v1 不支持，失败关闭）
-- enable_secret_enc：仅 network_device——enable/super 特权密码（cryptox 加密）
-- ---------------------------------------------------------------------------

ALTER TABLE servers ADD COLUMN device_kind text NOT NULL DEFAULT 'server'
    CHECK (device_kind IN ('server', 'network_device'));

ALTER TABLE servers ADD COLUMN netdev_brand text NOT NULL DEFAULT ''
    CHECK (netdev_brand IN ('', 'cisco', 'huawei', 'h3c', 'junos', 'generic'));

ALTER TABLE servers ADD COLUMN access_proto text NOT NULL DEFAULT 'ssh'
    CHECK (access_proto IN ('ssh', 'telnet'));

ALTER TABLE servers ADD COLUMN enable_secret_enc text NULL;

ALTER TABLE servers DROP CONSTRAINT servers_os_check;
ALTER TABLE servers ADD CONSTRAINT servers_os_check
    CHECK (os IN ('linux', 'windows', 'network'));

ALTER TABLE servers DROP CONSTRAINT servers_check;
ALTER TABLE servers ADD CONSTRAINT servers_check CHECK (
    (device_kind = 'server'
     AND os IN ('linux', 'windows')
     AND netdev_brand = ''
     AND access_proto = 'ssh'
     AND (os = 'windows' OR auth_method IN ('password', 'private_key'))
     AND (
       (auth_method IN ('password', 'password_ntlm', 'password_basic')
        AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
       OR
       (auth_method = 'private_key' AND private_key_enc IS NOT NULL AND password_enc IS NULL)
     ))
    OR
    (device_kind = 'network_device'
     AND os = 'network'
     AND netdev_brand IN ('cisco', 'huawei', 'h3c', 'junos', 'generic')
     AND access_proto IN ('ssh', 'telnet')
     AND auth_method = 'password'
     AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
);

-- ---------------------------------------------------------------------------
-- databases：engine 扩 mongodb（go.mongodb.org/mongo-driver v1.17.4）
--   host/port（handler 缺省填 27017）与 database_name 必填；username 与
--   password_enc 成对（同空=无认证部署）；auth_db 为 AuthSource（空=admin）；
--   不提供 uri 直连形态（契约 2.6：连接参数全部走结构化列）。
-- ---------------------------------------------------------------------------

ALTER TABLE databases ADD COLUMN auth_db text NOT NULL DEFAULT ''
    CHECK (length(auth_db) BETWEEN 0 AND 100);

ALTER TABLE databases DROP CONSTRAINT databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'mysql', 'sqlite', 'mssql', 'redis', 'mongodb'));

ALTER TABLE databases DROP CONSTRAINT databases_check;
ALTER TABLE databases ADD CONSTRAINT databases_check CHECK (
    (engine IN ('postgres', 'mysql')
     AND length(host) > 0 AND port > 0 AND length(database_name) > 0 AND length(username) > 0
     AND sqlite_path = '' AND instance_name = '' AND redis_db = 0 AND auth_db = '')
    OR
    (engine = 'sqlite'
     AND host = '' AND port = 0 AND database_name = '' AND username = ''
     AND password_enc IS NULL AND sqlite_path LIKE '/%'
     AND instance_name = '' AND redis_db = 0 AND auth_db = '')
    OR
    (engine = 'mssql'
     AND length(host) > 0 AND length(database_name) > 0 AND length(username) > 0
     AND password_enc IS NOT NULL AND sqlite_path = '' AND redis_db = 0 AND auth_db = ''
     AND ((instance_name = '' AND port BETWEEN 1 AND 65535)
          OR (instance_name <> '' AND port = 0)))
    OR
    (engine = 'redis'
     AND length(host) > 0 AND port BETWEEN 1 AND 65535
     AND database_name = '' AND username = '' AND sqlite_path = ''
     AND instance_name = '' AND redis_db BETWEEN 0 AND 1023 AND auth_db = '')
    OR
    (engine = 'mongodb'
     AND length(host) > 0 AND port BETWEEN 1 AND 65535 AND length(database_name) > 0
     AND ((username = '' AND password_enc IS NULL)
          OR (length(username) > 0 AND password_enc IS NOT NULL))
     AND sqlite_path = '' AND instance_name = '' AND redis_db = 0)
);

-- +goose Down
-- 回滚前必须清掉旧约束无法容纳的数据（三期网络设备服务器、mongodb 数据库）；
-- 契约 10.4：Down 仅限测试/评估，需接受数据丢失。
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_check;
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
DELETE FROM databases WHERE engine = 'mongodb';
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'mysql', 'sqlite', 'mssql', 'redis'));
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
ALTER TABLE databases DROP COLUMN IF EXISTS auth_db;

ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_check;
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_os_check;
-- 先删三期行（terminal/rdp_sessions 由外键级联），再恢复二期约束形态
DELETE FROM servers WHERE device_kind = 'network_device' OR os = 'network';
ALTER TABLE servers ADD CONSTRAINT servers_os_check
    CHECK (os IN ('linux', 'windows'));
ALTER TABLE servers ADD CONSTRAINT servers_check CHECK (
    ((os = 'linux' AND auth_method IN ('password', 'private_key'))
     OR (os = 'windows'))
    AND
    ((auth_method IN ('password', 'password_ntlm', 'password_basic')
      AND password_enc IS NOT NULL AND private_key_enc IS NULL AND passphrase_enc IS NULL)
     OR
     (auth_method = 'private_key' AND private_key_enc IS NOT NULL AND password_enc IS NULL))
);
ALTER TABLE servers DROP COLUMN IF EXISTS enable_secret_enc;
ALTER TABLE servers DROP COLUMN IF EXISTS access_proto;
ALTER TABLE servers DROP COLUMN IF EXISTS netdev_brand;
ALTER TABLE servers DROP COLUMN IF EXISTS device_kind;
