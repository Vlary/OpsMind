-- +goose Up
-- 00013：RBAC 默认角色最小权限加固。
--
-- 00012 给 users.role 的 DEFAULT 是 'operator'（含 execute:write）。
-- 应用路径总会显式指定 role，但任何绕过应用的 INSERT（运维手工 SQL、
-- 迁移脚本遗漏）都会静默获得命令执行权。默认值改为权限最小的 'auditor'
-- 是失败关闭：漏写 role 只会得到只读治理身份，不会多出执行面。
--
-- 说明：00012 之前的 users 表有 CHECK (username = 'admin')，历史数据只可能
-- 有一行 admin，不存在「其余历史用户被静默授予 operator」的存量
-- 行；这里仅加固默认值，不需要数据回填。
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'auditor';

-- +goose Down
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'operator';
