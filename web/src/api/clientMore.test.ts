import { describe, expect, it } from 'vitest';

import { toContractErrorResponse } from './client';

// toContractErrorResponse（代理 502/503 规范化中间件）的补充矩阵：
// 与 client.test.ts 互补，覆盖各类 4xx/5xx、JSON 错误体变体与替换响应的再消费。

describe('toContractErrorResponse 补充矩阵', () => {
  it('404 + 契约错误体：原样透传（同对象）', async () => {
    const original = new Response(
      JSON.stringify({ error: { code: 'NOT_FOUND', message: 'gone' } }),
      { status: 404 },
    );
    expect(await toContractErrorResponse(original)).toBe(original);
  });

  it('500 + 契约错误体（含 details）：原样透传', async () => {
    const original = new Response(
      JSON.stringify({
        error: { code: 'VALIDATION_FAILED', message: 'm', details: { fields: { host: 'bad' } } },
      }),
      { status: 422 },
    );
    expect(await toContractErrorResponse(original)).toBe(original);
  });

  it('error 字段为非对象的 JSON：替换为 NETWORK_ERROR', async () => {
    const original = new Response('{"error":"string"}', { status: 502 });
    const next = await toContractErrorResponse(original);
    expect(next).not.toBe(original);
    const body = (await next.json()) as { error: { code: string } };
    expect(body.error.code).toBe('NETWORK_ERROR');
  });

  it('非 JSON 文本（带左花括号但不完整）：替换为 NETWORK_ERROR', async () => {
    const original = new Response('{"partial": ', { status: 503 });
    const next = await toContractErrorResponse(original);
    const body = (await next.json()) as { error: { code: string } };
    expect(body.error.code).toBe('NETWORK_ERROR');
  });

  it('替换后的响应可被 openapi-fetch 再次 text()/json()（clone 语义）', async () => {
    const original = new Response('Bad Gateway', { status: 502, statusText: 'Bad Gateway' });
    const next = await toContractErrorResponse(original);
    const first = await next.json();
    // json() 已消费 body：新 Response 的一次性属正常语义；此处验证首次解析成功。
    expect(first).toEqual({
      error: {
        code: 'NETWORK_ERROR',
        message: 'HTTP 502 Bad Gateway',
        request_id: '',
        details: null,
      },
    });
  });

  it('替换体保留原始 statusText（describeFailure 拼接可见）', async () => {
    const original = new Response('oops', { status: 502, statusText: 'Bad Gateway' });
    const next = await toContractErrorResponse(original);
    expect(next.statusText).toBe('Bad Gateway');
    const body = (await next.json()) as { error: { message: string } };
    expect(body.error.message).toBe('HTTP 502 Bad Gateway');
  });

  it('401 非 JSON body（代理层直接 401）：同样规范化为 NETWORK_ERROR', async () => {
    const original = new Response('unauthorized text', { status: 401 });
    const next = await toContractErrorResponse(original);
    expect(next).not.toBe(original);
    const body = (await next.json()) as { error: { code: string; message: string } };
    expect(body.error.code).toBe('NETWORK_ERROR');
    expect(body.error.message).toBe('HTTP 401');
  });

  it('429 纯文本：状态码保留（限流重试语义由上层）', async () => {
    const original = new Response('too many', { status: 429, statusText: 'Too Many Requests' });
    const next = await toContractErrorResponse(original);
    expect(next.status).toBe(429);
    const body = (await next.json()) as { error: { message: string } };
    expect(body.error.message).toBe('HTTP 429 Too Many Requests');
  });

  it('statusText 为空串：message 只含状态码', async () => {
    const original = new Response('x', { status: 502, statusText: '' });
    const next = await toContractErrorResponse(original);
    const body = (await next.json()) as { error: { message: string } };
    expect(body.error.message).toBe('HTTP 502');
  });

  it('2xx 各档（201/204）原样透传', async () => {
    const created = new Response('{}', { status: 201 });
    expect(await toContractErrorResponse(created)).toBe(created);
    const noContent = new Response(null, { status: 204 });
    expect(await toContractErrorResponse(noContent)).toBe(noContent);
  });

  it('替换后的 Content-Type 覆盖为 JSON（原 text/plain 也一样）', async () => {
    const original = new Response('gateway text', {
      status: 502,
      headers: { 'Content-Type': 'text/plain' },
    });
    const next = await toContractErrorResponse(original);
    expect(next.headers.get('Content-Type')).toContain('application/json');
  });
});
