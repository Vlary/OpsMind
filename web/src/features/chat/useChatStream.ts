import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useRef, useState } from 'react';

import { qk } from '../../api/queryKeys';
import { readSseStream, type SseFrame } from '../../api/sse';
import type { components } from '../../api/schema';
import { StreamBuffer } from './streamBuffer';

type Approval = components['schemas']['Approval'];
type ApprovalStatus = components['schemas']['ApprovalStatus'];
type SseApprovalResolved = components['schemas']['SseApprovalResolved'];
type SseDone = components['schemas']['SseDone'];
type SseError = components['schemas']['SseError'];
type SseToolCall = components['schemas']['SseToolCall'];
type SseToolResult = components['schemas']['SseToolResult'];
type ToolCallStatus = components['schemas']['ToolCallStatus'];
type ToolCallSchema = components['schemas']['ToolCall'];

export type StreamPhase = 'idle' | 'connecting' | 'streaming';

type TimeoutHandle = ReturnType<typeof setTimeout>;

/**
 * 流式空闲看门狗评估周期：距上一帧（含发起请求）超过该时长即评估一次
 * 「连接是否已挂」。
 *
 * 反向代理挂住连接 / 网络半开等拓扑下，浏览器 fetch reader 会永远挂起：
 * 流既不结束也不抛错（走查 A2：SIGKILL 后端经 vite preview 代理，前端
 * 永久停留在「正在生成…」，直连时浏览器反而能立刻感知断开）。
 *
 * 注意这不是「30 秒无帧即断」：审批等待、工具执行、LLM 首 token 都是
 * 合法的长无帧窗口，fire 时由 shouldHoldIdleWatchdog 判断是否续期。
 */
export const STREAM_IDLE_TIMEOUT_MS = 30_000;

/** 审批过期后的续期缓冲：等后端补发 approval_resolved(expired) 帧。 */
const APPROVAL_IDLE_GRACE_MS = 5_000;
/** 工具执行的合法无帧上限：SSH 最长 300s（agent/tools.go MaxSSH）+ 余量。 */
const TOOL_IDLE_GRACE_MS = 330_000;
/** 首帧（首个 token/reasoning/工具事件）前的合法等待：后端 LLM 调用超时
 *  180s（agent/runner.go 默认）+ 余量；超时后端必发 error 帧，无需前端抢先。 */
const FIRST_FRAME_GRACE_MS = 210_000;

/**
 * 看门狗 fire 时判断是否仍处于「合法无帧等待」窗口：
 * - 待处理审批未过期（expires_at 前后缓冲内）：后端挂起等用户决策；
 * - 工具执行中（收到 tool_call 但未收到 tool_result）：SSH 等最长 300s；
 * - 尚未收到任何帧且后端 LLM 超时（180s）未到：首 token 可合法地慢。
 * 窗口内返回 true 由调用方续期重评，窗口外仍无帧才判定连接挂死。
 */
export function shouldHoldIdleWatchdog(
  live: LiveTurn | null,
  now: number,
  receivedAnyFrame: boolean,
): boolean {
  if (!live) {
    return false;
  }
  const pendingApproval = live.approvals.some((approval) => {
    if (approval.status !== 'pending') {
      return false;
    }
    const expiresAt = Date.parse(approval.expires_at);
    return Number.isFinite(expiresAt) && expiresAt > now - APPROVAL_IDLE_GRACE_MS;
  });
  if (pendingApproval) {
    return true;
  }
  const toolRunning = live.tools.some(
    (tool) =>
      (tool.status === 'started' || tool.status === 'awaiting_approval') &&
      now - tool.startedAt < TOOL_IDLE_GRACE_MS,
  );
  if (toolRunning) {
    return true;
  }
  return !receivedAnyFrame && now - live.startedAt < FIRST_FRAME_GRACE_MS;
}

/** SSE 空闲看门狗：每次 arm 重置计时，超过 timeoutMs 未再 arm 则触发一次 onFire。 */
export class IdleWatchdog {
  private handle: TimeoutHandle | null = null;

  constructor(
    private readonly timeoutMs: number,
    private readonly onFire: () => void,
  ) {}

  /** 发起请求 / 收到任意帧时调用，重新开始计时。 */
  arm(): void {
    this.cancel();
    this.handle = setTimeout(() => {
      this.handle = null;
      this.onFire();
    }, this.timeoutMs);
  }

  /** 流结束 / 中止时停止计时（幂等）。 */
  cancel(): void {
    if (this.handle !== null) {
      clearTimeout(this.handle);
      this.handle = null;
    }
  }
}

export interface LiveToolCall {
  id: string;
  providerCallId: string;
  name: string;
  arguments: Record<string, unknown>;
  result: Record<string, unknown> | null;
  error: { code: string; message: string } | null;
  status: ToolCallStatus;
  riskLevel: string;
  durationMs: number;
  approvalId: string;
  startedAt: number;
  /** 六期：tool_result 携带的有效门限（命令执行类工具）。 */
  approvalMode: string;
}

export interface LiveError {
  code: string;
  /** 面向用户的中文提示。 */
  message: string;
  /** 上游返回的英文短句，便于排查。 */
  detail: string;
  requestId: string;
  retryable: boolean;
}

export interface LiveTurn {
  userContent: string;
  text: string;
  reasoning: string;
  /** 本轮首个 reasoning delta 的本地时刻（ms）；无 reasoning 为 null（六期 8.6 第 13 条计时起点）。 */
  reasoningStartedAt: number | null;
  /** 最近一个 reasoning delta 的本地时刻（ms）；ReasoningBlock 摘要「耗时 Ns」的计时终点。 */
  reasoningEndedAt: number | null;
  tools: LiveToolCall[];
  approvals: Approval[];
  error: LiveError | null;
  done: SseDone | null;
  stopped: boolean;
  /** HTTP 层拒绝、服务端未持久化任何消息时为 true。 */
  settled: boolean;
  /** 服务端已开始本轮并写入 user 消息；此时重试会产生重复提问。 */
  persisted: boolean;
  startedAt: number;
}

/** ReasoningBlock 摘要耗时（六期 8.6 第 13 条）：首/尾 reasoning delta 本地时间差；无计时数据为 null。 */
export function liveReasoningDurationMs(live: LiveTurn): number | null {
  if (live.reasoningStartedAt === null || live.reasoningEndedAt === null) {
    return null;
  }
  return Math.max(0, live.reasoningEndedAt - live.reasoningStartedAt);
}

/** 持久化工具调用与流式工具调用共用的视图模型。 */
export interface ToolCallView {
  id: string;
  name: string;
  arguments: Record<string, unknown>;
  result: Record<string, unknown> | null;
  status: string;
  riskLevel: string;
  errorCode: string;
  errorMessage: string;
  durationMs: number;
  /** 六期：命令执行类工具结果携带的有效门限（契约 8.6 判空展示）。 */
  approvalMode: string;
}

function riskOfResult(result: Record<string, unknown> | null): string {
  const risk = result?.['risk_level'];
  return typeof risk === 'string' ? risk : '';
}

export function toToolCallView(toolCall: ToolCallSchema): ToolCallView {
  return {
    id: toolCall.id,
    name: toolCall.name,
    arguments: toolCall.arguments,
    result: toolCall.result,
    status: toolCall.status,
    riskLevel: toolCall.risk_level,
    errorCode: toolCall.error_code,
    errorMessage: toolCall.error_message ?? '',
    durationMs: 0,
    // 六期打磨 sprint 1：tool_calls.approval_mode 持久化（迁移 00010）——
    // 此前写死空串，刷新/重进会话后门限徽标即消失。空串=非命令执行类工具
    // 或旧行未记录（ToolCallCard 据此不渲染徽标）。
    approvalMode: toolCall.approval_mode ?? '',
  };
}

export function toLiveToolView(tool: LiveToolCall): ToolCallView {
  return {
    id: tool.id,
    name: tool.name,
    arguments: tool.arguments,
    result: tool.result,
    status: tool.status,
    riskLevel: tool.riskLevel || riskOfResult(tool.result),
    errorCode: tool.error?.code ?? '',
    errorMessage: tool.error?.message ?? '',
    durationMs: tool.durationMs,
    approvalMode: tool.approvalMode ?? '',
  };
}

/**
 * 工具卡是否展示耗时：denied/expired 终态的命令从未执行
 * （审批被拒 / 审批超时 / 策略禁止），耗时数字没有意义，
 * 展示出来反而会被误读为「执行了很久才失败」。
 */
export function showsDuration(view: ToolCallView): boolean {
  if (view.status === 'denied' || view.status === 'expired') {
    return false;
  }
  return view.durationMs > 0;
}

export interface SendOptions {
  content: string;
  conversationId?: string;
  providerId?: string;
  model?: string;
}

export interface UseChatStreamOptions {
  onUnauthorized?: () => void;
  /** SSE 头之前的 HTTP 拒绝（消息未落库）时回调，用于恢复草稿。 */
  onSendRejected?: (content: string) => void;
}

export interface ApiErrorInfo {
  code: string;
  message: string;
  requestId: string;
  status: number;
}

const ERROR_TEXT: Record<string, string> = {
  AUTH_REQUIRED: '登录已过期，请重新登录。',
  AUTH_INVALID: '登录凭据无效。',
  AUTH_RATE_LIMITED: '尝试过于频繁，请稍后再试。',
  ORIGIN_FORBIDDEN: '请求来源校验失败，请通过正确入口访问。',
  VALIDATION_FAILED: '请求参数不合法。',
  NOT_FOUND: '资源不存在或已被删除。',
  CONFLICT: '操作冲突，请刷新后重试。',
  INTERNAL: '服务内部错误，请稍后重试。',
  SERVICE_UNAVAILABLE: '服务暂不可用，请稍后重试。',
  TOO_MANY_STREAMS: '当前回答请求过多，请稍后重试。',
  STREAM_ACTIVE: '该会话已有正在进行的回答。',
  CONVERSATION_NOT_FOUND: '会话不存在或已被删除。',
  CONVERSATION_MODEL_LOCKED: '会话已绑定其他模型，切换模型请新建会话。',
  NO_PROVIDER_CONFIGURED: '尚未配置模型供应商，请前往模型管理添加。',
  PROVIDER_NOT_FOUND: '模型供应商不存在。',
  PROVIDER_TEST_FAILED: '模型供应商连接测试失败。',
  PROVIDER_MODELS_FAILED: '拉取模型列表失败。',
  LLM_UPSTREAM_ERROR: '模型供应商返回错误。',
  LLM_TIMEOUT: '模型响应超时。',
  LLM_STREAM_ERROR: '模型流解析失败。',
  LLM_OUTPUT_TRUNCATED: '模型输出被长度截断。',
  AGENT_MAX_ITERATIONS: '工具调用次数超过上限。',
  STREAM_TIMEOUT: '回答超时，已停止。',
  APPROVAL_NOT_FOUND: '审批不存在或已过期。',
  APPROVAL_NOT_PENDING: '审批已处理。',
  APPROVAL_DENIED: '命令审批被拒绝。',
  APPROVAL_TIMEOUT: '命令审批超时。',
  // 六期打磨 sprint 3：审批随回答流中断被连带取消（tool_calls.error_code）。
  // 与 APPROVAL_TIMEOUT 区分——后者是等满 TTL 的真实超时。
  APPROVAL_CANCELLED: '回答流已中断，本轮审批已取消。',
  COMMAND_FORBIDDEN: '该命令被风险规则禁止。',
  COMMAND_INVALID: '命令不合法，未执行。',
  // 三期网络设备通道（契约 9.6）：与 termCodes.ts 的既有中文口径一致。
  NETCLI_LOGIN_FAILED: '网络设备登录失败（凭据错误或提示符未出现）',
  NETCLI_EXEC_FAILED: '网络设备命令执行失败（expect 超时或通道断开）',
  // 四期容器工具错误码（契约 9.6），文案与 containers/containerFailures.ts
  // 的 CONTAINER_CODE_MESSAGES 对齐（走查 T8/T9：裸码无中文说明）。
  CONTAINER_UNSUPPORTED: '该服务器未探测到受支持的容器运行时（docker/podman）',
  CONTAINER_NOT_FOUND: '容器不存在或已删除',
  CONTAINER_CLI_FAILED: '容器 CLI 执行失败（daemon 未运行或命令执行失败）',
  // 工具卡错误码（ToolCallCard 也复用本映射渲染中文说明）。
  TOOL_NOT_FOUND: '调用了不支持的工具。',
  TOOL_ARGS_INVALID: '工具调用参数不合法。',
  SERVER_NOT_FOUND: '服务器不存在或已被删除。',
  DATABASE_NOT_FOUND: '数据库不存在或已被删除。',
  SSH_HOST_KEY_CHANGED: '服务器主机密钥已变化，连接被阻止。',
  SSH_TIMEOUT: 'SSH 命令执行超时。',
  SSH_EXEC_FAILED: '远程命令执行失败。',
  SQL_NOT_READONLY: '本系统只执行只读查询，写操作不会执行。',
  SQL_TIMEOUT: 'SQL 查询执行超时。',
  SQL_QUERY_FAILED: 'SQL 查询执行失败。',
  SSE_DISCONNECTED: '连接已断开，回答可能不完整。',
  NETWORK_ERROR: '网络异常，请检查连接后重试。',
  SERVER_SHUTTING_DOWN: '服务正在关闭，请稍后重试。',
  CLIENT_DISCONNECTED: '回答已停止。',
};

export function describeError(code: string, fallback?: string): string {
  if (ERROR_TEXT[code]) {
    return ERROR_TEXT[code];
  }
  if (code.startsWith('HTTP_')) {
    return `请求失败（HTTP ${code.slice(5)}）。`;
  }
  return fallback && fallback.length > 0 ? fallback : code;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function toApiErrorInfo(error: unknown, status = 0, requestId = ''): ApiErrorInfo {
  if (isRecord(error)) {
    // 兼容 FE2 的 ApiRequestError（{ failure: ApiFailure }）。
    const failure = error['failure'];
    if (isRecord(failure) && typeof failure['code'] === 'string') {
      return {
        code: failure['code'],
        message:
          typeof failure['message'] === 'string' ? failure['message'] : '请求失败，请稍后重试。',
        requestId: typeof failure['requestId'] === 'string' ? failure['requestId'] : requestId,
        status: typeof failure['status'] === 'number' ? failure['status'] : status,
      };
    }
    const body = error['error'];
    if (isRecord(body)) {
      const code = typeof body['code'] === 'string' ? body['code'] : 'UNKNOWN';
      const message =
        typeof body['message'] === 'string' ? body['message'] : '请求失败，请稍后重试。';
      const id = typeof body['request_id'] === 'string' ? body['request_id'] : requestId;
      return { code, message, requestId: id, status };
    }
    if (typeof error['code'] === 'string') {
      return {
        code: error['code'],
        message: typeof error['message'] === 'string' ? error['message'] : '请求失败，请稍后重试。',
        requestId,
        status,
      };
    }
    if (typeof error['message'] === 'string') {
      return { code: 'UNKNOWN', message: error['message'], requestId, status };
    }
  }
  if (typeof error === 'string') {
    return { code: 'UNKNOWN', message: error, requestId, status };
  }
  return {
    code: status === 401 ? 'AUTH_REQUIRED' : 'UNKNOWN',
    message: '请求失败，请稍后重试。',
    requestId,
    status,
  };
}

export function isUnauthorizedError(error: unknown): boolean {
  if (!isRecord(error)) {
    return false;
  }
  if (error['code'] === 'AUTH_REQUIRED') {
    return true;
  }
  const failure = error['failure'];
  if (isRecord(failure) && failure['code'] === 'AUTH_REQUIRED') {
    return true;
  }
  const body = error['error'];
  return isRecord(body) && body['code'] === 'AUTH_REQUIRED';
}

function parseFrameData<T>(data: string): T | null {
  try {
    return JSON.parse(data) as T;
  } catch {
    return null;
  }
}

function cloneLive(live: LiveTurn): LiveTurn {
  return { ...live, tools: [...live.tools], approvals: [...live.approvals] };
}

/**
 * 判断持久化的 assistant error 消息是否对应某个 live 回合。
 *
 * 服务端失败时落库的是「已收到的部分内容」（CONTRACT.md 5.8 第 7 步），可能
 * 包含服务端已 emit、但前端 abort/断流后未读到的 token，因此落库内容是前端
 * live.text 的超集；逐字符相等会把两者判为不同消息，导致同一轮对话渲染两份。
 * 用前缀匹配识别同一条消息。
 *
 * live.text 为空串时 startsWith 恒为 true，会把任意历史 error 消息误判为
 * 本轮；此时要求落库内容同样为空（空回答的失败轮）。
 */
export function matchesLiveErrorTurn(persistedContent: string, liveText: string): boolean {
  if (liveText.length === 0) {
    return persistedContent.length === 0;
  }
  return persistedContent.startsWith(liveText);
}

/** computeLiveSettled 需要的消息字段子集（Message 的 Pick）。 */
export interface SettledMessageView {
  id: string;
  role: string;
  status?: string;
  content: string;
}

/**
 * live 回合是否可从页面撤除（ChatPage liveSettled 的判定本体，抽出为
 * 纯函数以便对竞态时序做矩阵回归）。
 *
 * done 分支只看 messages：done 触发的 messages 与 toolCalls invalidate
 * 是两个并发 refetch，存在落地间隙。若撤除 live 还要等 toolCalls，
 * 间隙内会同时渲染「live 条目」与「持久化 assistant 消息」两份正文
 * （第 3 轮打磨实测：审批超时 E2E 全量并行下 getByText 撞进毫秒级
 * 间隙，strict mode 双元素失败）。因此正文切换只以 messages 为准；
 * 间隙内工具卡由 MessageList 的 liveToolsFallbackTurnId 回退渲染
 * （见该函数注释），卡片不会消失。
 *
 * 其余分支（HTTP 层拒绝 settled / 断流与停止的 error 轮前缀匹配）与
 * ChatPage 原实现一致。
 */
export function computeLiveSettled(
  live: LiveTurn | null,
  messages: ReadonlyArray<SettledMessageView>,
): boolean {
  if (!live) {
    return true;
  }
  if (live.settled) {
    return true;
  }
  if (live.done) {
    const doneId = live.done.message_id;
    return messages.some((message) => message.id === doneId);
  }
  if (live.error || live.stopped) {
    // 只从列表末尾向前找：本轮失败时 assistant error 消息总是最新一条；
    // 遇到本轮的 user 消息即停止——它已落库而 assistant 尚未写，说明最终
    // 落库（WithoutCancel+5s）还在路上，live 必须继续显示，否则该轮从页
    // 面直接消失。全表扫描会把更早轮次的空 error 消息误判为本轮结果。
    for (let index = messages.length - 1; index >= 0; index -= 1) {
      const message = messages[index];
      if (!message) {
        continue;
      }
      if (message.role === 'user') {
        return false;
      }
      if (
        message.role === 'assistant' &&
        message.status === 'error' &&
        matchesLiveErrorTurn(message.content, live.text)
      ) {
        return true;
      }
    }
    return false;
  }
  return false;
}

/**
 * done 轮工具卡的回退渲染判定（MessageList 使用）。
 *
 * 竞态背景：done 帧处理里 messages 与 toolCalls 的 invalidate 是两个
 * 并发 refetch。live 回合按 messages 落地即撤除（见 computeLiveSettled），
 * 若 toolCalls 尚未落地，MessageList 的 toolsByTurn 没有本轮分组——
 * 工具卡会从页面消失（快上游无 token 间隔时间歇复现，第 3 轮打磨
 * 实测 13 轮 3 轮）。live 状态对象在下一轮 send 前不会被清除，因此
 * 这里以它为回退数据源：done 消息已持久化（live 条目已撤）但该 turn
 * 的持久化工具分组为空时，返回该 turn_id，由 MessageList 用 live.tools
 * 补渲染；toolCalls 落地后分组出现，回退自动失效。后端先落库再 emit
 * （agent/runner.go Create → emit），done 帧到达时记录必已在库，间隙
 * 只是前端 refetch 时序，不会永久等待。
 *
 * 返回 null 表示无需回退（无工具 / done 消息未落地 / 分组已存在）。
 */
export function liveToolsFallbackTurnId(
  live: LiveTurn | null,
  messages: ReadonlyArray<{ id: string; turn_id: string }>,
  persistedToolCallTurns: ReadonlySet<string>,
): string | null {
  if (!live?.done || live.tools.length === 0) {
    return null;
  }
  const doneMessage = messages.find((message) => message.id === live.done?.message_id);
  if (!doneMessage) {
    // done 消息未落地：live 条目仍在渲染（含工具卡），无需回退。
    return null;
  }
  return persistedToolCallTurns.has(doneMessage.turn_id) ? null : doneMessage.turn_id;
}

function toolStatusFromError(code: string): ToolCallStatus {
  if (code === 'APPROVAL_DENIED') {
    return 'denied';
  }
  if (code === 'APPROVAL_TIMEOUT') {
    return 'expired';
  }
  return 'failed';
}

/**
 * 停止回答时把 live 里 pending 的审批置为服务端同款终态（第 4 轮打磨）。
 *
 * 服务端在回答流断开时会 Cancel 审批为 expired + note=stream_cancelled
 * （internal/agent/runner.go Wait 返回后、agent/runner.go:1072-1080 两条
 * 路径）；但 stop() 只 abort 本地 SSE 连接，前端收不到任何后续帧，
 * live.approvals 里残留的 pending 快照会（经 MessageList mergedApprovalsByTool
 * 的 live 同 id 覆盖）压住落库终态：卡片在 TTL 内死锁显示「待处理」并继续
 * 倒计时，批准按钮可点但必返回 409。这里同步写入与服务端一致的终态，
 * 卡片立即显示「已取消（回答流已中断，审批未生效）」，按钮随之不可点。
 */
export function cancelPendingApprovals(approvals: Approval[], now: string): Approval[] {
  return approvals.map((approval) =>
    approval.status === 'pending'
      ? { ...approval, status: 'expired' as const, note: 'stream_cancelled', decided_at: now }
      : approval,
  );
}

/**
 * 审批决策后补偿 invalidate 的延迟点（毫秒）。
 *
 * 非流发起窗口（第二个浏览器窗口打开同一会话）里 decide 成功时，卡片
 * 终态靠 approvals 的 invalidate 同步，但后续 tool_result 与最终回答
 * 依赖流连接内的 done 帧触发 refetch——该窗口没有流连接也没有轮询，
 * 落库结果只能手动刷新才能看到（第 10 轮打磨双窗口实测：批准后 3s+
 * 最终回答已落库而 B 窗消息区为空）。决策后后端会继续执行命令、完成
 * 本轮并落库，这里按落库窗口做几次延迟 invalidate，无需引入广播。
 */
export const APPROVAL_REFETCH_DELAYS_MS = [2_000, 5_000, 10_000] as const;

/**
 * 按延迟点调度审批决策后的补偿 invalidate（导出供单测断言调度行为）。
 * schedule 默认用全局 setTimeout；测试注入收集型调度器做行为断言。
 */
export function scheduleApprovalRefetch(
  cid: string,
  invalidate: (cid: string) => void,
  schedule: (fn: () => void, delayMs: number) => TimeoutHandle = (fn, delayMs) =>
    setTimeout(fn, delayMs),
): void {
  for (const delayMs of APPROVAL_REFETCH_DELAYS_MS) {
    schedule(() => invalidate(cid), delayMs);
  }
}

async function readHttpError(response: Response): Promise<ApiErrorInfo> {
  const headerId = response.headers.get('X-Request-ID') ?? '';
  try {
    const body: unknown = await response.json();
    const info = toApiErrorInfo(body, response.status, headerId);
    if (info.code !== 'UNKNOWN') {
      return info;
    }
  } catch {
    // 非 JSON 错误体，退回状态码提示。
  }
  return {
    code: response.status === 401 ? 'AUTH_REQUIRED' : `HTTP_${response.status}`,
    message: response.statusText || '请求失败，请稍后重试。',
    requestId: headerId,
    status: response.status,
  };
}

export function useChatStream(
  conversationId: string | undefined,
  options: UseChatStreamOptions = {},
) {
  const queryClient = useQueryClient();
  const [liveState, setLiveState] = useState<{
    cid: string | undefined;
    live: LiveTurn | null;
  }>({ cid: undefined, live: null });
  const [phaseState, setPhaseState] = useState<{
    cid: string | undefined;
    phase: StreamPhase;
  }>({ cid: undefined, phase: 'idle' });
  // 状态与发起请求的会话绑定，切换会话后旧流不会串到新会话。
  const live = liveState.cid === conversationId ? liveState.live : null;
  const phase = phaseState.cid === conversationId ? phaseState.phase : 'idle';

  const liveRef = useRef<LiveTurn | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const runIdRef = useRef(0);
  const textBufferRef = useRef<StreamBuffer | null>(null);
  const reasoningBufferRef = useRef<StreamBuffer | null>(null);
  const lastRequestRef = useRef<{
    conversationId: string;
    content: string;
    providerId: string | undefined;
    model: string | undefined;
  } | null>(null);

  const onUnauthorizedRef = useRef(options.onUnauthorized);
  useEffect(() => {
    onUnauthorizedRef.current = options.onUnauthorized;
  }, [options.onUnauthorized]);

  const onSendRejectedRef = useRef(options.onSendRejected);
  useEffect(() => {
    onSendRejectedRef.current = options.onSendRejected;
  }, [options.onSendRejected]);

  const disposeBuffers = useCallback(() => {
    textBufferRef.current?.dispose();
    textBufferRef.current = null;
    reasoningBufferRef.current?.dispose();
    reasoningBufferRef.current = null;
  }, []);

  const invalidateAfterStream = useCallback(
    (cid: string) => {
      void queryClient.invalidateQueries({ queryKey: qk.messages(cid) });
      void queryClient.invalidateQueries({ queryKey: qk.conversation(cid) });
      void queryClient.invalidateQueries({ queryKey: qk.conversations });
      void queryClient.invalidateQueries({ queryKey: qk.toolCalls(cid) });
      void queryClient.invalidateQueries({ queryKey: qk.approvals(cid) });
    },
    [queryClient],
  );

  // 停止/断流/网络错误路径专用：服务端此时仍在异步落库（CONTRACT.md 5.8
  // 第 7 步：WithoutCancel(ctx)+5s 超时写部分内容与 error 状态），与 abort
  // 后立即发出的 refetch 是并发关系，谁先完成不确定。queryClient 默认
  // staleTime 15s、无轮询，若第一次 refetch 抢先，live 回合会因消息未出现
  // 而永久残留。这里在落库窗口（5s）结束后再补偿一次 invalidate。
  const invalidateAfterStreamFailure = useCallback(
    (cid: string) => {
      invalidateAfterStream(cid);
      window.setTimeout(() => invalidateAfterStream(cid), 6000);
    },
    [invalidateAfterStream],
  );

  // 切换会话或卸载：中止进行中的流，丢弃本地流状态。
  useEffect(() => {
    return () => {
      runIdRef.current += 1;
      abortRef.current?.abort();
      abortRef.current = null;
      disposeBuffers();
      liveRef.current = null;
      setLiveState({ cid: conversationId, live: null });
      setPhaseState({ cid: conversationId, phase: 'idle' });
    };
  }, [conversationId, disposeBuffers]);

  const send = useCallback(
    async (opts: SendOptions) => {
      const cid = opts.conversationId ?? conversationId;
      const content = opts.content.trim();
      if (!cid || !content) {
        return;
      }

      runIdRef.current += 1;
      const runId = runIdRef.current;
      abortRef.current?.abort();
      disposeBuffers();

      const controller = new AbortController();
      abortRef.current = controller;
      lastRequestRef.current = {
        conversationId: cid,
        content,
        providerId: opts.providerId,
        model: opts.model,
      };

      const initial: LiveTurn = {
        userContent: content,
        text: '',
        reasoning: '',
        reasoningStartedAt: null,
        reasoningEndedAt: null,
        tools: [],
        approvals: [],
        error: null,
        done: null,
        stopped: false,
        settled: false,
        persisted: false,
        startedAt: Date.now(),
      };
      liveRef.current = initial;
      setLiveState({ cid, live: initial });
      setPhaseState({ cid, phase: 'connecting' });

      const mutate = (fn: (current: LiveTurn) => void): void => {
        if (runId !== runIdRef.current) {
          return;
        }
        const current = liveRef.current;
        if (!current) {
          return;
        }
        fn(current);
        setLiveState({ cid, live: cloneLive(current) });
      };

      const textBuffer = new StreamBuffer({
        onFlush: (delta) => {
          if (runId !== runIdRef.current) {
            return;
          }
          const current = liveRef.current;
          if (!current) {
            return;
          }
          current.text += delta;
          setLiveState({ cid, live: cloneLive(current) });
        },
      });
      const reasoningBuffer = new StreamBuffer({
        onFlush: (delta) => {
          if (runId !== runIdRef.current) {
            return;
          }
          const current = liveRef.current;
          if (!current) {
            return;
          }
          current.reasoning += delta;
          // 首/尾 delta 本地时间差（六期 8.6 第 13 条，ReasoningBlock 摘要
          // 「耗时 Ns」）：直接写 liveRef，随下方 cloneLive 的浅拷贝一起
          // 落地，不额外触发渲染。
          if (current.reasoningStartedAt === null) {
            current.reasoningStartedAt = Date.now();
          }
          current.reasoningEndedAt = Date.now();
          setLiveState({ cid, live: cloneLive(current) });
        },
      });
      textBufferRef.current = textBuffer;
      reasoningBufferRef.current = reasoningBuffer;

      const flushAll = (): void => {
        textBuffer.flush();
        reasoningBuffer.flush();
      };

      let terminal = false;
      let receivedAnyFrame = false;

      // 空闲看门狗（见 STREAM_IDLE_TIMEOUT_MS / shouldHoldIdleWatchdog）：
      // 连接被代理挂住时 reader 永远挂起，只能靠客户端超时中止。处于合法
      // 无帧窗口（审批等待 / 工具执行 / 首 token 前）时续期重评；窗口外
      // 仍无帧则先落 NETWORK_ERROR 再 abort——catch 分支对「主动中止」的
      // 提前返回不会吞掉这里的提示；服务端随后照常落库 error 消息。
      const watchdog = new IdleWatchdog(STREAM_IDLE_TIMEOUT_MS, () => {
        if (runId !== runIdRef.current || terminal) {
          return;
        }
        if (shouldHoldIdleWatchdog(liveRef.current, Date.now(), receivedAnyFrame)) {
          watchdog.arm();
          return;
        }
        mutate((current) => {
          current.error = {
            code: 'NETWORK_ERROR',
            message: `网络无响应：${STREAM_IDLE_TIMEOUT_MS / 1000} 秒未收到新数据，请检查网络或反向代理后重试。`,
            detail: 'idle watchdog fired: no SSE frame within timeout',
            requestId: '',
            retryable: true,
          };
        });
        terminal = true;
        invalidateAfterStreamFailure(cid);
        controller.abort();
      });

      const handleFrame = (frame: SseFrame): void => {
        switch (frame.event) {
          case 'token': {
            const payload = parseFrameData<{ delta?: unknown }>(frame.data);
            if (payload && typeof payload.delta === 'string') {
              textBuffer.append(payload.delta);
            }
            break;
          }
          case 'reasoning': {
            const payload = parseFrameData<{ delta?: unknown }>(frame.data);
            if (payload && typeof payload.delta === 'string') {
              reasoningBuffer.append(payload.delta);
            }
            break;
          }
          case 'tool_call': {
            flushAll();
            const payload = parseFrameData<SseToolCall>(frame.data);
            if (!payload || typeof payload.tool_call_id !== 'string') {
              break;
            }
            const args = isRecord(payload.arguments) ? payload.arguments : {};
            mutate((current) => {
              const tool: LiveToolCall = {
                id: payload.tool_call_id,
                providerCallId:
                  typeof payload.provider_call_id === 'string' ? payload.provider_call_id : '',
                name: typeof payload.name === 'string' ? payload.name : 'tool',
                arguments: args,
                result: null,
                error: null,
                status: 'started',
                riskLevel: '',
                durationMs: 0,
                approvalId: '',
                approvalMode: '',
                startedAt: Date.now(),
              };
              current.tools = [...current.tools.filter((item) => item.id !== tool.id), tool];
            });
            break;
          }
          case 'tool_result': {
            flushAll();
            const payload = parseFrameData<SseToolResult>(frame.data);
            if (!payload || typeof payload.tool_call_id !== 'string') {
              break;
            }
            const errorInfo =
              payload.error && typeof payload.error.code === 'string'
                ? { code: payload.error.code, message: String(payload.error.message ?? '') }
                : null;
            const reporterStatus =
              payload.result && typeof payload.result['status'] === 'string'
                ? payload.result['status']
                : '';
            let status: ToolCallStatus;
            if (payload.ok) {
              status = reporterStatus === 'awaiting_user' ? 'awaiting_user' : 'succeeded';
            } else {
              status = toolStatusFromError(errorInfo?.code ?? '');
            }
            const result = isRecord(payload.result) ? payload.result : null;
            const risk =
              result && typeof result['risk_level'] === 'string' ? result['risk_level'] : '';
            // 六期（契约 4210）：tool_result 携带 approval_mode（命令执行类
            // 工具）——前端消费展示门限徽标（此前未读取，六期评审发现）。
            const approvalMode = typeof payload.approval_mode === 'string' ? payload.approval_mode : '';
            mutate((current) => {
              let found = false;
              current.tools = current.tools.map((tool) => {
                if (tool.id !== payload.tool_call_id) {
                  return tool;
                }
                found = true;
                return {
                  ...tool,
                  status,
                  result,
                  error: errorInfo,
                  durationMs: typeof payload.duration_ms === 'number' ? payload.duration_ms : 0,
                  riskLevel: risk || tool.riskLevel,
                  approvalMode: approvalMode || tool.approvalMode,
                };
              });
              if (!found) {
                current.tools = [
                  ...current.tools,
                  {
                    id: payload.tool_call_id,
                    providerCallId: '',
                    name: typeof payload.name === 'string' ? payload.name : 'tool',
                    arguments: {},
                    result,
                    error: errorInfo,
                    status,
                    riskLevel: risk,
                    durationMs: typeof payload.duration_ms === 'number' ? payload.duration_ms : 0,
                    approvalId: '',
                    approvalMode: '',
                    startedAt: Date.now(),
                  },
                ];
              }
            });
            break;
          }
          case 'approval_required': {
            flushAll();
            const approval = parseFrameData<Approval>(frame.data);
            if (!approval || typeof approval.id !== 'string') {
              break;
            }
            mutate((current) => {
              const exists = current.approvals.some((item) => item.id === approval.id);
              current.approvals = exists
                ? current.approvals.map((item) => (item.id === approval.id ? approval : item))
                : [...current.approvals, approval];
              current.tools = current.tools.map((tool) =>
                tool.id === approval.tool_call_id
                  ? {
                      ...tool,
                      status: 'awaiting_approval',
                      riskLevel: approval.risk_level,
                      approvalId: approval.id,
                    }
                  : tool,
              );
            });
            break;
          }
          case 'approval_resolved': {
            flushAll();
            const payload = parseFrameData<SseApprovalResolved>(frame.data);
            if (!payload || typeof payload.approval_id !== 'string') {
              break;
            }
            const status: ApprovalStatus =
              payload.status === 'approved' ||
              payload.status === 'denied' ||
              payload.status === 'expired'
                ? payload.status
                : 'expired';
            mutate((current) => {
              current.approvals = current.approvals.map((approval) =>
                approval.id === payload.approval_id
                  ? { ...approval, status, decided_at: payload.decided_at ?? null }
                  : approval,
              );
            });
            // 回填到持久化审批列表（可能由其他窗口决策）。
            void queryClient.invalidateQueries({ queryKey: qk.approvals(cid) });
            break;
          }
          case 'done': {
            flushAll();
            const payload = parseFrameData<SseDone>(frame.data);
            if (payload && typeof payload.message_id === 'string') {
              mutate((current) => {
                current.done = payload;
              });
            }
            terminal = true;
            invalidateAfterStream(cid);
            break;
          }
          case 'error': {
            flushAll();
            const payload = parseFrameData<SseError>(frame.data);
            const code = payload && typeof payload.code === 'string' ? payload.code : 'INTERNAL';
            const detail = payload && typeof payload.message === 'string' ? payload.message : '';
            mutate((current) => {
              current.error = {
                code,
                message: describeError(code, detail),
                detail,
                requestId:
                  payload && typeof payload.request_id === 'string' ? payload.request_id : '',
                retryable: Boolean(payload?.retryable),
              };
            });
            terminal = true;
            invalidateAfterStream(cid);
            if (code === 'AUTH_REQUIRED') {
              onUnauthorizedRef.current?.();
            }
            break;
          }
          default:
            break;
        }
      };

      try {
        watchdog.arm();
        const body: components['schemas']['ChatRequest'] = {
          conversation_id: cid,
          content,
          ...(opts.providerId ? { provider_id: opts.providerId } : {}),
          ...(opts.model ? { model: opts.model } : {}),
        };

        const response = await fetch('/api/chat', {
          method: 'POST',
          credentials: 'same-origin',
          headers: {
            'Content-Type': 'application/json',
            Accept: 'text/event-stream',
            Origin: window.location.origin,
          },
          body: JSON.stringify(body),
          signal: controller.signal,
        });

        if (runId !== runIdRef.current) {
          void response.body?.cancel();
          return;
        }

        const contentType = response.headers.get('Content-Type') ?? '';
        if (!response.ok || !contentType.includes('text/event-stream')) {
          const info = await readHttpError(response);
          if (runId !== runIdRef.current) {
            return;
          }
          mutate((current) => {
            current.error = {
              code: info.code,
              message: describeError(info.code, info.message),
              detail: info.message,
              requestId: info.requestId,
              retryable: response.status >= 500 || response.status === 429,
            };
            // SSE 头之前的校验/供应商错误不会落库任何消息；该回合到此结束，
            // 避免留下一条永不消失的“正在思考”或虚假用户消息。
            current.settled = true;
          });
          terminal = true;
          invalidateAfterStream(cid);
          // 该消息没有落库，恢复草稿以免用户输入丢失。
          onSendRejectedRef.current?.(content);
          if (info.code === 'AUTH_REQUIRED' || response.status === 401) {
            onUnauthorizedRef.current?.();
          }
          return;
        }

        setPhaseState({ cid, phase: 'streaming' });
        // 进入 SSE 表示服务端已开始本轮：user 消息已落库，重试会重复提问。
        mutate((current) => {
          current.persisted = true;
        });
        if (!response.body) {
          throw new Error('response body is empty');
        }

        for await (const frame of readSseStream(response.body)) {
          if (runId !== runIdRef.current) {
            break;
          }
          receivedAnyFrame = true;
          watchdog.arm();
          handleFrame(frame);
        }

        if (!terminal && runId === runIdRef.current) {
          mutate((current) => {
            current.error = {
              code: 'SSE_DISCONNECTED',
              message: describeError('SSE_DISCONNECTED'),
              detail: 'stream ended without a done or error event',
              requestId: '',
              retryable: true,
            };
          });
          invalidateAfterStreamFailure(cid);
        }
      } catch (error) {
        if (runId !== runIdRef.current) {
          return;
        }
        if (controller.signal.aborted) {
          return;
        }
        const detail = error instanceof Error ? error.message : '';
        mutate((current) => {
          current.error = {
            code: 'NETWORK_ERROR',
            message: describeError('NETWORK_ERROR'),
            detail,
            requestId: '',
            retryable: true,
          };
        });
        invalidateAfterStreamFailure(cid);
      } finally {
        watchdog.cancel();
        if (runId === runIdRef.current) {
          flushAll();
          disposeBuffers();
          abortRef.current = null;
          setPhaseState({ cid, phase: 'idle' });
        }
      }
    },
    [
      conversationId,
      disposeBuffers,
      invalidateAfterStream,
      invalidateAfterStreamFailure,
      queryClient,
    ],
  );

  const stop = useCallback(() => {
    if (!abortRef.current) {
      return;
    }
    // 先把已收到的 token 显示出来，再作废本次运行。
    textBufferRef.current?.flush();
    reasoningBufferRef.current?.flush();
    const current = liveRef.current;
    runIdRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    disposeBuffers();
    if (current) {
      current.stopped = true;
      // 停止即作废本轮未决审批（服务端 Cancel 同款终态，见
      // cancelPendingApprovals 注释）：卡片立即终态，不再死锁「待处理」。
      current.approvals = cancelPendingApprovals(current.approvals, new Date().toISOString());
      setLiveState({ cid: conversationId, live: cloneLive(current) });
    }
    setPhaseState({ cid: conversationId, phase: 'idle' });
    if (conversationId) {
      invalidateAfterStreamFailure(conversationId);
    }
  }, [conversationId, disposeBuffers, invalidateAfterStreamFailure]);

  const retry = useCallback(() => {
    const last = lastRequestRef.current;
    if (!last) {
      return;
    }
    // 服务端已写入本轮 user 消息时，重发同一 content 会造成重复提问。
    if (liveRef.current?.persisted) {
      return;
    }
    void send({
      conversationId: last.conversationId,
      content: last.content,
      ...(last.providerId ? { providerId: last.providerId } : {}),
      ...(last.model ? { model: last.model } : {}),
    });
  }, [send]);

  const applyApproval = useCallback(
    (approval: Approval) => {
      const current = liveRef.current;
      if (current) {
        current.approvals = current.approvals.map((item) =>
          item.id === approval.id ? approval : item,
        );
        setLiveState({ cid: conversationId, live: cloneLive(current) });
      }
      if (conversationId) {
        void queryClient.invalidateQueries({ queryKey: qk.approvals(conversationId) });
        // 非流窗口（第二个浏览器窗口）决策后的补偿 refetch：拉取决策后
        // 继续执行落库的 tool_result 与最终回答（见 scheduleApprovalRefetch）。
        scheduleApprovalRefetch(conversationId, (cid) => {
          void queryClient.invalidateQueries({ queryKey: qk.messages(cid) });
          void queryClient.invalidateQueries({ queryKey: qk.toolCalls(cid) });
        });
      }
    },
    [conversationId, queryClient],
  );

  const clearError = useCallback(() => {
    const current = liveRef.current;
    if (!current || !current.error) {
      return;
    }
    if (current.settled) {
      // HTTP 层拒绝没有持久化内容，忽略错误后也不应保留该 live 回合。
      liveRef.current = null;
      setLiveState({ cid: conversationId, live: null });
      return;
    }
    current.error = null;
    if (!current.done && !current.stopped) {
      // 断流/网络错误后清除错误横幅时，该回合内容可能被截断：保留
      // “已停止”标记（MessageList 据此显示 Tag），避免已截断的部分文本
      // 以完整回答的外观留存。
      current.stopped = true;
    }
    setLiveState({ cid: conversationId, live: cloneLive(current) });
  }, [conversationId]);

  const clearLive = useCallback(() => {
    liveRef.current = null;
    setLiveState({ cid: conversationId, live: null });
  }, [conversationId]);

  return { live, phase, send, stop, retry, applyApproval, clearError, clearLive };
}
