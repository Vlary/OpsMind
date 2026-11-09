import { describe, expect, it } from 'vitest';

import { ApiRequestError, makeFailure } from '../../../components/admin/apiError';
import {
  describeError,
  isUnauthorizedError,
  toApiErrorInfo,
  toLiveToolView,
  toToolCallView,
  type LiveToolCall,
} from '../useChatStream';

// 聊天链路错误归一（toApiErrorInfo / isUnauthorizedError）与工具调用视图模型，
// 补充既有 useChatStream.test.ts 未覆盖的错误形态与映射分支。

describe('toApiErrorInfo 错误形态归一', () => {
  it('FE2 的 ApiRequestError（{failure}）被识别并透传', () => {
    const failure = makeFailure('CONFLICT', 'stream active');
    const info = toApiErrorInfo(new ApiRequestError({ ...failure, status: 409 }), 500, 'r0');
    expect(info).toEqual({
      code: 'CONFLICT',
      message: 'stream active',
      requestId: '',
      status: 409,
    });
  });

  it('契约错误体 {error:{code,message,request_id}} 优先于默认值', () => {
    const info = toApiErrorInfo(
      { error: { code: 'LLM_TIMEOUT', message: 'upstream slow', request_id: 'req-9' } },
      504,
      'hdr',
    );
    expect(info).toEqual({
      code: 'LLM_TIMEOUT',
      message: 'upstream slow',
      requestId: 'req-9',
      status: 504,
    });
  });

  it('error 体缺 code 时兜底 UNKNOWN，缺 message 时给中文默认', () => {
    const info = toApiErrorInfo({ error: {} }, 500, 'hdr');
    expect(info.code).toBe('UNKNOWN');
    expect(info.message).toBe('请求失败，请稍后重试。');
  });

  it('裸 {code,message} record 直接转 info（SSE/HTTP 层包装形态）', () => {
    expect(toApiErrorInfo({ code: 'STREAM_ACTIVE', message: 'busy' }, 409, 'r')).toEqual({
      code: 'STREAM_ACTIVE',
      message: 'busy',
      requestId: 'r',
      status: 409,
    });
  });

  it('只有 message 的对象归为 UNKNOWN；字符串错误透传 message', () => {
    expect(toApiErrorInfo({ message: '仅消息' }, 0, '')).toEqual({
      code: 'UNKNOWN',
      message: '仅消息',
      requestId: '',
      status: 0,
    });
    expect(toApiErrorInfo('字符串错误', 0, '')).toEqual({
      code: 'UNKNOWN',
      message: '字符串错误',
      requestId: '',
      status: 0,
    });
  });

  it('无可用信息的 401 归为 AUTH_REQUIRED（触发重新登录），其余 UNKNOWN', () => {
    expect(toApiErrorInfo(null, 401, '').code).toBe('AUTH_REQUIRED');
    expect(toApiErrorInfo(null, 500, '').code).toBe('UNKNOWN');
    expect(toApiErrorInfo(undefined, 0, 'r1').message).toBe('请求失败，请稍后重试。');
  });
});

describe('isUnauthorizedError 三种携带形态', () => {
  it('顶层 code / failure.code / error.code 均识别 AUTH_REQUIRED', () => {
    expect(isUnauthorizedError({ code: 'AUTH_REQUIRED' })).toBe(true);
    expect(isUnauthorizedError({ failure: { code: 'AUTH_REQUIRED' } })).toBe(true);
    expect(isUnauthorizedError({ error: { code: 'AUTH_REQUIRED' } })).toBe(true);
  });

  it('其他错误码与非对象输入返回 false', () => {
    expect(isUnauthorizedError({ code: 'AUTH_INVALID' })).toBe(false);
    expect(isUnauthorizedError({ failure: { code: 'INTERNAL' } })).toBe(false);
    expect(isUnauthorizedError(new Error('x'))).toBe(false);
    expect(isUnauthorizedError('AUTH_REQUIRED')).toBe(false);
    expect(isUnauthorizedError(null)).toBe(false);
  });
});

describe('describeError 流式链路错误码覆盖', () => {
  it('会话与供应商类错误码映射中文', () => {
    expect(describeError('CONVERSATION_NOT_FOUND')).toBe('会话不存在或已被删除。');
    expect(describeError('CONVERSATION_MODEL_LOCKED')).toBe(
      '会话已绑定其他模型，切换模型请新建会话。',
    );
    expect(describeError('NO_PROVIDER_CONFIGURED')).toBe(
      '尚未配置模型供应商，请前往模型管理添加。',
    );
    expect(describeError('PROVIDER_MODELS_FAILED')).toBe('拉取模型列表失败。');
  });

  it('流中断与停止类错误码映射中文', () => {
    expect(describeError('SSE_DISCONNECTED')).toBe('连接已断开，回答可能不完整。');
    expect(describeError('NETWORK_ERROR')).toBe('网络异常，请检查连接后重试。');
    expect(describeError('SERVER_SHUTTING_DOWN')).toBe('服务正在关闭，请稍后重试。');
    expect(describeError('CLIENT_DISCONNECTED')).toBe('回答已停止。');
    expect(describeError('STREAM_TIMEOUT')).toBe('回答超时，已停止。');
  });

  it('审批与限流类错误码映射中文', () => {
    expect(describeError('APPROVAL_NOT_FOUND')).toBe('审批不存在或已过期。');
    expect(describeError('APPROVAL_NOT_PENDING')).toBe('审批已处理。');
    expect(describeError('APPROVAL_TIMEOUT')).toBe('命令审批超时。');
    expect(describeError('TOO_MANY_STREAMS')).toBe('当前回答请求过多，请稍后重试。');
    expect(describeError('AUTH_RATE_LIMITED')).toBe('尝试过于频繁，请稍后再试。');
  });

  it('模型输出与迭代上限类错误码映射中文', () => {
    expect(describeError('LLM_OUTPUT_TRUNCATED')).toBe('模型输出被长度截断。');
    expect(describeError('AGENT_MAX_ITERATIONS')).toBe('工具调用次数超过上限。');
    expect(describeError('LLM_UPSTREAM_ERROR')).toBe('模型供应商返回错误。');
  });
});

describe('工具调用视图模型', () => {
  it('toToolCallView：持久化 toolCall 的 snake_case 字段映射到视图', () => {
    const view = toToolCallView({
      id: 'tc1',
      seq: 1,
      conversation_id: 'c1',
      turn_id: 't1',
      provider_call_id: 'p1',
      name: 'ssh_exec',
      arguments: { command: 'uptime' },
      result: { exit_code: 0 },
      risk_level: 'needs_approval',
      status: 'succeeded',
      error_code: '',
      error_message: '',
      approval_mode: 'none',
      started_at: '2026-01-01T00:00:00Z',
      finished_at: '2026-01-01T00:00:01Z',
    });
    expect(view).toMatchObject({
      id: 'tc1',
      name: 'ssh_exec',
      status: 'succeeded',
      riskLevel: 'needs_approval',
      errorCode: '',
      errorMessage: '',
      // 六期打磨 sprint 1：门限快照随 tool_calls 行落库后映射进视图，
      // 刷新/重进会话仍渲染门限徽标（此前写死空串，徽标只在流式当帧存在）。
      approvalMode: 'none',
    });
  });

  it('toToolCallView：approval_mode 空串（非命令执行类工具/旧行）映射为空串', () => {
    const view = toToolCallView({
      id: 'tc-empty-mode',
      seq: 3,
      conversation_id: 'c1',
      turn_id: 't1',
      provider_call_id: 'p3',
      name: 'list_servers',
      arguments: {},
      result: null,
      risk_level: 'safe',
      status: 'succeeded',
      error_code: '',
      error_message: '',
      approval_mode: '',
      started_at: '2026-01-01T00:00:00Z',
      finished_at: '2026-01-01T00:00:01Z',
    });
    expect(view.approvalMode).toBe('');
  });

  it('toToolCallView：历史失败工具卡的底层错误消息透出到视图', () => {
    // 刷新/重进会话后工具卡不只显示错误码：落库的 error_message
    // 必须映射到 errorMessage（ToolCallCard 的 errorDetail 已渲染）。
    const view = toToolCallView({
      id: 'tc2',
      seq: 2,
      conversation_id: 'c1',
      turn_id: 't1',
      provider_call_id: 'p2',
      name: 'ssh_exec',
      arguments: { command: 'uptime' },
      result: null,
      risk_level: 'safe',
      status: 'failed',
      error_code: 'SSH_EXEC_FAILED',
      error_message: 'sshx: dial 127.0.0.1:22: connect: connection refused',
      approval_mode: '',
      started_at: '2026-01-01T00:00:00Z',
      finished_at: '2026-01-01T00:00:01Z',
    });
    expect(view.errorCode).toBe('SSH_EXEC_FAILED');
    expect(view.errorMessage).toBe('sshx: dial 127.0.0.1:22: connect: connection refused');
  });

  it('toLiveToolView：riskLevel 为空时回退 result.risk_level', () => {
    const tool: LiveToolCall = {
      id: 'live1',
      providerCallId: 'pc',
      name: 'ssh_exec',
      arguments: {},
      result: { risk_level: 'needs_approval' },
      error: null,
      status: 'awaiting_user',
      riskLevel: '',
      durationMs: 42,
      approvalId: 'ap1',
      approvalMode: '',
      startedAt: 0,
    };
    expect(toLiveToolView(tool).riskLevel).toBe('needs_approval');
  });

  it('toLiveToolView：error 的 code/message 透出到视图字段', () => {
    const tool: LiveToolCall = {
      id: 'live2',
      providerCallId: '',
      name: 'db_query',
      arguments: {},
      result: null,
      error: { code: 'SQL_TIMEOUT', message: 'ctx deadline' },
      status: 'failed',
      riskLevel: '',
      durationMs: 1000,
      approvalId: '',
      approvalMode: '',
      startedAt: 0,
    };
    const view = toLiveToolView(tool);
    expect(view.errorCode).toBe('SQL_TIMEOUT');
    expect(view.errorMessage).toBe('ctx deadline');
    expect(view.riskLevel).toBe('');
  });
});
