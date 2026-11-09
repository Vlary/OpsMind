// 审批状态中文标签（六期打磨 sprint 4 走查 low）。
//
// approvals 的 expired 有两种来源：TTL 到时，以及回答流断开/服务关闭时的
// Cancel（note=stream_cancelled / server_shutting_down，对话卡显示「已取消」）。
// 因此统一用中性文案「已过期」——StatusTag（components/StatusTag.tsx）与
// 回放侧（features/chat/replayPlayer.ts 的 approvalStatusLabel）早已如此，
// 此前只有对话审批卡正文仍写「已超时」，同一屏出现两种措辞
// （标签「已过期」+ 正文「审批已超时」）。
//
// 抽成独立模块（非组件文件）以便单测直接断言表内容——ApprovalCard.tsx 只
// 导出组件，常量从这里导出不会触发 react-refresh/only-export-components。

import type { components } from '../api/schema';

type ApprovalStatus = components['schemas']['ApprovalStatus'];

export const APPROVAL_STATUS_LABELS: Record<ApprovalStatus, string> = {
  pending: '等待处理',
  approved: '已批准',
  denied: '已拒绝',
  expired: '已过期',
};
