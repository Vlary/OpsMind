// 回放页深链的权限收敛（RBAC 第 12 轮走查 low）。
//
// 审计员只有 audit:read：能进 /replays（导航 permissions = [chat, audit]），
// 但 execution 步无条件渲染的 /alerts、/inspections、/batch 深链与空目录
// 的 /chat 按钮都会在 PermissionGate 命中 403「无权访问」，把只读治理用户
// 带离工作流。这里直接复用路由门的唯一事实源 `canAccessPath`（NAV_ITEMS
// 声明的目标页权限），而不是手工复制一份权限集合——两处若漂移，测试会
// 立即发现（replayLinks.test.ts 逐一对照 canAccessPath）。
//
// 只决定入口是否出现；服务端权限矩阵不变（12.4：客户端不构成授权）。

import { canAccessPath } from '../../lib/rbac';
import type { Permission } from '../../lib/rbacPermissions';

export interface ReplayStepLinks {
  /** execution 步的「查看诊断会话」（目标 /alerts）。 */
  diagnosis: boolean;
  /** execution 步的「查看巡检报告」（目标 /inspections）。 */
  inspection: boolean;
  /** execution 步的「查看批量任务」（目标 /batch）。 */
  batch: boolean;
}

/** 按目标页的路由门权限，返回三个 execution 深链是否可渲染。 */
export function replayStepLinks(permissions: readonly Permission[]): ReplayStepLinks {
  return {
    diagnosis: canAccessPath('/alerts', permissions),
    inspection: canAccessPath('/inspections', permissions),
    batch: canAccessPath('/batch', permissions),
  };
}

/** 空目录的「去对话页」只对能进 /chat 的角色显示。 */
export function canOpenChatFromCatalog(permissions: readonly Permission[]): boolean {
  return canAccessPath('/chat', permissions);
}
