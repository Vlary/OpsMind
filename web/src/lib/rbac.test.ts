import { describe, expect, it } from 'vitest';

import { NAV_ITEMS } from '../components/navItems';
import {
  canAccessPath,
  defaultLandingPath,
  filterNavItems,
  navItemForPath,
  navLabelForPath,
  permissionForPath,
} from './rbac';
import {
  hasAnyPermission,
  PERMS,
  ROLE_LABELS,
  ROLE_OPTIONS,
  roleLabel,
  type Permission,
} from './rbacPermissions';

// 各角色权限集合按 CONTRACT.md 12.2 矩阵抄写（仅测试夹具，运行时权限
// 一律由 /api/auth/me 下发）。
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

const keys = (items: readonly { key: string }[]): string[] => items.map((item) => item.key);

describe('角色与权限常量', () => {
  it('四个固定角色的中文名与契约 12.1 一致', () => {
    expect(ROLE_LABELS.admin).toBe('管理员');
    expect(ROLE_LABELS['operator']).toBe('运维工程师');
    expect(ROLE_LABELS['approver']).toBe('审批者');
    expect(ROLE_LABELS.auditor).toBe('审计员');
    expect(ROLE_OPTIONS).toHaveLength(4);
  });

  it('roleLabel 对未知/空值不抛异常', () => {
    expect(roleLabel('admin')).toBe('管理员');
    expect(roleLabel(null)).toBe('未知角色');
    expect(roleLabel(undefined)).toBe('未知角色');
    // 越界值（后端新增角色而前端未同步）原样返回，不显示 undefined。
    expect(roleLabel('auditor2' as never)).toBe('auditor2');
  });

  it('每个导航项都声明了权限（防止新增页面漏配）', () => {
    for (const item of NAV_ITEMS) {
      expect(item.permissions, item.key).toBeDefined();
      expect(item.permissions?.length, item.key).toBeGreaterThan(0);
    }
  });
});

describe('hasAnyPermission：任一命中', () => {
  it('未声明所需权限时对所有已登录用户放行', () => {
    expect(hasAnyPermission([], undefined)).toBe(true);
    expect(hasAnyPermission([], [])).toBe(true);
  });

  it('命中任意一条即通过（多权限导航项）', () => {
    expect(hasAnyPermission([PERMS.auditRead], [PERMS.terminal, PERMS.auditRead])).toBe(true);
    expect(hasAnyPermission([PERMS.terminal], [PERMS.terminal, PERMS.auditRead])).toBe(true);
    expect(hasAnyPermission([PERMS.usersRead], [PERMS.terminal, PERMS.auditRead])).toBe(false);
  });

  it('空权限集合一律拒绝', () => {
    expect(hasAnyPermission([], [PERMS.chat])).toBe(false);
  });
});

describe('filterNavItems：导航按权限收敛', () => {
  it('admin 看到全部导航（含用户管理）', () => {
    expect(keys(filterNavItems(NAV_ITEMS, ADMIN_PERMS))).toEqual(keys(NAV_ITEMS));
    expect(keys(filterNavItems(NAV_ITEMS, ADMIN_PERMS))).toContain('/users');
  });

  it('运维工程师看不到用户/模型/审计/设置', () => {
    const visible = keys(filterNavItems(NAV_ITEMS, OPERATOR_PERMS));
    expect(visible).toContain('/chat');
    expect(visible).toContain('/servers');
    expect(visible).toContain('/databases');
    expect(visible).toContain('/containers');
    expect(visible).toContain('/batch');
    expect(visible).not.toContain('/users');
    expect(visible).not.toContain('/providers');
    expect(visible).not.toContain('/audit');
    expect(visible).not.toContain('/settings');
  });

  it('审批者有审批队列 + 资产只读入口（无对话、无管理面）', () => {
    // 00012 评审 high：审批者此前连审批入口都没有，无法履职。
    expect(keys(filterNavItems(NAV_ITEMS, APPROVER_PERMS))).toEqual([
      '/approvals',
      '/servers',
      '/databases',
    ]);
  });

  it('审批者默认落地审批队列而不是资产页', () => {
    expect(defaultLandingPath(APPROVER_PERMS)).toBe('/approvals');
  });

  it('审计员只有审计/跨用户录制/回放三项', () => {
    expect(keys(filterNavItems(NAV_ITEMS, AUDITOR_PERMS))).toEqual([
      '/audit',
      '/recordings',
      '/replays',
    ]);
  });

  it('空权限集合得到空导航', () => {
    expect(filterNavItems(NAV_ITEMS, [])).toEqual([]);
  });
});

describe('路由直达权限（403 页判定）', () => {
  it('/users 仅用户查看权限可直达', () => {
    expect(canAccessPath('/users', ADMIN_PERMS)).toBe(true);
    expect(canAccessPath('/users', OPERATOR_PERMS)).toBe(false);
    expect(canAccessPath('/users/550e8400-e29b-41d4-a716-446655440000', OPERATOR_PERMS)).toBe(
      false,
    );
  });

  it('/c/:id 归入对话权限', () => {
    expect(canAccessPath('/c/abc', OPERATOR_PERMS)).toBe(true);
    expect(canAccessPath('/c/abc', APPROVER_PERMS)).toBe(false);
    expect(navLabelForPath('/c/abc')).toBe('对话');
  });

  it('/replays/:id 沿用回放执行的多权限口径', () => {
    expect(canAccessPath('/replays/abc', OPERATOR_PERMS)).toBe(true);
    expect(canAccessPath('/replays/abc', AUDITOR_PERMS)).toBe(true);
    expect(canAccessPath('/replays/abc', APPROVER_PERMS)).toBe(false);
  });

  it('未知路径不做权限判定（交给 404 页）', () => {
    expect(permissionForPath('/definitely-not-a-route')).toBeNull();
    expect(canAccessPath('/definitely-not-a-route', [])).toBe(true);
    expect(navItemForPath('/definitely-not-a-route')).toBeNull();
  });
});

describe('登录默认落地页', () => {
  it('按角色落到第一个可访问页面', () => {
    expect(defaultLandingPath(ADMIN_PERMS)).toBe('/chat');
    expect(defaultLandingPath(OPERATOR_PERMS)).toBe('/chat');
    expect(defaultLandingPath(APPROVER_PERMS)).toBe('/approvals');
    expect(defaultLandingPath(AUDITOR_PERMS)).toBe('/audit');
  });

  it('全部不可见时回落 /chat（由 403 页解释）', () => {
    expect(defaultLandingPath([])).toBe('/chat');
  });
});
