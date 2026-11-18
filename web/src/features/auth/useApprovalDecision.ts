// 审批操作区展示状态 hook（RBAC 00012 / 第 7 轮走查）。
//
// 三处 console/batch 审批入口（批量详情、诊断修复、容器操作）共用：
// 从 `GET /api/auth/me` 取当前用户与权限，再交给纯函数
// `approvalDecisionState` 判定是否渲染批准/拒绝。身份/权限未就绪时返回
// 'self'（失败关闭），不会先渲染一个必然 403 的按钮。

import {
  approvalDecisionState,
  type ApprovalDecisionState,
  type ApprovalStatus,
} from '../../lib/approvalDecision';
import { useMe } from './useMe';

export function useApprovalDecision(input: {
  status: ApprovalStatus;
  requestedBy: string;
}): ApprovalDecisionState {
  const meQuery = useMe();
  return approvalDecisionState({
    status: input.status,
    permissions: meQuery.data?.permissions ?? [],
    requestedBy: input.requestedBy,
    currentUserId: meQuery.data?.user.id ?? '',
  });
}
