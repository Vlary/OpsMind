import { describe, expect, it } from 'vitest';

import {
  ACTOR_TYPE_LABELS,
  AUDIT_ACTIONS,
  AUTH_METHOD_LABELS,
  AUTH_METHOD_OPTIONS,
  DEFAULT_PORTS,
  ENGINE_LABELS,
  ENGINE_OPTIONS,
  KIND_DEFAULT_BASE_URL,
  OS_LABELS,
  OS_OPTIONS,
  PROVIDER_KIND_LABELS,
  PROVIDER_KIND_OPTIONS,
  RESOURCE_TYPE_LABELS,
  RESOURCE_TYPE_OPTIONS,
  SSL_MODE_OPTIONS,
  actionLabel,
  actorTypeLabel,
  engineLabel,
  osLabel,
  providerKindLabel,
  resourceTypeLabel,
} from './meta';

// 资源管理页枚举选项与中文标签的一致性矩阵（此前无任何单测）：
// OPTIONS 与 LABELS 必须同源一致，杜绝下拉值与表格标签对不上的回归。

describe('OPTIONS 与 LABELS 一致性', () => {
  it('AUTH_METHOD_OPTIONS 与 AUTH_METHOD_LABELS 键值一致', () => {
    for (const option of AUTH_METHOD_OPTIONS) {
      expect(AUTH_METHOD_LABELS[option.value]).toBe(option.label);
    }
    expect(Object.keys(AUTH_METHOD_LABELS)).toHaveLength(AUTH_METHOD_OPTIONS.length);
  });

  it('ENGINE_OPTIONS 与 ENGINE_LABELS 键值一致', () => {
    for (const option of ENGINE_OPTIONS) {
      expect(ENGINE_LABELS[option.value]).toBe(option.label);
    }
    expect(Object.keys(ENGINE_LABELS)).toHaveLength(ENGINE_OPTIONS.length);
  });

  it('PROVIDER_KIND_OPTIONS 与 PROVIDER_KIND_LABELS 键值一致', () => {
    for (const option of PROVIDER_KIND_OPTIONS) {
      expect(PROVIDER_KIND_LABELS[option.value]).toBe(option.label);
    }
    expect(Object.keys(PROVIDER_KIND_LABELS)).toHaveLength(PROVIDER_KIND_OPTIONS.length);
  });

  it('RESOURCE_TYPE_LABELS 由 RESOURCE_TYPE_OPTIONS 派生且一致', () => {
    for (const option of RESOURCE_TYPE_OPTIONS) {
      expect(RESOURCE_TYPE_LABELS[option.value]).toBe(option.label);
    }
  });

  it('动作与资源类型选项值无重复', () => {
    expect(new Set(AUDIT_ACTIONS.map((option) => option.value)).size).toBe(AUDIT_ACTIONS.length);
    expect(new Set(RESOURCE_TYPE_OPTIONS.map((option) => option.value)).size).toBe(
      RESOURCE_TYPE_OPTIONS.length,
    );
  });

  it('选项 label 均非空', () => {
    for (const option of [...AUDIT_ACTIONS, ...RESOURCE_TYPE_OPTIONS, ...SSL_MODE_OPTIONS]) {
      expect(option.label.length).toBeGreaterThan(0);
    }
  });
});

describe('actionLabel 审计动作中文映射', () => {
  it('认证类动作映射中文', () => {
    expect(actionLabel('auth.login_succeeded')).toBe('登录成功');
    expect(actionLabel('auth.login_failed')).toBe('登录失败');
    expect(actionLabel('auth.logout')).toBe('退出登录');
    expect(actionLabel('auth.rate_limited')).toBe('登录限流');
  });

  // 第 10 轮打磨：改密审计动作此前不在 AUDIT_ACTIONS 清单内，审计页动作列
  // 显示英文原串且动作筛选下拉搜不到（走查库 psql DISTINCT action 实测
  // auth.password_changed / auth.password_change_failed 均在库）。后端实现
  // 见 internal/httpapi/auth_handlers.go，此处补齐前端标签与下拉选项。
  it('改密动作映射中文（第 10 轮）', () => {
    expect(actionLabel('auth.password_changed')).toBe('修改密码成功');
    expect(actionLabel('auth.password_change_failed')).toBe('修改密码失败');
    expect(AUDIT_ACTIONS.some((option) => option.value === 'auth.password_changed')).toBe(true);
    expect(AUDIT_ACTIONS.some((option) => option.value === 'auth.password_change_failed')).toBe(
      true,
    );
  });

  it('资产 CRUD 动作映射中文', () => {
    expect(actionLabel('server.create')).toBe('新增服务器');
    expect(actionLabel('server.delete')).toBe('删除服务器');
    expect(actionLabel('database.update')).toBe('更新数据库');
    expect(actionLabel('provider.test')).toBe('测试供应商');
    expect(actionLabel('provider.models_refresh')).toBe('刷新模型列表');
  });

  it('会话与对话动作映射中文', () => {
    expect(actionLabel('conversation.create')).toBe('新建会话');
    expect(actionLabel('conversation.rename')).toBe('重命名会话');
    expect(actionLabel('chat.started')).toBe('开始回答');
    expect(actionLabel('chat.finished')).toBe('回答完成');
    expect(actionLabel('chat.failed')).toBe('回答失败');
    expect(actionLabel('chat.cancelled')).toBe('回答取消');
  });

  it('工具与审批动作映射中文', () => {
    expect(actionLabel('tool.call')).toBe('工具调用');
    expect(actionLabel('tool.result')).toBe('工具结果');
    expect(actionLabel('approval.created')).toBe('创建审批');
    expect(actionLabel('approval.approved')).toBe('审批通过');
    expect(actionLabel('approval.denied')).toBe('审批拒绝');
    expect(actionLabel('approval.expired')).toBe('审批过期');
    expect(actionLabel('approval.stream_cancelled')).toBe('审批随流取消');
  });

  it('命令动作映射中文', () => {
    expect(actionLabel('command.preview')).toBe('命令预览');
    expect(actionLabel('command.approved')).toBe('命令批准');
    expect(actionLabel('command.denied')).toBe('命令拒绝');
    expect(actionLabel('command.forbidden')).toBe('命令禁止');
    expect(actionLabel('command.executed')).toBe('命令执行');
  });

  // 六期打磨 sprint 5（走查 medium）：「命令预览」是永空的死筛选项——
  // 后端全仓无 `command.preview` 写入点（预览统一落 risk.preview），
  // 下拉选它必然「没有符合条件的审计记录」。选项从筛选清单移除，
  // 中文标签保留（历史行不回落英文原码）；「命令批准」则相反：本轮在
  // approvals.Decide 与 batchx.OnApprovalDecided 补写了写入点，必须留在
  // 清单里。
  it('死筛选项清理：command.preview 不进下拉但仍有中文，command.approved 保留', () => {
    expect(AUDIT_ACTIONS.some((option) => option.value === 'command.preview')).toBe(false);
    expect(actionLabel('command.preview')).toBe('命令预览');
    expect(AUDIT_ACTIONS.some((option) => option.value === 'command.approved')).toBe(true);
    expect(actionLabel('command.approved')).toBe('命令批准');
    // 下拉里每一项都必须真有写入点（command.approved 已由后端补写）。
    const values = AUDIT_ACTIONS.map((option) => option.value);
    expect(values).toEqual(expect.arrayContaining(['command.approved', 'command.denied']));
  });

  it('数据库查询动作映射中文（含被拒；p3 sprint 1 起 query 去 SQL 化、sprint 2 补齐 rejected）', () => {
    expect(actionLabel('database.query')).toBe('数据库查询');
    expect(actionLabel('database.query_rejected')).toBe('数据库查询被拒绝');
  });

  // 四期 p4 sprint 2：契约 9.7 四期唯一新增审计动作 container.action
  //（console 审批 decide 批准后执行与 container_action 工具执行两路径落审计，
  // containers_handlers.go:393 / runner_container.go:404）。此前不在
  // AUDIT_ACTIONS 清单内，审计页动作列显示英文原码且筛选下拉搜不到。
  it('容器操作动作映射中文（p4 sprint 2）', () => {
    expect(actionLabel('container.action')).toBe('容器操作');
    expect(AUDIT_ACTIONS.some((option) => option.value === 'container.action')).toBe(true);
  });

  it('每个 AUDIT_ACTIONS 值都能取回自己的 label', () => {
    for (const option of AUDIT_ACTIONS) {
      expect(actionLabel(option.value)).toBe(option.label);
    }
  });

  // 六期打磨 sprint 4（走查 medium）：五期/六期 18 个动作后端确实在写
  // （CONTRACT 9.7 五期段 + 六期 settings.update；逐条 grep `Action:` 核对），
  // 但此前不在 AUDIT_ACTIONS 里——审计页动作列回落原码、筛选下拉搜不到
  // （实测输入「设置」「批量」→ 0 项）。此用例钉死清单覆盖。
  it('五期/六期动作全部有中文标签且在筛选清单内（p6 sprint 4）', () => {
    const phase56: Array<[string, string]> = [
      ['settings.update', '设置变更'],
      ['metric.collect', '指标采集'],
      ['alert_rule.create', '新增告警规则'],
      ['alert_rule.update', '更新告警规则'],
      ['alert_rule.delete', '删除告警规则'],
      ['alert.triggered', '告警触发'],
      ['alert.resolved', '告警恢复'],
      ['alert.notified', '告警通知'],
      ['notification.create', '新增通知渠道'],
      ['notification.update', '更新通知渠道'],
      ['notification.delete', '删除通知渠道'],
      ['notification.test', '测试通知渠道'],
      ['diagnosis.started', '诊断开始'],
      ['diagnosis.completed', '诊断完成'],
      ['inspection.started', '巡检开始'],
      ['inspection.completed', '巡检完成'],
      ['batch.created', '创建批量任务'],
      ['batch.cancelled', '取消批量任务'],
    ];
    for (const [value, label] of phase56) {
      expect(actionLabel(value)).toBe(label);
      expect(AUDIT_ACTIONS.some((option) => option.value === value)).toBe(true);
      // 动作列不得回落原码（原码仍作为副行展示，见 AuditPage）。
      expect(actionLabel(value)).not.toBe(value);
    }
  });

  // 审计下拉的搜索走 optionFilterProp="label"（AuditPage.tsx），中文关键词
  // 必须能命中——走查实测「设置」「批量」都返回空列表。
  it('动作筛选的中文关键词能命中（p6 sprint 4）', () => {
    const byKeyword = (keyword: string) =>
      AUDIT_ACTIONS.filter((option) => option.label.includes(keyword));
    expect(byKeyword('设置').map((option) => option.value)).toContain('settings.update');
    expect(byKeyword('批量').map((option) => option.value)).toEqual(
      expect.arrayContaining(['batch.created', 'batch.cancelled']),
    );
    expect(byKeyword('告警').length).toBeGreaterThan(0);
    expect(byKeyword('巡检').map((option) => option.value)).toEqual(
      expect.arrayContaining(['inspection.started', 'inspection.completed']),
    );
  });

  it('五期/六期资源类型有中文标签（p6 sprint 4 目标列）', () => {
    const cases: Array<[string, string]> = [
      ['monitor', '监控采集'],
      ['alert_rule', '告警规则'],
      ['alert_event', '告警事件'],
      ['notification_channel', '通知渠道'],
      ['diagnosis_session', '诊断会话'],
      ['inspection_report', '巡检报告'],
      ['batch_job', '批量任务'],
    ];
    for (const [value, label] of cases) {
      expect(resourceTypeLabel(value)).toBe(label);
      expect(RESOURCE_TYPE_OPTIONS.some((option) => option.value === value)).toBe(true);
    }
  });

  it('未知动作原样返回（不丢信息）', () => {
    expect(actionLabel('future.action')).toBe('future.action');
    expect(actionLabel('')).toBe('');
  });
});

describe('resourceTypeLabel / actorTypeLabel / engineLabel / providerKindLabel', () => {
  it('资源类型映射中文', () => {
    expect(resourceTypeLabel('server')).toBe('服务器');
    expect(resourceTypeLabel('database')).toBe('数据库');
    expect(resourceTypeLabel('provider')).toBe('供应商');
    expect(resourceTypeLabel('conversation')).toBe('会话');
    expect(resourceTypeLabel('tool_call')).toBe('工具调用');
    expect(resourceTypeLabel('session')).toBe('会话令牌');
    expect(resourceTypeLabel('setting')).toBe('设置');
  });

  it('未知资源类型原样返回', () => {
    expect(resourceTypeLabel('mystery')).toBe('mystery');
  });

  it('操作者类型映射：user 是管理员（单管理员系统）', () => {
    expect(actorTypeLabel('user')).toBe('管理员');
    expect(actorTypeLabel('model')).toBe('模型');
    expect(actorTypeLabel('system')).toBe('系统');
    expect(actorTypeLabel('unknown')).toBe('unknown');
  });

  it('数据库引擎标签', () => {
    expect(engineLabel('postgres')).toBe('PostgreSQL');
    expect(engineLabel('mysql')).toBe('MySQL');
    expect(engineLabel('sqlite')).toBe('SQLite');
    expect(engineLabel('mssql')).toBe('SQL Server');
    expect(engineLabel('redis')).toBe('Redis');
  });

  it('os 标签（二期 F2）', () => {
    expect(osLabel('linux')).toBe('Linux');
    expect(osLabel('windows')).toBe('Windows');
    expect(OS_OPTIONS.map((option) => option.value)).toEqual(['linux', 'windows']);
    for (const option of OS_OPTIONS) {
      expect(OS_LABELS[option.value]).toBe(option.label);
    }
  });

  it('供应商类型标签', () => {
    expect(providerKindLabel('openai_compatible')).toBe('OpenAI 兼容');
    expect(providerKindLabel('anthropic')).toBe('Anthropic');
  });
});

describe('默认端口与 Base URL 常量', () => {
  it('DEFAULT_PORTS：postgres 5432 / mysql 3306 / sqlite 0（无端口语义）', () => {
    expect(DEFAULT_PORTS.postgres).toBe(5432);
    expect(DEFAULT_PORTS.mysql).toBe(3306);
    expect(DEFAULT_PORTS.sqlite).toBe(0);
  });

  it('DEFAULT_PORTS 二期引擎：mssql 1433 / redis 6379（契约 2.6）', () => {
    expect(DEFAULT_PORTS.mssql).toBe(1433);
    expect(DEFAULT_PORTS.redis).toBe(6379);
  });

  it('KIND_DEFAULT_BASE_URL：openai_compatible 带 /v1、anthropic 不带', () => {
    expect(KIND_DEFAULT_BASE_URL.openai_compatible).toBe('https://api.openai.com/v1');
    expect(KIND_DEFAULT_BASE_URL.anthropic).toBe('https://api.anthropic.com');
  });

  it('SSL_MODE_OPTIONS 覆盖 postgres 全部 sslmode', () => {
    const values = SSL_MODE_OPTIONS.map((option) => option.value);
    expect(values).toEqual(['disable', 'prefer', 'require', 'verify-ca', 'verify-full']);
  });

  it('ACTOR_TYPE_LABELS 键集合与后端契约一致', () => {
    expect(Object.keys(ACTOR_TYPE_LABELS).sort()).toEqual(['model', 'system', 'user']);
  });
});
