import { describe, expect, it } from 'vitest';

import {
  ApiRequestError,
  applyFieldErrors,
  describeFailure,
  failureFromError,
  fieldErrorsFrom,
  makeFailure,
  toApiFailure,
} from './apiError';

// 错误码中文映射与表单字段错误归一（FE2 契约）：覆盖 describeFailure 的
// 拼接规则、toApiFailure 的响应归一化、fieldErrorsFrom 的三种后端形态。

describe('describeFailure 错误码中文映射', () => {
  it('已知码 + 有信息量 message 拼接为「中文（原文）」', () => {
    expect(describeFailure(makeFailure('SSH_TIMEOUT', 'dial tcp 10.0.0.1:22: timeout'))).toBe(
      'SSH 连接超时（dial tcp 10.0.0.1:22: timeout）',
    );
  });

  it('已知码 + 通用兜底 message 不拼接（避免“资源冲突…（conflict）”）', () => {
    expect(describeFailure(makeFailure('CONFLICT', 'conflict'))).toBe(
      '资源冲突或当前状态不允许该操作',
    );
    expect(describeFailure(makeFailure('NOT_FOUND', 'Not Found'))).toBe('资源不存在');
    expect(describeFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '用户名或密码错误',
    );
  });

  it('已知码 + 空 message 只显示中文文案', () => {
    expect(describeFailure(makeFailure('SQL_NOT_READONLY', '   '))).toBe(
      '仅支持只读查询，写操作不会执行',
    );
  });

  it('未知码：有信息量 message 透传原文', () => {
    expect(describeFailure(makeFailure('SOME_FUTURE_CODE', 'specific reason here'))).toBe(
      'specific reason here',
    );
  });

  it('未知码：无信息量时统一“请求失败”', () => {
    expect(describeFailure(makeFailure('SOME_FUTURE_CODE', ''))).toBe('请求失败');
    expect(describeFailure(makeFailure('SOME_FUTURE_CODE', 'request failed'))).toBe('请求失败');
  });

  it('连接测试失败类兜底短语不拼进文案（后端未带原因时）', () => {
    expect(describeFailure(makeFailure('SERVER_TEST_FAILED', 'ssh connection test failed'))).toBe(
      'SSH 连接测试失败',
    );
    expect(
      describeFailure(makeFailure('DATABASE_TEST_FAILED', 'database connection test failed')),
    ).toBe('数据库连接测试失败');
  });

  it('供应商测试失败类兜底短语不拼进文案（后端未带原因时）', () => {
    expect(describeFailure(makeFailure('PROVIDER_TEST_FAILED', 'provider request failed'))).toBe(
      '供应商连接测试失败',
    );
    expect(describeFailure(makeFailure('LLM_TIMEOUT', 'provider request timed out'))).toBe(
      '模型服务超时',
    );
  });

  it('供应商测试失败带底层原因时拼进文案（连接被拒可见）', () => {
    const message =
      'provider request failed: network error: Get "http://127.0.0.1:1/v1/models": dial tcp 127.0.0.1:1: connect: connection refused';
    expect(describeFailure(makeFailure('PROVIDER_TEST_FAILED', message))).toBe(
      `供应商连接测试失败（${message}）`,
    );
  });

  it('SQL 查询失败透出底层原因（round 3：驱动错误可见）', () => {
    expect(
      describeFailure(makeFailure('SQL_QUERY_FAILED', '查询失败：no such table: orders')),
    ).toBe('SQL 执行失败（查询失败：no such table: orders）');
  });

  it('SQL 只读守卫拒绝带规则标识时拼进文案', () => {
    expect(
      describeFailure(makeFailure('SQL_NOT_READONLY', '查询被只读守卫拒绝：SQL_GUARD_DML')),
    ).toBe('仅支持只读查询，写操作不会执行（查询被只读守卫拒绝：SQL_GUARD_DML）');
  });

  it('SQL 超时固定中文 message 与标题同义，不重复拼接', () => {
    expect(describeFailure(makeFailure('SQL_TIMEOUT', '查询超时'))).toBe('SQL 执行超时');
  });
});

describe('toApiFailure 响应归一化', () => {
  function fakeResponse(status: number, requestId?: string): Response {
    return {
      status,
      headers: new Headers(requestId ? { 'X-Request-ID': requestId } : {}),
    } as Response;
  }

  it('标准错误体 {error:{code,message,request_id,details}} 全字段透传', () => {
    const failure = toApiFailure(
      {
        error: {
          code: 'VALIDATION_FAILED',
          message: 'invalid field',
          request_id: 'req-1',
          details: { fields: { host: 'required' } },
        },
      },
      fakeResponse(422, 'req-header'),
    );
    expect(failure).toEqual({
      code: 'VALIDATION_FAILED',
      message: 'invalid field',
      requestId: 'req-1',
      details: { fields: { host: 'required' } },
      status: 422,
    });
  });

  it('体缺 request_id 时回退响应头 X-Request-ID', () => {
    const failure = toApiFailure(
      { error: { code: 'INTERNAL', message: 'boom' } },
      fakeResponse(500, 'req-from-header'),
    );
    expect(failure.requestId).toBe('req-from-header');
  });

  it('体缺 code/message 时补 INTERNAL / request failed', () => {
    const failure = toApiFailure({ error: {} }, fakeResponse(500));
    expect(failure.code).toBe('INTERNAL');
    expect(failure.message).toBe('request failed');
  });

  it('details 非对象时归一为 null', () => {
    const failure = toApiFailure(
      { error: { code: 'INTERNAL', message: 'x', details: ['nope'] } },
      null,
    );
    expect(failure.details).toBeNull();
  });

  it('非契约形态（如 502 纯文本经中间件转换前）回退 INTERNAL + Error.message', () => {
    const failure = toApiFailure(new TypeError('fetch failed'), fakeResponse(502));
    expect(failure).toEqual({
      code: 'INTERNAL',
      message: 'fetch failed',
      requestId: '',
      details: null,
      status: 502,
    });
  });

  it('无 response 时 status 为 null', () => {
    const failure = toApiFailure({ error: { code: 'CONFLICT', message: 'm' } }, null);
    expect(failure.status).toBeNull();
  });
});

describe('failureFromError 异常归一', () => {
  it('ApiRequestError 直接取出 failure', () => {
    const failure = makeFailure('CONFLICT', 'server name already exists');
    const error = new ApiRequestError({ ...failure, status: 409 });
    expect(failureFromError(error)).toEqual({ ...failure, status: 409 });
  });

  it('已具备 code/message 的 record 直接转 failure', () => {
    expect(
      failureFromError({ code: 'NOT_FOUND', message: 'gone', requestId: 'r', status: 404 }),
    ).toEqual({ code: 'NOT_FOUND', message: 'gone', requestId: 'r', details: null, status: 404 });
  });

  it('普通 Error 转为 INTERNAL + 中文默认 message', () => {
    expect(failureFromError(new Error('whatever'))).toMatchObject({
      code: 'INTERNAL',
      message: 'whatever',
    });
    expect(failureFromError('裸字符串')).toMatchObject({
      code: 'INTERNAL',
      message: '请求失败',
    });
  });
});

describe('fieldErrorsFrom 三种 details 形态', () => {
  it('形态 1：{fields:{...}} 且 camelCase 字段名归一为 snake_case', () => {
    expect(fieldErrorsFrom({ fields: { baseUrl: '格式不正确', name: '必填' } })).toEqual({
      base_url: '格式不正确',
      name: '必填',
    });
  });

  it('形态 2：details 本身即字段映射', () => {
    expect(fieldErrorsFrom({ host: '不可达', port: '超出范围' })).toEqual({
      host: '不可达',
      port: '超出范围',
    });
  });

  it('形态 3：{field, message} 单字段补充为对应表单字段', () => {
    const errors = fieldErrorsFrom({ field: 'sqlitePath', message: '必须绝对路径' });
    expect(errors['sqlite_path']).toBe('必须绝对路径');
  });

  it('数组错误信息以「；」拼接；含非字符串项时整项忽略', () => {
    expect(fieldErrorsFrom({ host: ['必填', '格式错误'] })).toEqual({
      host: '必填；格式错误',
    });
    expect(fieldErrorsFrom({ host: ['必填', 42] })).toEqual({});
  });

  it('对象值取 message 字段；空字符串/空数组项被丢弃', () => {
    expect(fieldErrorsFrom({ db: { message: '引擎不支持' }, host: '', port: [] })).toEqual({
      db: '引擎不支持',
    });
  });

  it('details 为 null 返回空映射', () => {
    expect(fieldErrorsFrom(null)).toEqual({});
  });
});

describe('applyFieldErrors 表单应用', () => {
  it('命中表单字段时 setFields 收到 name + errors 并返回 true', () => {
    const items: { name: string; errors: string[] }[] = [];
    const applied = applyFieldErrors({ host: '必填' }, ['name', 'host'], (next) => {
      items.push(...next);
    });
    expect(applied).toBe(true);
    expect(items).toEqual([{ name: 'host', errors: ['必填'] }]);
  });

  it('不相关字段（如 server_id）不进表单并返回 false', () => {
    let called = false;
    const applied = applyFieldErrors({ owner: '无此字段' }, ['name', 'host'], () => {
      called = true;
    });
    expect(applied).toBe(false);
    expect(called).toBe(false);
  });
});
