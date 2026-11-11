// FE-R（八期）：独立回放页的纯逻辑内核（契约 2.12 / 8.7「回放页」）。
//
// 六期 FE-P2 的播放推进/进度/标签函数**原样迁移**（25 例单测保留），
// 八期新增：筛选（kind/工具/风险）、搜索（大小写不敏感子串 + 命中跳转）、
// 快捷键映射、筛选与命中文案。所有函数均为纯函数，浏览器与 node 单测共用；
// 定时器、虚拟滚动与高亮 DOM 在 ReplayPlayer.tsx。

import type { components } from '../../api/schema';
import { stripContextPrefix } from '../chat/markdownBlocks';
import { isCommandExecTool, isExemptExecution } from '../chat/toolCallBadges';

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

/** 全部 kind（筛选面板顺序，与契约列出的六类一致）。 */
export const REPLAY_KINDS: readonly ReplayStep['kind'][] = [
  'user_message',
  'assistant_text',
  'reasoning',
  'tool_call',
  'approval',
  'execution',
];

/**
 * 动作类工具（契约 7.8，11 个）——生成 execution 步；其余 7 个为纯本地
 * 查询/交互类。前端只用于筛选面板的分组展示，不参与任何执行判定。
 */
export const REPLAY_ACTION_TOOLS: readonly string[] = [
  'ssh_exec',
  'server_facts',
  'container_action',
  'container_list',
  'container_inspect',
  'container_logs',
  'image_list',
  'db_query',
  'batch_execute',
  'run_diagnosis',
  'run_inspection',
];

/** 纯本地查询/交互工具（契约 7.8，7 个）。 */
export const REPLAY_LOCAL_TOOLS: readonly string[] = [
  'list_servers',
  'list_databases',
  'get_metrics',
  'list_alerts',
  'audit_search',
  'get_inspection_report',
  'ask_user',
];

/** 风险级别筛选项（契约：safe / needs_approval / forbidden）。 */
export const REPLAY_RISK_LEVELS = ['safe', 'needs_approval', 'forbidden'] as const;
export type ReplayRiskFilter = (typeof REPLAY_RISK_LEVELS)[number];

/** 风险级别中文（筛选项标签）。 */
export const REPLAY_RISK_LABELS: Record<ReplayRiskFilter, string> = {
  safe: '安全',
  needs_approval: '需审批',
  forbidden: '禁止',
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

/**
 * execution 步的 `command` 字段按工具语义标注（契约 2.12 约定 3）：
 * 命令类工具称「命令」，其余动作类工具称「动作」（字段承载动作串，
 * 如 `docker ps -a` / `inspection:full` / `diagnosis:<id>`）。
 */
export function executionCommandLabel(name: string | null | undefined): string {
  return isCommandExecTool(name ?? '') ? '命令' : '动作';
}

/**
 * 截断横幅文案（契约 8.7 逐字写死，四值各不相同；`''` 不显示）。
 *
 * `steps` 与 `approvals` 曾共用「超出 20000 步上限」——审批满 200 条时
 * 用户看到 200 步却被告知 20000 上限，自相矛盾（六期打磨 sprint 4）。
 * 八期新增 `payload`（体积预算命中）：参数与输出被清空但步数可能完整。
 */
export function truncatedBanner(
  reason: string | null | undefined,
  shownSteps: number,
): string | null {
  switch (reason) {
    case 'steps':
      return `超出 20000 步上限，仅显示最近 ${shownSteps} 步`;
    case 'approvals':
      return `审批记录过多，仅显示最近 ${shownSteps} 步（更早的审批未纳入回放）`;
    case 'payload':
      return `会话内容过大，部分参数与输出未下发（仅保留最近 ${shownSteps} 步的摘要）`;
    default:
      return null;
  }
}

// ---------------------------------------------------------------------------
// 八期：筛选（kind / 工具 / 风险级别）——已加载 steps 上的客户端过滤。
// ---------------------------------------------------------------------------

/** 客户端筛选条件（空数组 = 该组不过滤；组内 OR，组间 AND）。 */
export interface ReplayFilter {
  kinds: readonly ReplayStep['kind'][];
  tools: readonly string[];
  risks: readonly ReplayRiskFilter[];
}

export const EMPTY_REPLAY_FILTER: ReplayFilter = { kinds: [], tools: [], risks: [] };

/** 筛选条件是否为空（全部组都未选）。 */
export function isFilterEmpty(filter: ReplayFilter): boolean {
  return filter.kinds.length === 0 && filter.tools.length === 0 && filter.risks.length === 0;
}

/** 步骤是否命中工具筛选（只有 tool_call / execution 步带工具名）。 */
function stepMatchesTool(step: ReplayStep, tools: readonly string[]): boolean {
  if (step.kind !== 'tool_call' && step.kind !== 'execution') {
    return false;
  }
  return tools.includes(step.name ?? '');
}

/** 步骤是否命中风险筛选（只有 tool_call / approval 步带风险级别）。 */
function stepMatchesRisk(step: ReplayStep, risks: readonly string[]): boolean {
  if (step.kind !== 'tool_call' && step.kind !== 'approval') {
    return false;
  }
  return risks.includes(step.risk_level ?? '');
}

/**
 * 按 kind / 工具 / 风险三组条件**交集**过滤；空条件返回原数组
 * （同一引用，不复制——20000 步时避免无谓的整表拷贝与重渲染）。
 */
export function filterSteps(
  steps: readonly ReplayStep[],
  filter: ReplayFilter,
): readonly ReplayStep[] {
  if (isFilterEmpty(filter)) {
    return steps;
  }
  return steps.filter((step) => {
    if (filter.kinds.length > 0 && !filter.kinds.includes(step.kind)) {
      return false;
    }
    if (filter.tools.length > 0 && !stepMatchesTool(step, filter.tools)) {
      return false;
    }
    if (filter.risks.length > 0 && !stepMatchesRisk(step, filter.risks)) {
      return false;
    }
    return true;
  });
}

// ---------------------------------------------------------------------------
// 八期：搜索（大小写不敏感子串）与命中跳转。
// ---------------------------------------------------------------------------

/**
 * 单字段参与搜索的最大长度（防 OOM 硬上限，写死）。
 *
 * 回放响应本身受契约 2.12 约定 6 的两道体积预算约束（arguments 6 MiB、
 * 序列化 8 MiB），因此**全表文本总量有上界**；这里的每字段截断是给
 * 「后端未按预算返回」的防御：单步瞬时临时字符串 ≤ 5 × 256 KiB = 1.25 MiB，
 * 逐字段检查完即释放，不跨步累积。命令、正文、摘要远小于此，长日志的
 * 命中通常也落在前 256 KiB。
 */
export const SEARCH_FIELD_LIMIT = 256 * 1024;

/**
 * 查询串长度上限（写死）：超长查询没有实际意义，还让逐步扫描无谓变慢。
 */
export const SEARCH_QUERY_LIMIT = 200;

function bounded(text: string): string {
  return text.length > SEARCH_FIELD_LIMIT ? text.slice(0, SEARCH_FIELD_LIMIT) : text;
}

/** 把 arguments 对象序列化为可搜索文本（失败降级空串，绝不抛）。 */
function argumentsText(step: ReplayStep): string {
  const args = step.arguments;
  if (args === null || args === undefined) {
    return '';
  }
  try {
    return JSON.stringify(args) ?? '';
  } catch {
    return '';
  }
}

/** 参与搜索的字段（契约 8.7 的五个字段，顺序即高亮优先序）。 */
export function stepSearchFields(step: ReplayStep): ReadonlyArray<{ field: string; text: string }> {
  return [
    { field: 'content', text: bounded(step.content ?? '') },
    { field: 'command', text: bounded(step.command ?? '') },
    { field: 'output', text: bounded(step.output ?? '') },
    { field: 'result_summary', text: bounded(step.result_summary ?? '') },
    { field: 'arguments', text: bounded(argumentsText(step)) },
  ];
}

/** 单步的搜索文本（五字段拼接；arguments 以 JSON 形态参与匹配）。 */
export function stepSearchText(step: ReplayStep): string {
  return stepSearchFields(step)
    .map((item) => item.text)
    .join('\n');
}

/** 命中片段（用于步骤卡顶部的「命中」提示与关键词高亮）。 */
export interface SearchExcerpt {
  field: string;
  /** 命中前的文本（已折叠空白，最多 40 字符尾巴）。 */
  before: string;
  /** 命中原文。 */
  hit: string;
  /** 命中后的文本（已折叠空白，最多 80 字符开头）。 */
  after: string;
}

/**
 * 命中片段：取第一个含查询的字段，返回上下文（供高亮渲染）。
 *
 * 单步最多渲染一段，避免长输出里每处命中都生成 DOM；上下文各截断到
 * 固定长度（写死），20000 步滚动时不会因高亮构造大字符串。
 */
export function firstMatchExcerpt(step: ReplayStep, query: string): SearchExcerpt | null {
  const needle = normalizeQuery(query);
  if (needle === '') {
    return null;
  }
  for (const { field, text } of stepSearchFields(step)) {
    const index = text.toLowerCase().indexOf(needle);
    if (index < 0) {
      continue;
    }
    const start = Math.max(0, index - 40);
    const end = Math.min(text.length, index + needle.length + 80);
    return {
      field,
      before: `${start > 0 ? '…' : ''}${text.slice(start, index).replace(/\s+/g, ' ')}`,
      hit: text.slice(index, index + needle.length),
      after: `${text.slice(index + needle.length, end).replace(/\s+/g, ' ')}${
        end < text.length ? '…' : ''
      }`,
    };
  }
  return null;
}

/** 规范化查询（trim → 截断到 SEARCH_QUERY_LIMIT → 小写）；空查询返回 ''。 */
function normalizeQuery(query: string): string {
  const trimmed = query.trim();
  if (trimmed === '') {
    return '';
  }
  return (
    trimmed.length > SEARCH_QUERY_LIMIT ? trimmed.slice(0, SEARCH_QUERY_LIMIT) : trimmed
  ).toLowerCase();
}

/**
 * 单步是否命中查询（空查询恒 false——不把全部步骤当成命中）。
 */
export function stepMatchesQuery(step: ReplayStep, query: string): boolean {
  const needle = normalizeQuery(query);
  if (needle === '') {
    return false;
  }
  return stepSearchText(step).toLowerCase().includes(needle);
}

/** 返回全部命中步的下标（升序；空查询返回空数组）。遍历有界（steps 上限 20000）。 */
export function searchSteps(steps: readonly ReplayStep[], query: string): number[] {
  const needle = normalizeQuery(query);
  if (needle === '') {
    return [];
  }
  const hits: number[] = [];
  for (let index = 0; index < steps.length; index += 1) {
    const step = steps[index];
    if (step && stepSearchText(step).toLowerCase().includes(needle)) {
      hits.push(index);
    }
  }
  return hits;
}

/** 下一个命中下标（首尾环绕）；无命中恒 0。 */
export function nextMatch(current: number, count: number): number {
  if (count <= 0) {
    return 0;
  }
  return (Math.max(current, -1) + 1) % count;
}

/** 上一个命中下标（首尾环绕）；无命中恒 0。 */
export function prevMatch(current: number, count: number): number {
  if (count <= 0) {
    return 0;
  }
  return current <= 0 ? count - 1 : Math.min(current - 1, count - 1);
}

/** 「第 k / K 个匹配」；无命中返回空串（不渲染）。 */
export function matchLabel(current: number, count: number): string {
  if (count <= 0) {
    return '';
  }
  const position = Math.min(Math.max(current, 0), count - 1);
  return `第 ${position + 1} / ${count} 个匹配`;
}

/**
 * 筛选 / 命中文案（契约 8.7 逐字写死「已筛选：显示 M / N 步」；
 * F08 同时要求「第 k / K 个匹配」由本函数合并输出）。
 */
export function mergeFilterLabel(
  visible: number,
  total: number,
  matches?: { current: number; count: number },
): string {
  const base = `已筛选：显示 ${visible} / ${total} 步`;
  if (!matches || matches.count <= 0) {
    return base;
  }
  return `${base} · ${matchLabel(matches.current, matches.count)}`;
}

// ---------------------------------------------------------------------------
// 八期：快捷键映射（焦点不在输入框/文本域/contentEditable 时生效）。
// ---------------------------------------------------------------------------

export type ReplayHotkeyAction =
  | 'toggle-play'
  | 'next-step'
  | 'prev-step'
  | 'next-match'
  | 'prev-match'
  | 'first-step'
  | 'last-step'
  | 'focus-search'
  | 'focus-filter'
  | 'clear';

export interface ReplayHotkeyContext {
  /** 焦点在 input / textarea / contentEditable / select 内：全部按键让位给控件。 */
  inInput: boolean;
  /** Shift 按下（Shift+←/→ = 上/下一个匹配）。 */
  shift?: boolean;
  /** 已有搜索词。 */
  hasSearch?: boolean;
  /** 已有筛选项。 */
  hasFilter?: boolean;
}

/**
 * 快捷键 → 动作（契约 8.7 ⑥）。返回 null 表示不拦截（浏览器默认行为保留）。
 *
 * `Esc` 仅在「有搜索或有筛选」时返回 `clear`——否则返回 null，避免页面
 * 在纯净态下吞掉 Esc（用户仍可 Esc 关闭其他浮层）。
 */
export function hotkeyAction(key: string, context: ReplayHotkeyContext): ReplayHotkeyAction | null {
  if (context.inInput) {
    return null;
  }
  switch (key) {
    case ' ':
    case 'Spacebar':
    case 'Space':
      return 'toggle-play';
    case 'ArrowLeft':
      return context.shift ? 'prev-match' : 'prev-step';
    case 'ArrowRight':
      return context.shift ? 'next-match' : 'next-step';
    case 'Home':
      return 'first-step';
    case 'End':
      return 'last-step';
    case '/':
      return 'focus-search';
    case 'f':
    case 'F':
      return 'focus-filter';
    case 'Escape':
    case 'Esc':
      return context.hasSearch || context.hasFilter ? 'clear' : null;
    default:
      return null;
  }
}

/**
 * 焦点是否位于可编辑/复合交互控件内（快捷键让位判定，契约 8.7 ⑥）。
 *
 * 覆盖三类：
 *  1. 原生 input / textarea / select（antd Select 的搜索框与 Input 都是原生 input）；
 *  2. contentEditable（富文本编辑）；
 *  3. 页面内的复合交互控件——antd Slider 手柄（role=slider）、Select 触发器
 *     （role=combobox）、下拉项（role=option）。这些控件自身用方向键/Home/End
 *     操作，回放快捷键不得抢走（否则速度下拉与进度条的键盘操作完全失效）。
 *
 * 按钮/链接**不**在此列：它们不用方向键，焦点停在「下一步」后方向键仍应
 * 推进播放；空格落在按钮上时由调用方让位给原生激活（否则一次空格既点按钮
 * 又切播放）。
 */
export function isEditableTarget(target: EventTarget | null): boolean {
  if (!target || typeof target !== 'object') {
    return false;
  }
  const element = target as {
    tagName?: unknown;
    isContentEditable?: unknown;
    closest?: (selector: string) => unknown;
  };
  const tagName = typeof element.tagName === 'string' ? element.tagName.toUpperCase() : '';
  if (tagName === 'INPUT' || tagName === 'TEXTAREA' || tagName === 'SELECT') {
    return true;
  }
  if (element.isContentEditable === true) {
    return true;
  }
  if (typeof element.closest === 'function') {
    return element.closest(INTERACTIVE_SELECTOR) !== null;
  }
  return false;
}

/** 复合交互控件的选择器（见 isEditableTarget 第 3 类）。 */
const INTERACTIVE_SELECTOR =
  '[role="combobox"],[role="slider"],[role="listbox"],[role="option"],' +
  '.ant-select,.ant-slider,.ant-collapse-header';

/**
 * 焦点是否落在按钮/链接上（空格必须让位给原生激活）。
 *
 * 浏览器对 `button` / `a[href]` 的空格键自带「按下」语义；若回放同时把
 * 空格映射成播放/暂停，一次按键会既点按钮又切播放（走查实测：点完「下一步」
 * 后按空格，播放状态与焦点按钮状态互相打架）。方向键不受影响——按钮不用
 * 方向键，仍应推进播放。
 */
export function isActivationTarget(target: EventTarget | null): boolean {
  if (!target || typeof target !== 'object') {
    return false;
  }
  const element = target as { closest?: (selector: string) => unknown };
  if (typeof element.closest !== 'function') {
    return false;
  }
  return element.closest('button,a[href],[role="button"]') !== null;
}

/**
 * 步骤跳转输入解析：「第 N 步」输入框回车定位，越界钳制。
 *
 * 非数字/空串返回 null（调用方保留原位置，不跳到 0——空草稿回车不该
 * 把播放器拉回开头）。数字按 1 基计数，钳制到 [1, total]。
 */
export function parseStepJump(raw: string, total: number): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) {
    return null;
  }
  const value = Number.parseInt(trimmed, 10);
  if (!Number.isFinite(value)) {
    return null;
  }
  return clampStepIndex(value - 1, total);
}
