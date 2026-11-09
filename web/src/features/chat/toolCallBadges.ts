// 六期打磨 sprint 1：工具卡风险/门限徽标的互斥判定（契约 8.7 / 2.11 约定 2）。
//
// 背景：`none`（免审批）或 `high`（仅高危审批）档下，命令的分类级别
// （`risk_level=needs_approval`）与「实际是否走了审批」是两件事。此前卡片
// 同时渲染 RiskBadge「需审批」+ ApprovalModeTag「免审批」+ Tag「高风险」，
// 同一张卡出现两种相反提示（实机流式首帧 Tag 序列 =
// ["成功","需审批","免审批","高风险"]），用户无法判断命令到底是否需要审批。
//
// 判定纯函数在这里（react-refresh 要求组件文件只导出组件），ToolCallCard
// 与单测共用。

import type { components } from '../../api/schema';

type Approval = components['schemas']['Approval'];

/** 免审批执行提示文案（高风险 Tag 的替代文案）。 */
export const EXEMPT_EXECUTION_LABEL = '高风险 · 免审批执行';

export interface BadgeInput {
  /** 工具名（仅命令执行类工具参与判定）。 */
  name: string;
  /** 命令分类级别（safe|needs_approval|forbidden）。 */
  riskLevel: string;
  /** 工具卡当前状态（started/awaiting_approval/succeeded/...）。 */
  status: string;
  /** 判定时的有效门限快照（'' = 未知/旧行）。 */
  approvalMode: string;
  /** 本工具调用关联的审批条数（>0 表示确实走了审批）。 */
  approvalCount: number;
}

/**
 * 是否为「免审批档下实际直执行」的命令执行类工具。
 *
 * 条件（全部满足）：
 *  1. 命令执行类工具（ssh_exec / container_action / batch_execute）；
 *  2. 门限为 none 或 high（这两档允许 needs_approval 级命令免审执行）；
 *  3. 分类级别为 needs_approval（safe 本来就不需要审批，无冲突）；
 *  4. 未创建审批（approvalCount === 0）且状态不是 awaiting_approval
 *     —— 确实没有走人工审批，才把「需审批」徽标改写为「免审批执行」。
 *
 * forbidden 除外：那类命令被规则禁止、从未执行，仍需红色「禁止」提示。
 */
export function isExemptExecution(input: BadgeInput): boolean {
  if (!isCommandExecTool(input.name)) {
    return false;
  }
  if (input.approvalMode !== 'none' && input.approvalMode !== 'high') {
    return false;
  }
  if (input.riskLevel !== 'needs_approval') {
    return false;
  }
  if (input.status === 'awaiting_approval') {
    return false;
  }
  return input.approvalCount === 0;
}

/** 与 backend isCommandExecTool 同名同集合（契约 6.2）。 */
export function isCommandExecTool(name: string): boolean {
  return name === 'ssh_exec' || name === 'container_action' || name === 'batch_execute';
}

/** 判定的输入投影（从 ToolCallView + approvals 装配）。 */
export function badgeInputOf(
  view: {
    name: string;
    riskLevel: string;
    status: string;
    approvalMode: string;
  },
  approvals: Approval[],
): BadgeInput {
  return {
    name: view.name,
    riskLevel: view.riskLevel,
    status: view.status,
    approvalMode: view.approvalMode,
    approvalCount: approvals.length,
  };
}
