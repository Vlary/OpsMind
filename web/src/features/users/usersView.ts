// 用户管理页的纯逻辑（护栏 + 状态文案）。
//
// 服务端在事务内做最后管理员/自我操作守卫（CONTRACT.md 12.3 第 14/15 条，
// 409 LAST_ADMIN / SELF_OPERATION）。前端提前禁用按钮并说明原因只是
// 「别让用户点了才报错」，不是授权：并发窗口下的最终判定仍在服务端。

import type { components } from '../../api/schema';
import { ROLE_DESCRIPTIONS, type Role } from '../../lib/rbacPermissions';

export type User = components['schemas']['User'];

export interface GuardResult {
  allowed: boolean;
  /** 不允许时的中文原因（按钮 Tooltip / 确认框）。 */
  reason?: string;
}

const ALLOW: GuardResult = { allowed: true };

export function enabledAdminCount(users: readonly User[]): number {
  return users.filter((user) => user.role === 'admin' && user.enabled).length;
}

/** 该用户是否是当前唯一启用中的管理员。 */
export function isLastEnabledAdmin(user: User, users: readonly User[]): boolean {
  return user.role === 'admin' && user.enabled && enabledAdminCount(users) <= 1;
}

export function canDisableUser(
  user: User,
  currentUserId: string,
  users: readonly User[],
): GuardResult {
  if (!user.enabled) {
    return { allowed: false, reason: '账号已停用' };
  }
  if (user.id === currentUserId) {
    return { allowed: false, reason: '不能停用自己' };
  }
  if (isLastEnabledAdmin(user, users)) {
    return { allowed: false, reason: '不能停用最后一个管理员' };
  }
  return ALLOW;
}

/**
 * 账号是否仍在邀请流程中（含邀请已过期）。
 *
 * 这类账号没有可用密码：启用会得到一个无法登录的账号，重置密码也不会把
 * enabled 置 true，还会让邮箱里的邀请链接失效。唯一可用的操作是重发邀请。
 */
export function isInvitePending(user: User): boolean {
  return !user.enabled && user.invite_pending;
}

/** 启用停用账号总是允许的；待接受邀请（含过期）只能重发邀请。 */
export function canEnableUser(user: User): GuardResult {
  if (user.enabled) {
    return { allowed: false, reason: '账号已启用' };
  }
  if (isInvitePending(user)) {
    return { allowed: false, reason: '账号待接受邀请（或邀请已过期），请重发邀请' };
  }
  return ALLOW;
}

/** 重发邀请只对「停用 + 仍持有未使用 invite」的账号开放。 */
export function canResendInvite(user: User): GuardResult {
  if (!isInvitePending(user)) {
    return { allowed: false, reason: '账号不在邀请流程中' };
  }
  return ALLOW;
}

export function canChangeRole(
  user: User,
  currentUserId: string,
  users: readonly User[],
): GuardResult {
  if (user.id === currentUserId) {
    return { allowed: false, reason: '不能修改自己的角色' };
  }
  if (isLastEnabledAdmin(user, users)) {
    return { allowed: false, reason: '不能降级最后一个管理员' };
  }
  return ALLOW;
}

/** 角色弹窗的提交守卫：目标角色与原角色相同不提交。 */
export function canSubmitRoleChange(
  user: User,
  nextRole: User['role'],
  currentUserId: string,
  users: readonly User[],
): GuardResult {
  if (nextRole === user.role) {
    return { allowed: false, reason: '角色未变化' };
  }
  return canChangeRole(user, currentUserId, users);
}

export function canResetPassword(user: User, currentUserId: string): GuardResult {
  if (user.id === currentUserId) {
    return { allowed: false, reason: '不能重置自己的密码' };
  }
  if (isInvitePending(user)) {
    // 服务端同样拒绝：reset 只改密码不置 enabled，接受后仍无法登录，且会
    // 让未使用的邀请链接失效（第 5 轮走查 medium）。
    return { allowed: false, reason: '账号待接受邀请（或邀请已过期），请先重发邀请' };
  }
  return ALLOW;
}

export function canRevokeSessions(user: User, currentUserId: string): GuardResult {
  if (user.id === currentUserId) {
    return { allowed: false, reason: '不能吊销自己的会话（请退出登录）' };
  }
  return ALLOW;
}

export interface UserStatusMeta {
  label: string;
  color: string;
}

export function userStatus(user: User): UserStatusMeta {
  if (user.enabled) {
    return { label: '已启用', color: 'green' };
  }
  if (user.invite_pending) {
    return { label: '待接受邀请', color: 'gold' };
  }
  return { label: '已停用', color: 'default' };
}

/** 页面顶部护栏提示里展示的管理员数量文案。 */
export function adminGuardHint(users: readonly User[], currentUserId: string): string {
  const admins = enabledAdminCount(users);
  if (admins <= 1) {
    return `当前只有 ${admins} 个启用中的管理员：不能停用或降级最后一个管理员，也不能停用/改自己的角色。`;
  }
  const self = users.find((user) => user.id === currentUserId);
  const selfNote = self ? '不能停用自己或修改自己的角色。' : '';
  return `当前有 ${admins} 个启用中的管理员；${selfNote}停用、改角色与重置密码会立即吊销目标账号的会话。`;
}

/**
 * roleDescription 返回角色职责说明（与 ROLE_DESCRIPTIONS 同源）。
 *
 * 改角色弹窗与创建用户弹窗必须显示同一段说明：只列四个角色名会让管理员
 * 不知道「审批者不能发起对话」「审计员只读」（评审 medium）。
 */
export function roleDescription(role: string | null | undefined): string {
  if (!role) {
    return '';
  }
  return ROLE_DESCRIPTIONS[role as Role] ?? '';
}
