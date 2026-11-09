// 审批卡「编辑命令」的纯逻辑（六期 FE-P1，契约 2.11 约定 3 / 6.3 / 8.7）：
// decide 请求体构造、改写命令校验（1..2000、禁 NUL——服务端同款口径的
// 前置拦截）、改写后风险对照文案。导出仅作组件与单测共用（无 DOM 依赖）。

import type { components } from '../api/schema';

type ApprovalDecisionBody = components['schemas']['ApprovalDecision'];

/** 契约 2.11 约定 3 / openapi ApprovalDecision.edited_command：1..2000 字符。 */
export const EDITED_COMMAND_MAX_LENGTH = 2000;

/**
 * 改写命令校验：返回 null 表示合法，否则返回中文错误文案（编辑态内联展示）。
 * 服务端（approvals.Service.Decide）对超长与 NUL 均返回 400 VALIDATION_FAILED，
 * 这里做同款前置拦截，避免用户点「保存并批准」后才收到 400。
 *
 * 六期打磨 sprint 2（走查 high）：空串/纯空白草稿必须拦下——此前空草稿被
 * buildDecideBody 当作「未改写」提交纯 approve，服务端按**原命令**批准并
 * 真实执行（实测清空后点「保存并批准」执行了 `env`，泄漏 SSH_CONNECTION/
 * PATH 等环境变量）。空命令不是「放弃修改」；放弃修改的唯一出口是「取消
 * 编辑」，文案直接写清楚。
 */
export function validateEditedCommand(command: string): string | null {
  if (command.trim() === '') {
    return '命令不能为空，如需放弃修改请点「取消编辑」。';
  }
  if (command.includes('\0')) {
    return '命令包含非法字符（NUL），请删除后重试。';
  }
  if ([...command].length > EDITED_COMMAND_MAX_LENGTH) {
    return `命令超过 ${EDITED_COMMAND_MAX_LENGTH} 字符上限，请精简后重试。`;
  }
  return null;
}

/**
 * 是否真的发生了改写：与原命令逐字节不同才算（编辑态预填原命令，用户
 * 未改动时「保存并批准」等价于直接批准，不携带 edited_command）。
 * 首尾空白视为未改（trim 后相等不提交改写，避免无意义的服务端重分类）。
 */
export function hasEditedChange(original: string, draft: string): boolean {
  return draft.trim() !== original.trim();
}

/**
 * decide 请求体（契约 6.3 / 8.7）：仅 approve 且确有改写时携带 edited_command；
 * deny 永不携带（服务端 deny + edited_command 返回 400，拒绝流程不变）。
 *
 * draft 为空串/纯空白时**不携带** edited_command（服务端要求 edited_command
 * 1..2000 字符）——但这条分支只对「非编辑态直接批准」成立：编辑态下空草稿
 * 已被 validateEditedCommand/canSubmitEditedCommand 拦下、「保存并批准」保持
 * 禁用，因此不会再出现「清空后按原命令批准执行」（六期打磨 sprint 2 high）。
 * exactOptionalPropertyTypes：不带该键而非 undefined。
 */
export function buildDecideBody(
  decision: 'approve' | 'deny',
  original: string,
  draft: string | null | undefined,
): ApprovalDecisionBody {
  if (
    decision === 'approve' &&
    draft !== null &&
    draft !== undefined &&
    draft.trim() !== '' &&
    hasEditedChange(original, draft)
  ) {
    return { decision, edited_command: draft };
  }
  return { decision };
}

export type RiskChangeKind = 'forbidden' | 'up' | 'down' | 'same';

export interface RiskChange {
  kind: RiskChangeKind;
  /** 面向用户的中文对照文案。 */
  label: string;
}

/**
 * 改写前后风险对照（8.7：改写后实时预览，可能升级/降级）。
 * 原级别是审批落库级别（safe|needs_approval，forbidden 不会进审批表）；
 * 改写后经 preview-risk 重新分类，可能 forbidden（decide 会被 403 拒绝）。
 */
export function describeRiskChange(original: string, edited: string): RiskChange {
  if (edited === 'forbidden') {
    return { kind: 'forbidden', label: '改写后被禁止' };
  }
  if (original === edited) {
    return { kind: 'same', label: '风险不变' };
  }
  if (original === 'safe' && edited === 'needs_approval') {
    return { kind: 'up', label: '风险升级' };
  }
  if (original === 'needs_approval' && edited === 'safe') {
    return { kind: 'down', label: '风险降级' };
  }
  return { kind: 'same', label: '风险不变' };
}

/**
 * 「保存并批准」是否可提交：校验通过且预览非 forbidden。
 * preview 为 null（尚未返回/查询失败）不拦截——服务端 decide 仍会兜底
 * 重分类（预览只是提前提示，不承担裁决）。
 *
 * 空草稿恒不可提交（validateEditedCommand 的空白分支，六期打磨 sprint 2
 * high）：否则空串会走「未改写」分支、服务端按原命令批准执行。
 */
export function canSubmitEditedCommand(
  draft: string,
  previewLevel: string | null | undefined,
): boolean {
  if (validateEditedCommand(draft) !== null) {
    return false;
  }
  return previewLevel !== 'forbidden';
}
