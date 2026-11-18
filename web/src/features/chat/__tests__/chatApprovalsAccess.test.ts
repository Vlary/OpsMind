import { describe, expect, it } from 'vitest';

import { PERMS, type Permission } from '../../../lib/rbacPermissions';
// approvalsQueryEnabled 为 ChatPage 私有纯函数，export 仅作单测钩子
//（无行为变化）。RBAC 走查 sprint 3：运维工程师/审计员没有
// approvals:read，会话页不得请求 GET /api/approvals（服务端 403 是
// 有意的权限矩阵，见 internal/httpapi/rbac_enforcement_test.go）。
import { approvalsQueryEnabled } from '../ChatPage';

const operatorPermissions: Permission[] = [
  PERMS.chat,
  PERMS.assetsRead,
  PERMS.execRead,
  PERMS.execWrite,
];

const approverPermissions: Permission[] = [
  PERMS.assetsRead,
  PERMS.approvalsRead,
  PERMS.approvalsDecide,
];

describe('approvalsQueryEnabled', () => {
  it('运维工程师（无 approvals:read）不发审批列表请求', () => {
    expect(approvalsQueryEnabled('conv-1', operatorPermissions)).toBe(false);
  });

  it('审计员（只读治理、无 approvals:read）不发审批列表请求', () => {
    expect(approvalsQueryEnabled('conv-1', [PERMS.auditRead, PERMS.terminal])).toBe(false);
  });

  it('审批者（有 approvals:read）在会话页请求审批列表', () => {
    expect(approvalsQueryEnabled('conv-1', approverPermissions)).toBe(true);
  });

  it('管理员等仅有 approvals:read 的集合同样启用', () => {
    expect(approvalsQueryEnabled('conv-1', [PERMS.approvalsRead])).toBe(true);
  });

  it('新会话（无 conversationId）不发请求，即使有权限', () => {
    expect(approvalsQueryEnabled(undefined, approverPermissions)).toBe(false);
    expect(approvalsQueryEnabled('', approverPermissions)).toBe(false);
  });

  it('权限未加载完成（空集合）时不预发请求', () => {
    expect(approvalsQueryEnabled('conv-1', [])).toBe(false);
  });
});
