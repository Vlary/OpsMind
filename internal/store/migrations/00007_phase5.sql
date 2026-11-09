-- +goose Up
-- 五期：监控告警 / AI 诊断 / 定时巡检 / 批量操作（契约 2.10/4.16-4.26）。
--   任务原文写作 00005_phase5.sql；因 00005/00006 已被三期/四期占用且迁移版本
--   必须连续、不得改已应用文件（4.15，同四期 00006 的更名先例），五期迁移
--   定名 00007_phase5.sql。
--
--   指标存储依据（五期调研实测，postgres:16-alpine，100 台 x 30 天 x 1440 点
--   = 432 万行）：单索引 (created_at, server_id) 即可覆盖单机等值+范围查询
--   （0.265ms/近 1h）与跨机窗口聚合（2.56ms）；不建第二索引；「每台最新值」
--   用 server_metrics_latest 物化表（UPSERT, PK=server_id），DISTINCT ON 全表
--   扫（11.27s）不可用；保留策略 = 每日 DELETE created_at < now()-30d（实测
--   42.6ms/天），无需分区。
--
--   approvals.source 扩枚举 'batch'：批量操作的逐台审批（契约 2.10 约定 5）
--   ——decide handler 对 source IN ('console','batch') 且批准时同步逐字节
--   执行 approval.command（四期 console 语义原样扩展），batch 额外回填
--   batch_results（经 batch_results.approval_id 反查）。

-- ---------------------------------------------------------------- metrics

CREATE TABLE server_metrics (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    server_id   uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    cpu_pct     real NOT NULL CHECK (cpu_pct BETWEEN 0 AND 100),
    mem_pct     real NOT NULL CHECK (mem_pct BETWEEN 0 AND 100),
    disk_pct    real NOT NULL CHECK (disk_pct BETWEEN 0 AND 100),
    load_1      real NOT NULL CHECK (load_1 >= 0),
    net_in_bps  bigint NOT NULL DEFAULT 0 CHECK (net_in_bps >= 0),
    net_out_bps bigint NOT NULL DEFAULT 0 CHECK (net_out_bps >= 0)
);

-- 唯一索引（实测结论：不需要第二索引 (server_id, created_at)）。
CREATE INDEX server_metrics_created_server_idx ON server_metrics(created_at, server_id);

-- 每台最新值物化表（告警评估与仪表盘第一屏；UPSERT ON CONFLICT (server_id)）。
CREATE TABLE server_metrics_latest (
    server_id   uuid PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    cpu_pct     real NOT NULL CHECK (cpu_pct BETWEEN 0 AND 100),
    mem_pct     real NOT NULL CHECK (mem_pct BETWEEN 0 AND 100),
    disk_pct    real NOT NULL CHECK (disk_pct BETWEEN 0 AND 100),
    load_1      real NOT NULL CHECK (load_1 >= 0),
    net_in_bps  bigint NOT NULL DEFAULT 0 CHECK (net_in_bps >= 0),
    net_out_bps bigint NOT NULL DEFAULT 0 CHECK (net_out_bps >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------- notification_channels

-- 渠道配置（webhook URL / 飞书钉钉 webhook / SMTP 凭据与收件人）整体
-- cryptox 加密存 config_enc；明文只在投递瞬间存在于内存。config_hint 是
-- 非敏感掩码提示（如 "https://open.feishu.cn/***(24)"/"3 收件人"）。
CREATE TABLE notification_channels (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    kind        text NOT NULL CHECK (kind IN ('webhook','feishu','dingtalk','email')),
    config_enc  text NOT NULL,
    config_hint text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------- alert_rules

CREATE TABLE alert_rules (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    metric             text NOT NULL CHECK (metric IN ('cpu_pct','mem_pct','disk_pct','load_1','net_in_bps','net_out_bps')),
    comparator         text NOT NULL CHECK (comparator IN ('gt','lt')),
    threshold          numeric NOT NULL CHECK (threshold >= 0),
    duration_windows   int NOT NULL DEFAULT 3 CHECK (duration_windows BETWEEN 1 AND 30),
    cooldown_seconds   int NOT NULL DEFAULT 300 CHECK (cooldown_seconds BETWEEN 60 AND 86400),
    severity           text NOT NULL DEFAULT 'warning' CHECK (severity IN ('info','warning','critical')),
    silent_until       timestamptz NULL,
    notify_channel_id  uuid NULL REFERENCES notification_channels(id) ON DELETE SET NULL,
    enabled            boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (metric NOT IN ('cpu_pct','mem_pct','disk_pct') OR threshold <= 100)
);

-- ------------------------------------------------------------- alert_events

CREATE TABLE alert_events (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rule_id           uuid NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    server_id         uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    status            text NOT NULL DEFAULT 'firing' CHECK (status IN ('firing','resolved')),
    severity          text NOT NULL CHECK (severity IN ('info','warning','critical')),
    value             real NOT NULL,
    message           text NOT NULL DEFAULT '',
    fired_at          timestamptz NOT NULL,
    resolved_at       timestamptz NULL,
    last_notified_at  timestamptz NULL,
    notify_status     text NOT NULL DEFAULT 'none' CHECK (notify_status IN ('none','pending','sent','failed','skipped')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- 同一 (rule, server) 至多一条 firing（评估器据此判断重复触发）。
CREATE UNIQUE INDEX alert_events_open_uniq ON alert_events(rule_id, server_id) WHERE status = 'firing';
CREATE INDEX alert_events_status_fired_idx ON alert_events(status, fired_at DESC);
CREATE INDEX alert_events_server_idx ON alert_events(server_id, fired_at DESC);

-- -------------------------------------------------------- diagnosis_sessions

-- 状态机（契约 2.10 约定 6）：pending -> diagnosing -> report -> awaiting_fix
-- -> done | expired；执行错误 -> failed（终态）。expires_at 在进入
-- awaiting_fix 时设置（+30m），到期由扫描器置 expired。
CREATE TABLE diagnosis_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_event_id  bigint NULL REFERENCES alert_events(id) ON DELETE SET NULL,
    server_id       uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    conversation_id uuid NULL REFERENCES conversations(id) ON DELETE SET NULL,
    trigger         text NOT NULL CHECK (trigger IN ('alert','manual','chat')),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','diagnosing','report','awaiting_fix','done','expired','failed')),
    report          jsonb NULL CHECK (report IS NULL OR jsonb_typeof(report)='object'),
    error_code      text NOT NULL DEFAULT '',
    started_at      timestamptz NULL,
    finished_at     timestamptz NULL,
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '30 minutes',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX diagnosis_sessions_status_created_idx ON diagnosis_sessions(status, created_at DESC);
CREATE INDEX diagnosis_sessions_server_idx ON diagnosis_sessions(server_id, created_at DESC);

-- ------------------------------------------------------------ inspection_jobs

-- cron 为五段标准 cron（分 时 日 月 周，UTC）；target_tags 为空数组 = 全部
-- device_kind=server 的资产；network_device 不参与巡检（契约 2.10 约定 7）。
CREATE TABLE inspection_jobs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    cron               text NOT NULL,
    target_tags        text[] NOT NULL DEFAULT '{}',
    template           text NOT NULL DEFAULT 'basic' CHECK (template IN ('basic','full')),
    notify_enabled     boolean NOT NULL DEFAULT false,
    notify_channel_id  uuid NULL REFERENCES notification_channels(id) ON DELETE SET NULL,
    enabled            boolean NOT NULL DEFAULT true,
    last_run_at        timestamptz NULL,
    next_run_at        timestamptz NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- --------------------------------------------------------- inspection_reports

CREATE TABLE inspection_reports (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id        uuid NULL REFERENCES inspection_jobs(id) ON DELETE CASCADE,
    trigger       text NOT NULL CHECK (trigger IN ('cron','manual','chat')),
    server_count  int NOT NULL CHECK (server_count >= 0),
    avg_score     int NOT NULL CHECK (avg_score BETWEEN 0 AND 100),
    anomaly_count int NOT NULL DEFAULT 0 CHECK (anomaly_count >= 0),
    started_at    timestamptz NOT NULL,
    finished_at   timestamptz NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX inspection_reports_created_idx ON inspection_reports(created_at DESC);
CREATE INDEX inspection_reports_job_idx ON inspection_reports(job_id, created_at DESC);

CREATE TABLE inspection_report_items (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    report_id    uuid NOT NULL REFERENCES inspection_reports(id) ON DELETE CASCADE,
    server_id    uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    score        int NOT NULL CHECK (score BETWEEN 0 AND 100),
    anomaly_count int NOT NULL DEFAULT 0 CHECK (anomaly_count >= 0),
    anomalies    jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(anomalies)='array'),
    raw_output   text NOT NULL DEFAULT '',
    truncated    boolean NOT NULL DEFAULT false,
    duration_ms  int NOT NULL DEFAULT 0,
    error_code   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX inspection_report_items_report_idx ON inspection_report_items(report_id);
CREATE INDEX inspection_report_items_server_idx ON inspection_report_items(server_id, created_at DESC);

-- ---------------------------------------------------------------- batch

-- server_ids 为 uuid 数组（无外键——PostgreSQL 数组不支持 FK）：执行时对已
-- 删除的 server 按 SERVER_NOT_FOUND 跳过；上限 50 台（契约 11 节防 OOM）。
-- description 是自然语言原文（页面/对话入口均保留原句）。
CREATE TABLE batch_jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    description     text NOT NULL DEFAULT '' CHECK (length(description) BETWEEN 0 AND 2000),
    command         text NOT NULL CHECK (length(command) BETWEEN 1 AND 16384),
    server_ids      uuid[] NOT NULL CHECK (array_length(server_ids, 1) BETWEEN 1 AND 50),
    source          text NOT NULL DEFAULT 'console' CHECK (source IN ('console','chat')),
    conversation_id uuid NULL REFERENCES conversations(id) ON DELETE SET NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','partial','done','failed','cancelled')),
    created_by      uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    finished_at     timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX batch_jobs_created_idx ON batch_jobs(created_at DESC);

CREATE TABLE batch_results (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_job_id uuid NOT NULL REFERENCES batch_jobs(id) ON DELETE CASCADE,
    server_id    uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    risk_level   text NOT NULL CHECK (risk_level IN ('safe','needs_approval','forbidden')),
    rule_id      text NOT NULL DEFAULT '',
    approval_id  uuid NULL REFERENCES approvals(id) ON DELETE SET NULL,
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','awaiting_approval','executed','failed','denied','expired','skipped','cancelled')),
    exit_code    int NULL,
    output       text NOT NULL DEFAULT '',
    truncated    boolean NOT NULL DEFAULT false,
    error_code   text NOT NULL DEFAULT '',
    duration_ms  int NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX batch_results_job_idx ON batch_results(batch_job_id, created_at);
-- decide handler 执行 batch 审批后按 approval_id 反查回填（契约 2.10 约定 5）。
CREATE INDEX batch_results_approval_idx ON batch_results(approval_id);

-- ------------------------------------------------------- approvals 扩枚举

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_source_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_source_check
    CHECK (source IN ('chat', 'console', 'batch'));

-- +goose Down
-- Down 仅限测试/评估（契约 10.4）；先删 batch 审批行（其关联的
-- batch_results.approval_id 由 ON DELETE SET NULL 兜底为 NULL），
-- 否则收缩后的 approvals_source_check 会被遗留 batch 行违反
--（G8：Down 再 Up 必须可用——批量数据在场时同样成立）。
DROP TABLE IF EXISTS batch_results;
DROP TABLE IF EXISTS batch_jobs;
DROP TABLE IF EXISTS inspection_report_items;
DROP TABLE IF EXISTS inspection_reports;
DROP TABLE IF EXISTS inspection_jobs;
DROP TABLE IF EXISTS diagnosis_sessions;
DROP TABLE IF EXISTS alert_events;
DROP TABLE IF EXISTS alert_rules;
DROP TABLE IF EXISTS notification_channels;
DROP TABLE IF EXISTS server_metrics_latest;
DROP TABLE IF EXISTS server_metrics;
DELETE FROM approvals WHERE source = 'batch';
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_source_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_source_check
    CHECK (source IN ('chat', 'console'));
