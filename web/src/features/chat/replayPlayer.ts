// FE-P2（六期）：回放播放器的纯逻辑内核（契约 2.11 约定 4 / 8.7「回放播放器」）。
//
// 播放控制：上一步/下一步/自动播放（步进间隔 800ms，速度 1x/2x/4x 换算为
// 800/400/200ms）+ 进度条（seq 比例）。所有函数为纯函数，可在 node 环境
// 单测；定时器与高亮滚动在 ReplayViewer.tsx。

import type { components } from '../../api/schema';
import { stripContextPrefix } from './markdownBlocks';
import { isExemptExecution } from './toolCallBadges';

export type ReplayStep = components['schemas']['ReplayStep'];

/** 自动播放的基准步进间隔（契约 8.7：步进间隔 800ms）。 */
export const REPLAY_STEP_INTERVAL_MS = 800;

/** 速度档位（契约：1x/2x/4x）。 */
export const REPLAY_SPEEDS = [1, 2, 4] as const;

export type ReplaySpeed = (typeof REPLAY_SPEEDS)[number];

export function isReplaySpeed(value: number): value is ReplaySpeed {
  return (REPLAY_SPEEDS as readonly number[]).includes(value);
}

/** 速度 → 步进间隔：1x=800ms、2x=400ms、4x=200ms。 */
export function stepIntervalMs(speed: ReplaySpeed): number {
  return REPLAY_STEP_INTERVAL_MS / speed;
}

/** 播放器位置状态：index 为当前高亮步下标（0..total-1）。 */
export interface ReplayPosition {
  index: number;
  playing: boolean;
}

export interface StepsTotal {
  total: number;
}

/** 空列表安全钳制：total<=0 时恒 0。 */
export function clampStepIndex(index: number, total: number): number {
  if (total <= 0) {
    return 0;
  }
  return Math.min(Math.max(index, 0), total - 1);
}

/**
 * 自动播放一次步进（tick）：index+1；到达最后一步时停在末步并暂停
 * （终止条件：index 不会越过 total-1，播放自然收敛）。
 */
export function advanceTick(position: ReplayPosition, total: number): ReplayPosition {
  if (total <= 0 || position.index >= total - 1) {
    return { index: clampStepIndex(position.index, total), playing: false };
  }
  return { index: position.index + 1, playing: position.playing };
}

/** 单步：delta=1 下一步 / delta=-1 上一步；越界钳制，播放中单步即暂停。 */
export function stepBy(position: ReplayPosition, delta: number, total: number): ReplayPosition {
  return { index: clampStepIndex(position.index + delta, total), playing: false };
}

/**
 * 进度条比例（0..100）：index 在 total 步中的位置；空列表与单步恒 0。
 *
 * 单步会话（total=1）时 `index/(total-1)` 是 0/0 → NaN——antd Slider 收到
 * NaN 后不写 aria-valuenow、轨道宽度恒 0%（六期打磨 sprint 2 走查：
 * `replayProgress(0,1)` 返回 NaN）。单步没有「位置」可言，固定 0。
 */
export function replayProgress(index: number, total: number): number {
  if (total <= 1) {
    return 0;
  }
  return (clampStepIndex(index, total) / (total - 1)) * 100;
}

/** 进度条拖动/点击 → 步下标（四舍五入后钳制）。 */
export function progressToIndex(percent: number, total: number): number {
  if (total <= 0) {
    return 0;
  }
  const ratio = Math.min(Math.max(percent, 0), 100) / 100;
  return clampStepIndex(Math.round(ratio * (total - 1)), total);
}

/** kind 分色（契约 8.7：user_message 蓝 / assistant_text 默认 / reasoning 紫 /
 *  tool_call 青 / approval 黄 / execution 绿）——CSS 类名后缀。 */
export const REPLAY_KIND_CLASS: Record<ReplayStep['kind'], string> = {
  user_message: 'blue',
  assistant_text: 'default',
  reasoning: 'purple',
  tool_call: 'cyan',
  approval: 'gold',
  execution: 'green',
};

/** 步骤卡片标题文案。 */
export const REPLAY_KIND_LABELS: Record<ReplayStep['kind'], string> = {
  user_message: '用户',
  assistant_text: '回答',
  reasoning: '深度思考',
  tool_call: '工具调用',
  approval: '审批',
  execution: '执行',
};

/**
 * 审批状态 → 中文（回放中的 approval 步展示批准/拒绝/过期/待决）。
 *
 * `expired` 用中性文案「已过期」而不是「已超时」（六期打磨 sprint 3 走查）：
 * approvals 的 expired 有两种来源——TTL 到时（真超时）与回答流断开/服务
 * 关闭时的 Cancel（note=stream_cancelled / server_shutting_down，对话卡显示
 * 「已取消」）。回放侧拿不到 note（ReplayStep 无该字段），统一说成「超时」
 * 会与同会话对话卡的「已取消」直接矛盾；中性文案对两种来源都成立。
 */
export function approvalStatusLabel(status: string | null | undefined): string {
  switch (status) {
    case 'approved':
      return '已批准';
    case 'denied':
      return '已拒绝';
    case 'expired':
      return '已过期';
    case 'pending':
      return '待审批';
    default:
      return status ?? '—';
  }
}

/** execution 步状态（openapi：succeeded|failed）。 */
export function executionStatusLabel(status: string | null | undefined): string {
  if (status === 'succeeded') {
    return '成功';
  }
  if (status === 'failed') {
    return '失败';
  }
  return status ?? '—';
}

/**
 * 审批来源 → 中文（六期打磨 sprint 5 走查 low）。
 *
 * 回放审批步头部此前直出 `step.source`（chat / console / batch），是全站
 * 唯一的中英混排（相邻的审批状态、门限、批量状态一律中文）。未知来源原样
 * 返回，后端将来新增来源时不会渲染空白。
 */
export function approvalSourceLabel(source: string | null | undefined): string {
  switch (source) {
    case 'chat':
      return '对话';
    case 'console':
      return '控制台';
    case 'batch':
      return '批量';
    case null:
    case undefined:
    case '':
      return '—';
    default:
      return source;
  }
}

/** tool_call 步状态沿用 ToolCallStatus 的站点口径。 */
export function toolCallStatusLabel(status: string | null | undefined): string {
  switch (status) {
    case 'started':
      return '进行中';
    case 'awaiting_approval':
      return '待审批';
    case 'succeeded':
      return '成功';
    // 六期打磨 sprint 2（走查）：failed 此前被交给 approvalStatusLabel，
    // 而后者没有 failed 分支、走 default 原样返回英文「failed」（回放 UI
    // 唯一的中英混排）。failed 与 denied/expired 语义不同，单独给中文。
    case 'failed':
      return '失败';
    case 'denied':
    case 'expired':
      return approvalStatusLabel(status);
    case 'awaiting_user':
      return '待用户回答';
    default:
      return status ?? '—';
  }
}

/** approval 步是否被用户改写（original_command 非空 = 改写标注，契约 3.5）。 */
export function isEditedApproval(step: ReplayStep): boolean {
  const original = step.original_command;
  return typeof original === 'string' && original.length > 0;
}

/**
 * 回放 tool_call 步是否「免审批档直执行」（六期打磨 sprint 2 走查 medium）。
 *
 * 背景：`none`（免审批）或 `high`（仅高危审批）档下，needs_approval 级命令
 * 可以不经审批直接执行。回放此前仍渲染 RiskBadge「需审批」，与聊天工具卡
 * （`toolCallBadges.isExemptExecution` 已改写为「高风险 · 免审批执行」）
 * 说法相反。
 *
 * `hasApproval` 由调用方按步骤集合算好（同一 tool_call_id 是否存在 approval
 * 步）——回放里真走了审批的调用绝不能被判成免审批，因此与聊天侧
 * `approvalCount === 0` 同义。
 */
export function isReplayExemptExecution(step: ReplayStep, hasApproval: boolean): boolean {
  if (step.kind !== 'tool_call') {
    return false;
  }
  return isExemptExecution({
    name: step.name ?? '',
    riskLevel: step.risk_level ?? '',
    status: step.status ?? '',
    approvalMode: step.approval_mode ?? '',
    approvalCount: hasApproval ? 1 : 0,
  });
}

/**
 * 收集「已产生审批」的 tool_call_id 集合（回放里 approval 步与 tool_call 步
 * 以 tool_call_id 关联）。遍历有界（steps 已受 20000 步上限约束）。
 */
export function approvedToolCallIds(steps: readonly ReplayStep[]): Set<string> {
  const ids = new Set<string>();
  for (const step of steps) {
    if (step.kind === 'approval' && step.tool_call_id) {
      ids.add(step.tool_call_id);
    }
  }
  return ids;
}

/**
 * 步骤摘要行：控制条上方「第 N/M 步 · 标题」与已播放步的预览。
 *
 * user_message 走与正文相同的 stripContextPrefix（六期打磨 sprint 2：正文
 * 已剥离内部上下文前缀，但摘要行仍显示 `<!--opsmind-context servers: …`，
 * 等于把同一处泄漏换了个位置）。
 */
export function stepSummary(step: ReplayStep): string {
  switch (step.kind) {
    case 'user_message':
    case 'assistant_text':
    case 'reasoning': {
      const raw = step.content ?? '';
      const content = step.kind === 'user_message' ? stripContextPrefix(raw) : raw;
      return content.length > 0 ? content.replace(/\s+/g, ' ').slice(0, 60) : '（空）';
    }
    case 'tool_call':
      return step.name ?? '—';
    case 'approval':
      return step.command ?? '—';
    case 'execution':
      return step.command ?? '—';
    default:
      return '—';
  }
}
