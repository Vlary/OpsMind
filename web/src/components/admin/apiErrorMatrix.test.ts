import { describe, expect, it, vi } from 'vitest';

import {
  ApiRequestError,
  applyFieldErrors,
  describeFailure,
  failureFromError,
  fieldErrorsFrom,
  makeFailure,
  toApiFailure,
  writeHeaders,
} from './apiError';

// 错误码 → 中文文案映射（FE2 契约 docs/CONTRACT.md 9.6）的全量矩阵：
// 每个已知码必须有中文文案；通用兜底 message 集合不拼进标题；
// 表单字段错误归一的形态组合。与 apiError.test.ts / apiErrorMore.test.ts 互补。

const KNOWN_CODES: Array<[string, string]> = [
  ['AUTH_REQUIRED', '登录已过期，请重新登录'],
  ['AUTH_INVALID', '用户名或密码错误'],
  ['AUTH_RATE_LIMITED', '登录尝试过于频繁，请稍后再试'],
  ['ORIGIN_FORBIDDEN', '请求来源校验失败'],
  ['VALIDATION_FAILED', '表单校验未通过'],
  ['NOT_FOUND', '资源不存在'],
  ['CONFLICT', '资源冲突或当前状态不允许该操作'],
  ['INTERNAL', '服务器内部错误'],
  ['SERVICE_UNAVAILABLE', '服务暂不可用，请稍后重试'],
  ['NETWORK_ERROR', '网络异常，请检查连接后重试'],
  ['NO_PROVIDER_CONFIGURED', '尚未配置模型供应商'],
  ['PROVIDER_NOT_FOUND', '供应商不存在'],
  ['PROVIDER_TEST_FAILED', '供应商连接测试失败'],
  ['PROVIDER_MODELS_FAILED', '拉取模型列表失败'],
  ['SERVER_NOT_FOUND', '服务器不存在'],
  ['SERVER_TEST_FAILED', '服务器连接测试失败'],
  ['SSH_HOST_KEY_CHANGED', '主机密钥已变化，连接被阻止'],
  ['SSH_TIMEOUT', 'SSH 连接超时'],
  ['SSH_EXEC_FAILED', '远程命令执行失败'],
  ['DATABASE_NOT_FOUND', '数据库不存在'],
  ['DATABASE_TEST_FAILED', '数据库连接测试失败'],
  ['SQL_NOT_READONLY', '仅支持只读查询，写操作不会执行'],
  ['SQL_TIMEOUT', 'SQL 执行超时'],
  ['SQL_QUERY_FAILED', 'SQL 执行失败'],
  ['AUDIT_WRITE_FAILED', '审计日志写入失败'],
  ['TOO_MANY_STREAMS', '并发对话流过多，请稍后重试'],
  ['STREAM_ACTIVE', '该会话已有进行中的回答'],
  ['LLM_UPSTREAM_ERROR', '上游模型服务错误'],
  ['LLM_TIMEOUT', '模型服务超时'],
  ['LLM_STREAM_ERROR', '模型流式响应解析失败'],
  ['COMMAND_FORBIDDEN', '该命令被风险规则禁止'],
  ['COMMAND_INVALID', '命令不合法'],
  ['COMMAND_TOO_LARGE', '命令超出大小限制'],
  ['CONVERSATION_NOT_FOUND', '会话不存在'],
  ['CONVERSATION_MODEL_LOCKED', '会话已绑定其他供应商/模型'],
  ['APPROVAL_NOT_FOUND', '审批不存在'],
  ['APPROVAL_NOT_PENDING', '审批已被处理或已过期'],
  ['APPROVAL_DENIED', '审批被拒绝，命令未执行'],
  ['APPROVAL_TIMEOUT', '审批超时，命令未执行'],
  // 六期打磨 sprint 3：审批随回答流中断被连带取消（tool_calls.error_code）。
  ['APPROVAL_CANCELLED', '回答流已中断，本轮审批已取消，命令未执行'],
  ['TOOL_NOT_FOUND', '模型调用了未知的工具'],
  ['TOOL_ARGS_INVALID', '工具参数不合法'],
  ['AGENT_MAX_ITERATIONS', '工具调用轮次达到上限'],
  ['LLM_OUTPUT_TRUNCATED', '模型输出被截断且无可用文本'],
  ['STREAM_TIMEOUT', '回答流超时'],
  ['SERVER_SHUTTING_DOWN', '服务正在关闭'],
  ['APPROVAL_STREAM_CANCELLED', '连接已断开，审批随之失效'],
  ['CLIENT_DISCONNECTED', '客户端已断开连接'],
  ['TERM_SESSION_NOT_FOUND', '终端会话不存在'],
  ['TERM_SESSION_EXISTS', '该服务器已有活动终端会话'],
  ['TERM_SESSION_LIMIT', '活动终端会话数已达上限'],
  ['TERM_CONNECT_FAILED', '终端连接建立失败'],
  ['TERM_UNSUPPORTED', '该服务器认证方式不支持终端（WinRM 无交互终端）'],
  ['TERM_WS_PROTOCOL', '终端 WebSocket 子协议缺失或不匹配'],
  ['RECORDING_NOT_FOUND', '会话没有可用的录制文件'],
  ['RECORDING_UNAVAILABLE', '录制文件缺失或不可读'],
  ['RDP_SESSION_NOT_FOUND', 'RDP 会话不存在'],
  ['RDP_SESSION_EXISTS', '该服务器已有活动 RDP 会话'],
  ['RDP_SESSION_LIMIT', '活动 RDP 会话数已达上限'],
  ['RDP_CONNECT_FAILED', 'RDP 连接建立失败'],
  ['RDP_UNSUPPORTED', '仅 Windows 服务器支持 RDP'],
  ['RDP_NO_CREDENTIALS', '该服务器没有可用的密码凭据'],
  ['GUACD_UNAVAILABLE', 'RDP 网关服务未配置或不可达'],
];

describe('describeFailure 已知错误码中文映射全量', () => {
  it.each(KNOWN_CODES)('%s → 固定中文文案（空 message）', (code, text) => {
    expect(describeFailure(makeFailure(code, ''))).toBe(text);
  });

  it('每个已知码的中文文案都不以英文码开头且非空', () => {
    for (const [code, text] of KNOWN_CODES) {
      expect(text.length).toBeGreaterThan(0);
      expect(text.startsWith(code)).toBe(false);
    }
  });
});

describe('describeFailure 通用兜底 message 集合（不拼进文案）', () => {
  const GENERIC: Array<[string, string]> = [
    ['CONFLICT', 'conflict'],
    ['NOT_FOUND', 'not found'],
    ['VALIDATION_FAILED', 'invalid argument'],
    ['INTERNAL', 'internal server error'],
    ['INTERNAL', 'request failed'],
    ['AUTH_INVALID', 'invalid credentials'],
    ['AUTH_RATE_LIMITED', 'too many login attempts'],
    ['SSH_EXEC_FAILED', 'ssh command failed'],
    // SERVER_TEST_FAILED 的 ssh/winrm 裸短语走通道化标题（二期 2.7 双通道
    // 同码、文案按通道），不等于通用映射，见下方专用用例。
    ['DATABASE_TEST_FAILED', 'database connection test failed'],
    ['PROVIDER_TEST_FAILED', 'provider request failed'],
    ['LLM_TIMEOUT', 'provider request timed out'],
    ['SQL_TIMEOUT', '查询超时'],
  ];

  it.each(GENERIC)('%s + 兜底短语「%s」：只显示中文映射', (code, message) => {
    expect(describeFailure(makeFailure(code, message))).toBe(
      KNOWN_CODES.find(([c]) => c === code)?.[1] ?? '',
    );
  });

  it('SERVER_TEST_FAILED 裸兜底短语：通道化标题（二期 2.7 双通道同码、文案按通道）', () => {
    expect(describeFailure(makeFailure('SERVER_TEST_FAILED', 'ssh connection test failed'))).toBe(
      'SSH 连接测试失败',
    );
    expect(describeFailure(makeFailure('SERVER_TEST_FAILED', 'winrm connection test failed'))).toBe(
      'WinRM 连接测试失败',
    );
  });

  it('兜底短语大小写不敏感（Not Found 与 not found 同样被过滤）', () => {
    expect(describeFailure(makeFailure('NOT_FOUND', 'Not Found'))).toBe('资源不存在');
    expect(describeFailure(makeFailure('CONFLICT', 'CONFLICT'))).toBe(
      '资源冲突或当前状态不允许该操作',
    );
  });

  it('message 首尾空白被 trim 后再判定与拼接', () => {
    expect(describeFailure(makeFailure('CONFLICT', '  conflict  '))).toBe(
      '资源冲突或当前状态不允许该操作',
    );
    expect(describeFailure(makeFailure('SSH_TIMEOUT', ' dial timeout 2s '))).toBe(
      'SSH 连接超时（dial timeout 2s）',
    );
  });
});

describe('describeFailure 拼接规则矩阵', () => {
  it('已知码 + informative message：中文（原文）', () => {
    expect(
      describeFailure(makeFailure('SSH_HOST_KEY_CHANGED', 'expected sha256 AAA got BBB')),
    ).toBe('主机密钥已变化，连接被阻止（expected sha256 AAA got BBB）');
  });

  it('未知码 + informative message：原文透传（无括号）', () => {
    expect(describeFailure(makeFailure('FUTURE_CODE', 'boom detail'))).toBe('boom detail');
  });

  it('未知码 + 空 message：统一「请求失败」', () => {
    expect(describeFailure(makeFailure('FUTURE_CODE', ''))).toBe('请求失败');
  });

  it('未知码 + 通用兜底 message：统一「请求失败」', () => {
    expect(describeFailure(makeFailure('FUTURE_CODE', 'request failed'))).toBe('请求失败');
    expect(describeFailure(makeFailure('FUTURE_CODE', 'conflict'))).toBe('请求失败');
  });
});

describe('toApiFailure 归一化补充矩阵', () => {
  function responseOf(status: number, headers: Record<string, string> = {}): Response {
    return { status, headers: new Headers(headers) } as Response;
  }

  it('error 体 code 为空串：回退 INTERNAL', () => {
    const failure = toApiFailure({ error: { code: '', message: 'm' } }, responseOf(500));
    expect(failure.code).toBe('INTERNAL');
  });

  it('error 体 code 非字符串：回退 INTERNAL', () => {
    const failure = toApiFailure({ error: { code: 42, message: 'm' } }, responseOf(500));
    expect(failure.code).toBe('INTERNAL');
  });

  it('error 体 message 为空串：回退 request failed', () => {
    const failure = toApiFailure({ error: { code: 'X', message: '' } }, responseOf(500));
    expect(failure.message).toBe('request failed');
  });

  it('error 体 request_id 为空串：回退响应头 X-Request-ID', () => {
    const failure = toApiFailure(
      { error: { code: 'X', message: 'm', request_id: '' } },
      responseOf(500, { 'X-Request-ID': 'from-header' }),
    );
    expect(failure.requestId).toBe('from-header');
  });

  it('error.error 不是对象（如字符串）：走兜底 INTERNAL 分支', () => {
    const failure = toApiFailure(new Error('plain'), responseOf(502, { 'X-Request-ID': 'r' }));
    expect(failure).toEqual({
      code: 'INTERNAL',
      message: 'plain',
      requestId: 'r',
      details: null,
      status: 502,
    });
  });

  it('error 顶层不是 record（如 null）：兜底分支保留 Error.message', () => {
    const failure = toApiFailure(new TypeError('network down'), responseOf(503));
    expect(failure.code).toBe('INTERNAL');
    expect(failure.message).toBe('network down');
    expect(failure.status).toBe(503);
  });

  it('error.error.details 为对象时透传（含嵌套）', () => {
    const failure = toApiFailure(
      { error: { code: 'VALIDATION_FAILED', message: 'm', details: { fields: { host: 'bad' } } } },
      responseOf(422),
    );
    expect(failure.details).toEqual({ fields: { host: 'bad' } });
  });

  it('response 为 null：status/requestId 均为空', () => {
    const failure = toApiFailure({ error: { code: 'X', message: 'm' } }, null);
    expect(failure.status).toBeNull();
    expect(failure.requestId).toBe('');
  });
});

describe('failureFromError 补充矩阵', () => {
  it('record 缺 message：不满足 {code,message} 形态，走 INTERNAL 兜底', () => {
    expect(failureFromError({ code: 'NOT_FOUND' })).toMatchObject({
      code: 'INTERNAL',
      message: '请求失败',
    });
  });

  it('record 的 requestId/status 非法类型回退默认值', () => {
    const failure = failureFromError({
      code: 'X',
      message: 'm',
      requestId: 123,
      details: 'not-a-record',
      status: '500',
    });
    expect(failure.requestId).toBe('');
    expect(failure.details).toBeNull();
    expect(failure.status).toBeNull();
  });

  it('非对象非 Error 输入（数字）：INTERNAL + 中文默认', () => {
    expect(failureFromError(42)).toEqual({
      code: 'INTERNAL',
      message: '请求失败',
      requestId: '',
      details: null,
      status: null,
    });
  });

  it('Error 子类实例走 Error 分支', () => {
    class CustomError extends Error {}
    expect(failureFromError(new CustomError('custom boom'))).toMatchObject({
      code: 'INTERNAL',
      message: 'custom boom',
    });
  });
});

describe('fieldErrorsFrom 字段名归一矩阵', () => {
  it('camelCase → snake_case：baseUrl → base_url', () => {
    expect(fieldErrorsFrom({ baseUrl: '格式不正确' })).toEqual({ base_url: '格式不正确' });
  });

  it('连续大写逐字符加下划线：APIKey → _a_p_i_key（文档化归一语义）', () => {
    expect(Object.keys(fieldErrorsFrom({ APIKey: 'x' }))).toEqual(['_a_p_i_key']);
  });

  it('已是 snake_case 的字段名不变', () => {
    expect(fieldErrorsFrom({ sqlite_path: '必须绝对路径' })).toEqual({
      sqlite_path: '必须绝对路径',
    });
  });

  it('数字/布尔/null 值被忽略（仅字符串、数组、带 message 对象有效）', () => {
    expect(fieldErrorsFrom({ a: 1, b: true, c: null, d: undefined })).toEqual({});
  });

  it('空数组值被忽略；全字符串数组以「；」拼接', () => {
    expect(fieldErrorsFrom({ port: [], host: ['必填', '范围 1-65535'] })).toEqual({
      host: '必填；范围 1-65535',
    });
  });

  it('数组内含非字符串项：整项忽略（不部分拼接）', () => {
    expect(fieldErrorsFrom({ host: ['必填', 42] })).toEqual({});
  });

  it('对象值带 message 字符串取 message', () => {
    expect(fieldErrorsFrom({ db: { message: '引擎不支持' } })).toEqual({ db: '引擎不支持' });
  });

  it('对象值无 message 被忽略', () => {
    expect(fieldErrorsFrom({ db: { code: 'ENGINE' } })).toEqual({});
  });

  it('fields 形态优先于顶层同名字段', () => {
    expect(fieldErrorsFrom({ fields: { host: '来自 fields' }, host: '来自顶层' })).toEqual({
      host: '来自 fields',
    });
  });

  it('形态 3 的 {field,message} 不覆盖已存在的同名字段（原始 field/message 键也进入结果，文档化）', () => {
    expect(fieldErrorsFrom({ host: '主信息', field: 'host', message: '补充信息' })).toEqual({
      host: '主信息',
      field: 'host',
      message: '补充信息',
    });
  });

  it('形态 3 与其他字段共存：field/message 原始键一并出现在结果中', () => {
    expect(fieldErrorsFrom({ port: '端口必填', field: 'host', message: '主机必填' })).toEqual({
      port: '端口必填',
      field: 'host',
      message: '主机必填',
      host: '主机必填',
    });
  });

  it('字符串值首尾空白保留（仅 trim 判空，不裁剪内容）', () => {
    expect(fieldErrorsFrom({ host: '  详细原因  ' })).toEqual({ host: '  详细原因  ' });
  });

  it('字符串值仅空白（trim 后为空）被丢弃', () => {
    expect(fieldErrorsFrom({ host: '   ' })).toEqual({});
  });
});

describe('applyFieldErrors 补充矩阵', () => {
  it('多个命中字段全部传入 setFields', () => {
    const items: { name: string; errors: string[] }[][] = [];
    const applied = applyFieldErrors(
      { name: '必填', host: '不可达', port: '超范围' },
      ['name', 'host', 'port'],
      (next) => items.push(next),
    );
    expect(applied).toBe(true);
    expect(items[0]).toEqual([
      { name: 'name', errors: ['必填'] },
      { name: 'host', errors: ['不可达'] },
      { name: 'port', errors: ['超范围'] },
    ]);
  });

  it('details 为 camelCase、fields 列表为 snake_case：归一后仍命中', () => {
    let received: { name: string }[] = [];
    const applied = applyFieldErrors({ baseUrl: '格式不正确' }, ['base_url'], (next) => {
      received = next;
    });
    expect(applied).toBe(true);
    expect(received).toEqual([{ name: 'base_url', errors: ['格式不正确'] }]);
  });

  it('部分命中：只传命中项但返回 true', () => {
    const items: { name: string; errors: string[] }[] = [];
    const applied = applyFieldErrors({ host: '必填', other: '无关' }, ['host'], (next) => {
      items.push(...next);
    });
    expect(applied).toBe(true);
    expect(items).toEqual([{ name: 'host', errors: ['必填'] }]);
  });

  it('details 为 null：返回 false 且不调用 setFields', () => {
    let called = false;
    expect(applyFieldErrors(null, ['host'], () => (called = true))).toBe(false);
    expect(called).toBe(false);
  });
});

describe('ApiRequestError 与 writeHeaders', () => {
  it('ApiRequestError 的 message 取 failure.message，name 为类名', () => {
    const error = new ApiRequestError(makeFailure('SQL_TIMEOUT', '查询超时'));
    expect(error.message).toBe('查询超时');
    expect(error.name).toBe('ApiRequestError');
    expect(error.failure.code).toBe('SQL_TIMEOUT');
  });

  it('writeHeaders 返回当前页面 Origin（写操作契约 9.6）', () => {
    vi.stubGlobal('window', { location: { origin: 'http://localhost:4173' } });
    try {
      expect(writeHeaders()).toEqual({ Origin: 'http://localhost:4173' });
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
