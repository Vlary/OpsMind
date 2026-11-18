import { describe, expect, it } from 'vitest';

import { canAccessPath } from '../../../lib/rbac';
import { PERMS, type Permission } from '../../../lib/rbacPermissions';
import { canOpenChatFromCatalog, replayStepLinks } from '../replayLinks';

// 契约 12.2 的角色权限夹具（与 internal/authz/authz.go 一致）。
const ADMIN_PERMS = Object.values(PERMS) as Permission[];

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

const APPROVER_PERMS: Permission[] = [PERMS.assetsRead, PERMS.approvalsRead, PERMS.approvalsDecide];

const AUDITOR_PERMS: Permission[] = [PERMS.auditRead];

describe('replayStepLinks（execution 深链按目标页权限收敛）', () => {
  it('审计员只有 audit:read：三个深链全部不渲染', () => {
    expect(replayStepLinks(AUDITOR_PERMS)).toEqual({
      diagnosis: false,
      inspection: false,
      batch: false,
    });
  });

  it('运维工程师/管理员保留可用深链', () => {
    expect(replayStepLinks(OPERATOR_PERMS)).toEqual({
      diagnosis: true,
      inspection: true,
      batch: true,
    });
    expect(replayStepLinks(ADMIN_PERMS)).toEqual({
      diagnosis: true,
      inspection: true,
      batch: true,
    });
  });

  it('审批者无告警/巡检/批量权限：三个深链全部不渲染', () => {
    expect(replayStepLinks(APPROVER_PERMS)).toEqual({
      diagnosis: false,
      inspection: false,
      batch: false,
    });
  });

  it('每个布尔值与路由门 canAccessPath 完全同源（防权限集合漂移）', () => {
    const cases = [ADMIN_PERMS, OPERATOR_PERMS, APPROVER_PERMS, AUDITOR_PERMS, []];
    for (const permissions of cases) {
      expect(replayStepLinks(permissions)).toEqual({
        diagnosis: canAccessPath('/alerts', permissions),
        inspection: canAccessPath('/inspections', permissions),
        batch: canAccessPath('/batch', permissions),
      });
    }
  });
});

describe('canOpenChatFromCatalog（空目录「去对话页」）', () => {
  it('审计员/审批者没有 chat:use：不渲染去对话页入口', () => {
    expect(canOpenChatFromCatalog(AUDITOR_PERMS)).toBe(false);
    expect(canOpenChatFromCatalog(APPROVER_PERMS)).toBe(false);
  });

  it('运维工程师/管理员有 chat:use：保留入口', () => {
    expect(canOpenChatFromCatalog(OPERATOR_PERMS)).toBe(true);
    expect(canOpenChatFromCatalog(ADMIN_PERMS)).toBe(true);
  });

  it('权限未加载完成（空集合）按无权限处理', () => {
    expect(canOpenChatFromCatalog([])).toBe(false);
  });
});
