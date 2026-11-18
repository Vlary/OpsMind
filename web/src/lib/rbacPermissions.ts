// 多用户 RBAC（CONTRACT.md 12.1/12.2）在前端的只读镜像。
//
// 权限串与角色落库值全部来自服务端：本文件只做「固定枚举 → 中文名」与
// 「权限集合判断」，不参与任何授权决策。客户端拿到的 permissions 由
// `GET /api/auth/me` 下发（12.4 第 1 条：服务端计算、客户端不得提交或覆盖），
// 这里的能力仅用于隐藏不可用入口与给出 403 解释；越权请求仍由服务端
// requirePermission 拒绝并写 authz.denied。

import type { components } from '../api/schema';

export type Role = components['schemas']['Role'];
export type Permission = components['schemas']['Permission'];

// 权限全集（x:x），顺序与 CONTRACT.md 12.1 的 19 条一致。
export const PERMS = {
  /** 发起对话与管理自己的会话 */
  chat: 'chat:use',
  /** 查看资产元数据 */
  assetsRead: 'asset:read',
  /** 创建/修改/删除资产 */
  assetsWrite: 'asset:write',
  /** 连接测试 */
  connectionTest: 'asset:test',
  /** 只读执行与查询 */
  execRead: 'exec:read',
  /** 发起命令与写操作 */
  execWrite: 'exec:write',
  /** 指标与告警只读 */
  metricsRead: 'monitor:read',
  /** 告警规则/渠道/采集器配置 */
  alertsWrite: 'monitor:manage',
  /** 巡检任务与报告 */
  inspections: 'inspection:manage',
  /** JIT 终端/RDP 与自己录制 */
  terminal: 'terminal:use',
  /** 审批列表/详情/有界上下文 */
  approvalsRead: 'approval:read',
  /** 批准/拒绝审批与策略变更 */
  approvalsDecide: 'approval:decide',
  /** 请求审批策略变更 */
  policyWrite: 'approval:policy',
  /** 审计/回放/录制跨用户只读 */
  auditRead: 'audit:read',
  /** 供应商与 API Key 管理 */
  providers: 'provider:manage',
  /** 查看用户 */
  usersRead: 'user:read',
  /** 创建用户/改角色/启停/吊销会话 */
  usersWrite: 'user:write',
  /** 签发一次性密码重置令牌 */
  usersReset: 'user:credential_reset',
  /** 读取 pprof */
  pprof: 'debug:read',
} as const satisfies Record<string, Permission>;

export const ROLE_LABELS: Record<Role, string> = {
  admin: '管理员',
  operator: '运维工程师',
  approver: '审批者',
  auditor: '审计员',
};

/**
 * 角色职责说明（00012 评审 medium）：创建/改角色时只列角色名，管理员
 * 无法知道审批者不能发起对话、审计员只能只读。文案与契约 12.2 一致。
 */
export const ROLE_DESCRIPTIONS: Record<Role, string> = {
  admin: '全部权限：资产与用户管理、命令执行、审批、审计、策略变更。',
  operator: '发起对话、执行命令与调用工具；不能审批、不能管理用户/策略。',
  approver: '只审批不发起：查看并决定他人的命令/终端授权/策略变更，不能发起对话或执行命令。',
  auditor: '只读治理：审计日志、跨用户回放/录制；不能执行、审批或开终端。',
};

export const ROLE_OPTIONS: readonly { value: Role; label: string }[] = (
  ['admin', 'operator', 'approver', 'auditor'] as const
).map((value) => ({ value, label: ROLE_LABELS[value] ?? value }));

export function roleLabel(role: Role | null | undefined): string {
  if (!role) {
    return '未知角色';
  }
  return ROLE_LABELS[role] ?? role;
}

// 角色徽标颜色（antd Tag 预设色）。后台管理面用暖色（admin）与大权限面
// 区分；只影响展示，不参与授权。
const ROLE_TAG_COLORS: Record<Role, string> = {
  admin: 'volcano',
  operator: 'blue',
  approver: 'purple',
  auditor: 'default',
};

export function roleTagColor(role: Role | null | undefined): string {
  return role ? (ROLE_TAG_COLORS[role] ?? 'default') : 'default';
}

/**
 * 是否有任一所需权限。
 *
 * 导航与路由守卫按「任一命中」判定（例如 /recordings：本人终端录制需要
 * terminal 权限，审计员看跨用户录制需要 audit:read）。`required` 为空
 * 表示所有已登录用户可见。
 */
export function hasAnyPermission(
  permissions: readonly Permission[],
  required?: readonly Permission[],
): boolean {
  if (!required || required.length === 0) {
    return true;
  }
  const granted = new Set<Permission>(permissions);
  return required.some((permission) => granted.has(permission));
}
