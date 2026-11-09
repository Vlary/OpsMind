import { describe, expect, it } from 'vitest';

import { makeFailure } from '../../../components/admin/apiError';
import {
  describeError,
  isUnauthorizedError,
  matchesLiveErrorTurn,
  toApiErrorInfo,
  toLiveToolView,
  toToolCallView,
  type LiveToolCall,
} from '../useChatStream';

// 聊天链路错误码 → 中文文案（useChatStream ERROR_TEXT）的全量矩阵与
// 错误形态归一的补充组合。与 useChatStream.test.ts / useChatStreamMore.test.ts 互补。

const CHAT_ERROR_CODES: Array<[string, string]> = [
  ['AUTH_REQUIRED', '登录已过期，请重新登录。'],
  ['AUTH_INVALID', '登录凭据无效。'],
  ['AUTH_RATE_LIMITED', '尝试过于频繁，请稍后再试。'],
  ['ORIGIN_FORBIDDEN', '请求来源校验失败，请通过正确入口访问。'],
  ['VALIDATION_FAILED', '请求参数不合法。'],
  ['NOT_FOUND', '资源不存在或已被删除。'],
  ['CONFLICT', '操作冲突，请刷新后重试。'],
  ['INTERNAL', '服务内部错误，请稍后重试。'],
  ['SERVICE_UNAVAILABLE', '服务暂不可用，请稍后重试。'],
  ['TOO_MANY_STREAMS', '当前回答请求过多，请稍后重试。'],
  ['STREAM_ACTIVE', '该会话已有正在进行的回答。'],
  ['CONVERSATION_NOT_FOUND', '会话不存在或已被删除。'],
  ['CONVERSATION_MODEL_LOCKED', '会话已绑定其他模型，切换模型请新建会话。'],
  ['NO_PROVIDER_CONFIGURED', '尚未配置模型供应商，请前往模型管理添加。'],
  ['PROVIDER_NOT_FOUND', '模型供应商不存在。'],
  ['PROVIDER_TEST_FAILED', '模型供应商连接测试失败。'],
  ['PROVIDER_MODELS_FAILED', '拉取模型列表失败。'],
  ['LLM_UPSTREAM_ERROR', '模型供应商返回错误。'],
  ['LLM_TIMEOUT', '模型响应超时。'],
  ['LLM_STREAM_ERROR', '模型流解析失败。'],
  ['LLM_OUTPUT_TRUNCATED', '模型输出被长度截断。'],
  ['AGENT_MAX_ITERATIONS', '工具调用次数超过上限。'],
  ['STREAM_TIMEOUT', '回答超时，已停止。'],
  ['APPROVAL_NOT_FOUND', '审批不存在或已过期。'],
  ['APPROVAL_NOT_PENDING', '审批已处理。'],
  ['APPROVAL_DENIED', '命令审批被拒绝。'],
  ['APPROVAL_TIMEOUT', '命令审批超时。'],
  // 六期打磨 sprint 3：与 APPROVAL_TIMEOUT 区分——后者是等满 TTL 的真超时。
  ['APPROVAL_CANCELLED', '回答流已中断，本轮审批已取消。'],
  ['COMMAND_FORBIDDEN', '该命令被风险规则禁止。'],
  ['COMMAND_INVALID', '命令不合法，未执行。'],
  ['TOOL_NOT_FOUND', '调用了不支持的工具。'],
  ['TOOL_ARGS_INVALID', '工具调用参数不合法。'],
  ['SERVER_NOT_FOUND', '服务器不存在或已被删除。'],
  ['DATABASE_NOT_FOUND', '数据库不存在或已被删除。'],
  ['SSH_HOST_KEY_CHANGED', '服务器主机密钥已变化，连接被阻止。'],
  ['SSH_TIMEOUT', 'SSH 命令执行超时。'],
  ['SSH_EXEC_FAILED', '远程命令执行失败。'],
  // 四期容器工具错误码（与 containerFailures.ts 的 CONTAINER_CODE_MESSAGES 对齐）。
  ['CONTAINER_UNSUPPORTED', '该服务器未探测到受支持的容器运行时（docker/podman）'],
  ['CONTAINER_NOT_FOUND', '容器不存在或已删除'],
  ['CONTAINER_CLI_FAILED', '容器 CLI 执行失败（daemon 未运行或命令执行失败）'],
  ['SQL_NOT_READONLY', '本系统只执行只读查询，写操作不会执行。'],
  ['SQL_TIMEOUT', 'SQL 查询执行超时。'],
  ['SQL_QUERY_FAILED', 'SQL 查询执行失败。'],
  ['SSE_DISCONNECTED', '连接已断开，回答可能不完整。'],
  ['NETWORK_ERROR', '网络异常，请检查连接后重试。'],
  ['SERVER_SHUTTING_DOWN', '服务正在关闭，请稍后重试。'],
  ['CLIENT_DISCONNECTED', '回答已停止。'],
];

describe('describeError 聊天链路错误码全量矩阵', () => {
  it.each(CHAT_ERROR_CODES)('%s → 固定中文文案', (code, text) => {
    expect(describeError(code)).toBe(text);
  });

  it('每个映射文案非空且不回显英文码', () => {
    for (const [code, text] of CHAT_ERROR_CODES) {
      expect(text.length).toBeGreaterThan(0);
      expect(text.includes(code)).toBe(false);
    }
  });
});

describe('describeError HTTP_ 前缀与 fallback 矩阵', () => {
  it('HTTP_ 前缀拼状态码：常见 4xx/5xx', () => {
    expect(describeError('HTTP_400')).toBe('请求失败（HTTP 400）。');
    expect(describeError('HTTP_401')).toBe('请求失败（HTTP 401）。');
    expect(describeError('HTTP_403')).toBe('请求失败（HTTP 403）。');
    expect(describeError('HTTP_404')).toBe('请求失败（HTTP 404）。');
    expect(describeError('HTTP_409')).toBe('请求失败（HTTP 409）。');
    expect(describeError('HTTP_429')).toBe('请求失败（HTTP 429）。');
    expect(describeError('HTTP_500')).toBe('请求失败（HTTP 500）。');
    expect(describeError('HTTP_502')).toBe('请求失败（HTTP 502）。');
    expect(describeError('HTTP_503')).toBe('请求失败（HTTP 503）。');
    expect(describeError('HTTP_504')).toBe('请求失败（HTTP 504）。');
  });

  it('未知码 + 非空 fallback：显示 fallback', () => {
    expect(describeError('UNKNOWN_CODE', '底层原因：x')).toBe('底层原因：x');
  });

  it('未知码 + 空 fallback：回显原码', () => {
    expect(describeError('UNKNOWN_CODE', '')).toBe('UNKNOWN_CODE');
  });

  it('未知码无 fallback：回显原码', () => {
    expect(describeError('UNKNOWN_CODE')).toBe('UNKNOWN_CODE');
  });

  it('已映射码优先于 fallback（不显示 fallback）', () => {
    expect(describeError('SQL_TIMEOUT', '自定义文案')).toBe('SQL 查询执行超时。');
  });

  it('HTTP_ 前缀优先于 fallback', () => {
    expect(describeError('HTTP_502', 'gateway text')).toBe('请求失败（HTTP 502）。');
  });
});

describe('toApiErrorInfo 补充矩阵', () => {
  it('failure 形态缺 message：中文默认文案', () => {
    const info = toApiErrorInfo({ failure: { code: 'X' } }, 500, 'r');
    expect(info).toEqual({
      code: 'X',
      message: '请求失败，请稍后重试。',
      requestId: 'r',
      status: 500,
    });
  });

  it('failure.requestId 非字符串回退入参 requestId', () => {
    const info = toApiErrorInfo({ failure: { code: 'X', message: 'm', requestId: 7 } }, 0, 'hdr');
    expect(info.requestId).toBe('hdr');
  });

  it('failure.status 非数字回退入参 status', () => {
    const info = toApiErrorInfo({ failure: { code: 'X', message: 'm', status: '502' } }, 418, '');
    expect(info.status).toBe(418);
  });

  it('error.error.code 非字符串：UNKNOWN + 中文默认', () => {
    const info = toApiErrorInfo({ error: { code: 1 } }, 500, '');
    expect(info.code).toBe('UNKNOWN');
    expect(info.message).toBe('请求失败，请稍后重试。');
  });

  it('error.error.request_id 非字符串回退入参', () => {
    const info = toApiErrorInfo({ error: { code: 'X', message: 'm', request_id: 9 } }, 0, 'r9');
    expect(info.requestId).toBe('r9');
  });

  it('顶层 {code 非字符串, message 字符串}：UNKNOWN + message', () => {
    const info = toApiErrorInfo({ code: 5, message: '只有消息' }, 0, '');
    expect(info).toEqual({ code: 'UNKNOWN', message: '只有消息', requestId: '', status: 0 });
  });

  it('数字/布尔输入走兜底：401 → AUTH_REQUIRED', () => {
    expect(toApiErrorInfo(42, 401, '').code).toBe('AUTH_REQUIRED');
    expect(toApiErrorInfo(true, 500, '').code).toBe('UNKNOWN');
  });

  it('FE2 ApiRequestError 形态：status 透传优先于入参', () => {
    const failure = { ...makeFailure('CONFLICT', 'm'), status: 409 };
    const info = toApiErrorInfo({ failure }, 500, 'ignored');
    expect(info.status).toBe(409);
    expect(info.requestId).toBe('');
  });
});

describe('isUnauthorizedError 补充矩阵', () => {
  it('error.error 深层形态识别 AUTH_REQUIRED', () => {
    expect(isUnauthorizedError({ error: { code: 'AUTH_REQUIRED' } })).toBe(true);
  });

  it('深层形态带其他字段也不影响判定', () => {
    expect(
      isUnauthorizedError({ error: { code: 'AUTH_REQUIRED', message: 'expired', extra: 1 } }),
    ).toBe(true);
  });

  it('数组、数字、布尔、undefined 一律 false', () => {
    expect(isUnauthorizedError([])).toBe(false);
    expect(isUnauthorizedError(0)).toBe(false);
    expect(isUnauthorizedError(false)).toBe(false);
    expect(isUnauthorizedError(undefined)).toBe(false);
  });

  it('AUTH_REQUIRED 字符串本身不是对象：false', () => {
    expect(isUnauthorizedError('AUTH_REQUIRED')).toBe(false);
  });

  it('failure 形态带非 AUTH_REQUIRED 码：false', () => {
    expect(isUnauthorizedError({ failure: { code: 'AUTH_INVALID' } })).toBe(false);
  });
});

describe('matchesLiveErrorTurn 补充矩阵', () => {
  it('live 与落库完全一致（长文本）', () => {
    const text = '结论：磁盘使用率 83%，建议清理 /var/log。'.repeat(5);
    expect(matchesLiveErrorTurn(text, text)).toBe(true);
  });

  it('落库是 live 的严格前缀扩展（多出的部分在后）', () => {
    expect(matchesLiveErrorTurn('回答前半后半也落库了', '回答前半')).toBe(true);
  });

  it('中间分叉的内容不匹配', () => {
    expect(matchesLiveErrorTurn('回答甲乙丙', '回答甲丙')).toBe(false);
  });

  it('live 为空且落库为空：匹配（空回答的失败轮）', () => {
    expect(matchesLiveErrorTurn('', '')).toBe(true);
  });

  it('live 非空但落库为空：不匹配', () => {
    expect(matchesLiveErrorTurn('', '部分回答')).toBe(false);
  });

  it('live 为空且落库非空：不匹配（避免误吞历史 error 消息）', () => {
    expect(matchesLiveErrorTurn('历史轮次的 error 内容', '')).toBe(false);
  });

  it('多字节字符前缀按 code unit 对齐', () => {
    expect(matchesLiveErrorTurn('检查磁盘', '检查磁')).toBe(true);
    expect(matchesLiveErrorTurn('检查网络', '检查磁')).toBe(false);
  });
});

describe('工具调用视图模型补充', () => {
  it('toToolCallView：provider_call_id 不透出到视图（视图无此字段）', () => {
    const view = toToolCallView({
      id: 'tc9',
      seq: 2,
      conversation_id: 'c1',
      turn_id: 't1',
      provider_call_id: 'pc-9',
      name: 'db_query',
      arguments: { sql: 'SELECT 1' },
      result: null,
      risk_level: 'safe',
      status: 'succeeded',
      error_code: '',
      error_message: '',
      approval_mode: '',
      started_at: '2026-01-01T00:00:00Z',
      finished_at: '2026-01-01T00:00:02Z',
    });
    expect(view.id).toBe('tc9');
    expect(view.name).toBe('db_query');
    expect(view.arguments).toEqual({ sql: 'SELECT 1' });
    expect(view.result).toBeNull();
    expect(view.durationMs).toBe(0);
    expect('providerCallId' in view).toBe(false);
  });

  it('toLiveToolView：riskLevel 已有值时优先于 result.risk_level', () => {
    const tool: LiveToolCall = {
      id: 'l1',
      providerCallId: 'p',
      name: 'ssh_exec',
      arguments: {},
      result: { risk_level: 'safe' },
      error: null,
      status: 'succeeded',
      riskLevel: 'needs_approval',
      durationMs: 10,
      approvalId: '',
      approvalMode: '',
      startedAt: 0,
    };
    expect(toLiveToolView(tool).riskLevel).toBe('needs_approval');
  });

  it('toLiveToolView：result 无 risk_level 且 riskLevel 为空 → 空串', () => {
    const tool: LiveToolCall = {
      id: 'l2',
      providerCallId: '',
      name: 'list_servers',
      arguments: {},
      result: { servers: [] },
      error: null,
      status: 'succeeded',
      riskLevel: '',
      durationMs: 3,
      approvalId: '',
      approvalMode: '',
      startedAt: 0,
    };
    expect(toLiveToolView(tool).riskLevel).toBe('');
  });

  it('toLiveToolView：durationMs 透传', () => {
    const tool: LiveToolCall = {
      id: 'l3',
      providerCallId: '',
      name: 'ssh_exec',
      arguments: {},
      result: null,
      error: null,
      status: 'failed',
      riskLevel: '',
      durationMs: 4567,
      approvalId: '',
      approvalMode: '',
      startedAt: 0,
    };
    expect(toLiveToolView(tool).durationMs).toBe(4567);
  });

  it('toLiveToolView：无 error 时 errorCode/errorMessage 为空串', () => {
    const tool: LiveToolCall = {
      id: 'l4',
      providerCallId: '',
      name: 'ssh_exec',
      arguments: {},
      result: { exit_code: 0 },
      error: null,
      status: 'succeeded',
      riskLevel: '',
      durationMs: 0,
      approvalId: '',
      approvalMode: '',
      startedAt: 0,
    };
    const view = toLiveToolView(tool);
    expect(view.errorCode).toBe('');
    expect(view.errorMessage).toBe('');
  });
});
