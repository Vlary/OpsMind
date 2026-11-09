import { describe, expect, it } from 'vitest';

import { toContractErrorResponse } from './client';

// 第 5 轮打磨：后端断连/网络闪断时，vite 代理回 502 纯文本或空 body，
// openapi-fetch 的 error 变成 falsy（空串）或纯文本，queryFn 的
// `if (error) throw error` 失效、data 为 undefined，页面把 TanStack Query
// 的 “data is undefined” 等内部错误渲染成英文横幅。中间件必须把不符合
// ErrorResponse 契约的非 2xx 错误体规范化为 NETWORK_ERROR。

describe('toContractErrorResponse', () => {
  it('非 2xx 纯文本 body（代理 502）替换为 NETWORK_ERROR 标准错误体，保留状态码', async () => {
    const original = new Response('Error occurred while trying to proxy', {
      status: 502,
      statusText: 'Bad Gateway',
    });
    const next = await toContractErrorResponse(original);
    expect(next).not.toBe(original);
    expect(next.status).toBe(502);
    expect(next.headers.get('Content-Type')).toContain('application/json');
    const body = (await next.json()) as {
      error: { code: string; message: string; request_id: string };
    };
    expect(body.error.code).toBe('NETWORK_ERROR');
    expect(body.error.message).toBe('HTTP 502 Bad Gateway');
  });

  it('非 2xx 空 body 替换为 NETWORK_ERROR，message 只含状态码', async () => {
    const original = new Response('', { status: 503, statusText: '' });
    const next = await toContractErrorResponse(original);
    const body = (await next.json()) as { error: { code: string; message: string } };
    expect(body.error.code).toBe('NETWORK_ERROR');
    expect(body.error.message).toBe('HTTP 503');
  });

  it('非 2xx JSON 但缺少 error 字段也替换（例如网关回的 JSON 健康信息）', async () => {
    const original = new Response('{"status":"unhealthy"}', { status: 502 });
    const next = await toContractErrorResponse(original);
    const body = (await next.json()) as { error: { code: string } };
    expect(body.error.code).toBe('NETWORK_ERROR');
  });

  it('符合 ErrorResponse 契约的错误体原样透传（同对象，body 未消费）', async () => {
    const original = new Response(
      JSON.stringify({ error: { code: 'AUTH_INVALID', message: 'invalid credentials' } }),
      { status: 401 },
    );
    const next = await toContractErrorResponse(original);
    expect(next).toBe(original);
  });

  it('2xx 响应原样透传（成功体不是 error 契也不能被改写）', async () => {
    const original = new Response(JSON.stringify({ items: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
    const next = await toContractErrorResponse(original);
    expect(next).toBe(original);
  });

  it('原响应头（如 X-Request-ID）在替换后的响应上保留', async () => {
    const original = new Response('bad gateway text', {
      status: 502,
      headers: { 'X-Request-ID': 'req-123' },
    });
    const next = await toContractErrorResponse(original);
    expect(next.headers.get('X-Request-ID')).toBe('req-123');
  });
});
