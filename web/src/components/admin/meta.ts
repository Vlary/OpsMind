// FE2：资源管理页共用的枚举选项与中文标签。
// 动作名固定见 docs/CONTRACT.md 9.7，不得自行新增。

import type { components } from '../../api/schema';

export type AuthMethod = components['schemas']['AuthMethod'];
export type DatabaseEngine = components['schemas']['DatabaseEngine'];
export type ProviderKind = components['schemas']['ProviderKind'];
export type SSLMode = components['schemas']['SSLMode'];
export type TargetOS = components['schemas']['TargetOS'];

export interface SelectOption<T extends string = string> {
  value: T;
  label: string;
}

export const AUTH_METHOD_OPTIONS: SelectOption<AuthMethod>[] = [
  { value: 'password', label: '密码认证' },
  { value: 'private_key', label: '私钥认证' },
  { value: 'password_ntlm', label: 'WinRM 密码（NTLM）' },
  { value: 'password_basic', label: 'WinRM 密码（Basic）' },
];

export const AUTH_METHOD_LABELS: Record<AuthMethod, string> = {
  password: '密码认证',
  private_key: '私钥认证',
  password_ntlm: 'WinRM 密码（NTLM）',
  password_basic: 'WinRM 密码（Basic）',
};

// 二期（F2）：服务器 os 选择。契约 docs/CONTRACT.md 2.7 双轨执行路由。
export const OS_OPTIONS: SelectOption<TargetOS>[] = [
  { value: 'linux', label: 'Linux' },
  { value: 'windows', label: 'Windows' },
];

export const OS_LABELS: Record<TargetOS, string> = {
  linux: 'Linux',
  windows: 'Windows',
  // 三期（N1/N3 代改，归属 FE2）：仅 device_kind=network_device 使用。
  network: '网络设备',
};

export function osLabel(os: TargetOS): string {
  return OS_LABELS[os] ?? os;
}

// 契约 2.6：os=linux 时 auth_method 只允许 password/private_key；
// os=windows 还允许 password_ntlm/password_basic（WinRM）。
const OS_AUTH_METHODS: Record<TargetOS, AuthMethod[]> = {
  linux: ['password', 'private_key'],
  windows: ['password', 'private_key', 'password_ntlm', 'password_basic'],
  // 三期（N3 代改，归属 FE2）：契约 2.6——network 仅允许 password（SSH
  // 密钥认证三期不支持）；表单层不直接消费该键（网络设备认证在
  // ServerFormDrawer 内归一），此处补齐以满足 TargetOS 全键 Record。
  network: ['password'],
};

export function authMethodOptionsForOS(os: TargetOS): SelectOption<AuthMethod>[] {
  return OS_AUTH_METHODS[os].map((value) => ({
    value,
    label: AUTH_METHOD_LABELS[value],
  }));
}

// 切换 os 后现存认证方式可能不合法（如 windows+NTLM 切回 linux），
// 后端会 400（契约 2.6）；前端在切换时先行归一，回退到 password。
export function normalizeAuthMethodForOS(os: TargetOS, method: AuthMethod): AuthMethod {
  return OS_AUTH_METHODS[os].includes(method) ? method : 'password';
}

export function isWinRMAuthMethod(method: AuthMethod): boolean {
  return method === 'password_ntlm' || method === 'password_basic';
}

export const ENGINE_OPTIONS: SelectOption<DatabaseEngine>[] = [
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'mysql', label: 'MySQL' },
  { value: 'sqlite', label: 'SQLite' },
  { value: 'mssql', label: 'SQL Server' },
  { value: 'redis', label: 'Redis' },
  // 三期（N3 代改，归属 FE2）：契约 2.6/4.4——engine 增补 mongodb。
  { value: 'mongodb', label: 'MongoDB' },
];

export const ENGINE_LABELS: Record<DatabaseEngine, string> = {
  postgres: 'PostgreSQL',
  mysql: 'MySQL',
  sqlite: 'SQLite',
  mssql: 'SQL Server',
  redis: 'Redis',
  // 三期（N3 代改，归属 FE2）：契约 2.6/4.4。
  mongodb: 'MongoDB',
};

export const DEFAULT_PORTS: Record<DatabaseEngine, number> = {
  postgres: 5432,
  mysql: 3306,
  sqlite: 0,
  mssql: 1433,
  redis: 6379,
  // 三期（N3 代改，归属 FE2）：契约 4.4——创建未提供 port 时 27017。
  mongodb: 27017,
};

export const SSL_MODE_OPTIONS: SelectOption<SSLMode>[] = [
  { value: 'disable', label: 'disable' },
  { value: 'prefer', label: 'prefer' },
  { value: 'require', label: 'require' },
  { value: 'verify-ca', label: 'verify-ca' },
  { value: 'verify-full', label: 'verify-full' },
];

export const PROVIDER_KIND_OPTIONS: SelectOption<ProviderKind>[] = [
  { value: 'openai_compatible', label: 'OpenAI 兼容' },
  { value: 'anthropic', label: 'Anthropic' },
];

export const PROVIDER_KIND_LABELS: Record<ProviderKind, string> = {
  openai_compatible: 'OpenAI 兼容',
  anthropic: 'Anthropic',
};

export const KIND_DEFAULT_BASE_URL: Record<ProviderKind, string> = {
  openai_compatible: 'https://api.openai.com/v1',
  anthropic: 'https://api.anthropic.com',
};

// 单管理员系统：actor_type=user 的操作者即管理员，标签直接写「管理员」。
export const ACTOR_TYPE_LABELS: Record<string, string> = {
  user: '管理员',
  model: '模型',
  system: '系统',
};

export const RESOURCE_TYPE_OPTIONS: SelectOption[] = [
  { value: 'server', label: '服务器' },
  { value: 'database', label: '数据库' },
  { value: 'provider', label: '供应商' },
  { value: 'conversation', label: '会话' },
  { value: 'command', label: '命令' },
  { value: 'approval', label: '审批' },
  { value: 'tool_call', label: '工具调用' },
  { value: 'user', label: '用户' },
  { value: 'session', label: '会话令牌' },
  { value: 'terminal_session', label: '终端会话' },
  { value: 'rdp_session', label: 'RDP 会话' },
  { value: 'setting', label: '设置' },
  // 五期/六期资源类型（六期打磨 sprint 4）：后端 audit_logs 里真实存在的
  // resource_type，此前不在清单内——审计表「目标」列回落显示英文原码、
  // 「按目标类型筛选」下拉也搜不到。
  { value: 'monitor', label: '监控采集' },
  { value: 'alert_rule', label: '告警规则' },
  { value: 'alert_event', label: '告警事件' },
  { value: 'notification_channel', label: '通知渠道' },
  { value: 'diagnosis_session', label: '诊断会话' },
  { value: 'inspection_report', label: '巡检报告' },
  { value: 'batch_job', label: '批量任务' },
];

export const RESOURCE_TYPE_LABELS: Record<string, string> = Object.fromEntries(
  RESOURCE_TYPE_OPTIONS.map((option) => [option.value, option.label]),
);

export const AUDIT_ACTIONS: SelectOption[] = [
  { value: 'auth.login_succeeded', label: '登录成功' },
  { value: 'auth.login_failed', label: '登录失败' },
  { value: 'auth.logout', label: '退出登录' },
  { value: 'auth.rate_limited', label: '登录限流' },
  { value: 'auth.password_changed', label: '修改密码成功' },
  { value: 'auth.password_change_failed', label: '修改密码失败' },
  { value: 'server.create', label: '新增服务器' },
  { value: 'server.update', label: '更新服务器' },
  { value: 'server.delete', label: '删除服务器' },
  { value: 'server.test', label: '测试服务器' },
  { value: 'server.host_key_trusted', label: '信任主机密钥' },
  { value: 'server.host_key_changed', label: '主机密钥变化' },
  // 四期 p4 sprint 2（C1/C2）：契约 9.7 新增唯一审计动作 container.action
  //（resource_type=server；console 审批 decide 批准后执行与 container_action
  // 工具执行两条路径落审计，containers_handlers.go / runner_container.go）。
  { value: 'container.action', label: '容器操作' },
  { value: 'database.create', label: '新增数据库' },
  { value: 'database.update', label: '更新数据库' },
  { value: 'database.delete', label: '删除数据库' },
  { value: 'database.test', label: '测试数据库' },
  // 三期 p3 sprint 1：SQL →「数据库查询」——mongodb 的 JSON 查询同走
  // database.query 动作（databases.go handleQueryDatabase），「SQL 查询」
  // 与三期「JSON 查询」抽屉口径不一致（SqlConsole 已去 SQL 化）；新标签
  // 同时覆盖 SQL 与 JSON 两种查询口径。
  // p3 sprint 2：query_rejected 同源对齐——mongodb 的 JSON 命令被只读
  // 守卫拒绝（MONGO_PARAM_NOT_WHITELISTED 等）同走 database.query_rejected
  // 审计（databases.go 拒绝路径），「SQL 查询被拒绝」对 JSON 命令文档
  // 语义错位，sprint 1 漏改此条。
  { value: 'database.query', label: '数据库查询' },
  { value: 'database.query_rejected', label: '数据库查询被拒绝' },
  { value: 'provider.create', label: '新增供应商' },
  { value: 'provider.update', label: '更新供应商' },
  { value: 'provider.delete', label: '删除供应商' },
  { value: 'provider.test', label: '测试供应商' },
  { value: 'provider.models_refresh', label: '刷新模型列表' },
  { value: 'conversation.create', label: '新建会话' },
  { value: 'conversation.rename', label: '重命名会话' },
  { value: 'conversation.delete', label: '删除会话' },
  { value: 'chat.started', label: '开始回答' },
  { value: 'chat.finished', label: '回答完成' },
  { value: 'chat.failed', label: '回答失败' },
  { value: 'chat.cancelled', label: '回答取消' },
  { value: 'tool.call', label: '工具调用' },
  { value: 'tool.result', label: '工具结果' },
  { value: 'tool.error', label: '工具错误' },
  // 六期打磨 sprint 5（走查 medium）：`command.approved` 此前同样是死选项
  // ——后端 0 处写入。本轮按契约 9.7 在 approvals.Decide（chat/console）与
  // batchx.OnApprovalDecided（source=batch）补写该动作，选项保留并生效。
  // `command.preview` 则确实没有语义归属：命令预览统一走 `risk.preview`
  //（/api/ops/preview-risk），全仓无任何 `command.preview` 写入点。它从
  // 筛选下拉移除（选中必然 0 行），中文标签仍在 AUDIT_ACTION_LABELS_RESERVED
  // 里保留，历史行不回落英文原码。
  { value: 'command.approved', label: '命令批准' },
  { value: 'command.denied', label: '命令拒绝' },
  { value: 'command.forbidden', label: '命令禁止' },
  { value: 'command.executed', label: '命令执行' },
  { value: 'approval.created', label: '创建审批' },
  { value: 'approval.approved', label: '审批通过' },
  { value: 'approval.denied', label: '审批拒绝' },
  { value: 'approval.expired', label: '审批过期' },
  { value: 'approval.stream_cancelled', label: '审批随流取消' },
  { value: 'session.start', label: '会话开始' },
  { value: 'session.end', label: '会话结束' },
  { value: 'session.input_summary', label: '终端输入摘要' },
  { value: 'risk.preview', label: '风险预览' },
  // 六期打磨 sprint 4（走查 medium）：五期/六期新增的审计动作此前全部缺位
  // ——后端确在写（grep `Action:` 逐条核对），前端不在清单内导致审计页
  // 「动作」列回落显示英文原码、动作筛选下拉也搜不到（实测输入「设置」
  // 「批量」命中 0 项）。清单口径对齐 CONTRACT 9.7 五期/六期段。
  { value: 'settings.update', label: '设置变更' },
  { value: 'metric.collect', label: '指标采集' },
  { value: 'alert_rule.create', label: '新增告警规则' },
  { value: 'alert_rule.update', label: '更新告警规则' },
  { value: 'alert_rule.delete', label: '删除告警规则' },
  { value: 'alert.triggered', label: '告警触发' },
  { value: 'alert.resolved', label: '告警恢复' },
  { value: 'alert.notified', label: '告警通知' },
  { value: 'notification.create', label: '新增通知渠道' },
  { value: 'notification.update', label: '更新通知渠道' },
  { value: 'notification.delete', label: '删除通知渠道' },
  { value: 'notification.test', label: '测试通知渠道' },
  { value: 'diagnosis.started', label: '诊断开始' },
  { value: 'diagnosis.completed', label: '诊断完成' },
  { value: 'inspection.started', label: '巡检开始' },
  { value: 'inspection.completed', label: '巡检完成' },
  { value: 'batch.created', label: '创建批量任务' },
  { value: 'batch.cancelled', label: '取消批量任务' },
];

const AUDIT_ACTION_LABELS: Record<string, string> = Object.fromEntries(
  AUDIT_ACTIONS.map((option) => [option.value, option.label]),
);

/**
 * 契约 9.7 列出、但当前后端没有写入点的动作：只保留中文标签、不进筛选
 * 下拉（选中必然 0 行 = 死筛选项，六期打磨 sprint 5 走查）。
 *
 * `command.preview`：命令预览在实现里统一落 `risk.preview`
 *（/api/ops/preview-risk 的 handlePreviewRisk）——全仓无 `command.preview`
 * 写入点；若将来恢复写入，把它移回 AUDIT_ACTIONS 即可。
 */
const AUDIT_ACTION_LABELS_RESERVED: Record<string, string> = {
  'command.preview': '命令预览',
};

export function actionLabel(action: string): string {
  return AUDIT_ACTION_LABELS[action] ?? AUDIT_ACTION_LABELS_RESERVED[action] ?? action;
}

export function resourceTypeLabel(resourceType: string): string {
  return RESOURCE_TYPE_LABELS[resourceType] ?? resourceType;
}

export function actorTypeLabel(actorType: string): string {
  return ACTOR_TYPE_LABELS[actorType] ?? actorType;
}

export function engineLabel(engine: DatabaseEngine): string {
  return ENGINE_LABELS[engine] ?? engine;
}

export function providerKindLabel(kind: ProviderKind): string {
  return PROVIDER_KIND_LABELS[kind] ?? kind;
}
