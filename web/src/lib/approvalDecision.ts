// 审批操作区展示状态的纯逻辑（RBAC 00012）。
//
// 服务端是唯一授权点：decide 端点要求 `approval:decide`，且
// `requested_by == decided_by` 返回 403 APPROVAL_SELF_DECISION。客户端此前
// 只看 `status === 'pending'` 就渲染批准/拒绝，导致运维执行者（无审批权限）
// 与自审发起人点下去只能拿到 403。这里按「有权限 + 不是发起人」收敛入口，
// 不足条件时由调用方给出等待/自审说明，避免点击后才报错。
//
// 从 components/ApprovalCard.tsx 抽出到 lib，供对话审批卡之外的批量详情、
// 诊断修复、容器操作三处复用同一判定（第 7 轮走查 medium）；本模块只依赖
// 类型与权限集合判断，不引入 React/antd，便于 node 直测。

import type { components } from '../api/schema';
import { hasAnyPermission, PERMS, type Permission } from './rbacPermissions';

export type ApprovalStatus = components['schemas']['ApprovalStatus'];

export type ApprovalDecisionState = 'actions' | 'waiting' | 'self' | 'resolved';

export function approvalDecisionState(input: {
  status: ApprovalStatus;
  permissions: readonly Permission[];
  requestedBy: string;
  currentUserId: string;
}): ApprovalDecisionState {
  if (input.status !== 'pending') {
    return 'resolved';
  }
  if (!hasAnyPermission(input.permissions, [PERMS.approvalsDecide])) {
    return 'waiting';
  }
  // 身份未就绪或与发起人相同都失败关闭：宁可等下一次 me 刷新，也不渲染
  // 一个必然被服务端拒绝的按钮。
  if (input.currentUserId === '' || input.currentUserId === input.requestedBy) {
    return 'self';
  }
  return 'actions';
}
