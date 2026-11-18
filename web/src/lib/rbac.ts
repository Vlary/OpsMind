// 角色感知导航与路由权限判定（纯函数，node 环境可直测）。
//
// 权限矩阵见 CONTRACT.md 12.2；导航项在 components/navItems.ts 上声明
// 所需权限。这里的过滤只决定「入口是否出现 / 直达是否给 403 页」，
// 不构成授权：服务端 requirePermission（12.4 第 1 条）仍是唯一事实源。

import { NAV_ITEMS, type NavItem } from '../components/navItems';
import { hasAnyPermission, type Permission } from './rbacPermissions';

/** 按当前用户 permissions 过滤导航项（任一所需权限命中即保留）。 */
export function filterNavItems(
  items: readonly NavItem[],
  permissions: readonly Permission[],
): NavItem[] {
  return items.filter((item) => hasAnyPermission(permissions, item.permissions));
}

/**
 * 路径 → 导航项。`/` 与 `/c/:conversationId` 归入「对话」（与 App.tsx 的
 * index 重定向、AppShell.selectedKey 口径一致）。
 */
export function navItemForPath(pathname: string): NavItem | null {
  const target =
    pathname === '/' || pathname === '/c' || pathname.startsWith('/c/') ? '/chat' : pathname;
  return NAV_ITEMS.find((item) => target === item.key || target.startsWith(`${item.key}/`)) ?? null;
}

/** 该路径所需权限；未知路径（由 404 页兜底）返回 null 表示不做权限判定。 */
export function permissionForPath(pathname: string): readonly Permission[] | null {
  return navItemForPath(pathname)?.permissions ?? null;
}

/** 当前用户能否直达该路径（无权限时页面渲染 403，而不是空白）。 */
export function canAccessPath(pathname: string, permissions: readonly Permission[]): boolean {
  const required = permissionForPath(pathname);
  return required === null ? true : hasAnyPermission(permissions, required);
}

/** 该路径对应的页面名（与导航 label / 页面标题同源）；未知路径返回 null。 */
export function navLabelForPath(pathname: string): string | null {
  return navItemForPath(pathname)?.label ?? null;
}

/**
 * 登录后的默认落地页：第一个有权访问的导航项。避免审批者/审计员登录后
 * 被固定送到 /chat 再吃一个 403。全部不可见时回落 /chat（由 PermissionGate
 * 给出清晰的 403 页）。
 */
export function defaultLandingPath(permissions: readonly Permission[]): string {
  return filterNavItems(NAV_ITEMS, permissions)[0]?.key ?? '/chat';
}
