import { describe, expect, it } from 'vitest';

import { PERMS, type Permission } from '../../../lib/rbacPermissions';
// canManageProviders 为 ChatPage 的导出纯函数（仅作单测钩子）：
// 无 provider:manage 时三处供应商空态不得渲染 /providers 入口。
import { canManageProviders } from '../ChatPage';

// 契约 12.2 的角色权限夹具（与 internal/authz/authz.go 一致）。
const OPERATOR_PERMS: Permission[] = [
  PERMS.chat,
  PERMS.assetsRead,
  PERMS.connectionTest,
  PERMS.execRead,
  PERMS.execWrite,
  PERMS.metricsRead,
  PERMS.inspections,
  PERMS.terminal,
];

const ADMIN_PERMS = Object.values(PERMS) as Permission[];
const APPROVER_PERMS: Permission[] = [PERMS.assetsRead, PERMS.approvalsRead, PERMS.approvalsDecide];

describe('canManageProviders（供应商空态入口权限）', () => {
  it('运维工程师没有 provider:manage：不渲染 /providers 链接/按钮', () => {
    // 与走查证据一致：operator 登录 permissions 不含 provider:manage。
    expect(canManageProviders(OPERATOR_PERMS)).toBe(false);
  });

  it('审计员/审批者同样没有 provider:manage', () => {
    expect(canManageProviders([PERMS.auditRead])).toBe(false);
    expect(canManageProviders(APPROVER_PERMS)).toBe(false);
  });

  it('管理员有 provider:manage：保留原「配置模型 / 去模型管理」CTA', () => {
    expect(canManageProviders(ADMIN_PERMS)).toBe(true);
    expect(canManageProviders([PERMS.providers])).toBe(true);
  });

  it('权限未加载完成（空集合）按无权限处理，不提前渲染入口', () => {
    expect(canManageProviders([])).toBe(false);
  });
});
