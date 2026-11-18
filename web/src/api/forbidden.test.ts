import { describe, expect, it } from 'vitest';

import { describeFailure } from '../components/admin/apiError';
import { FORBIDDEN_MESSAGE, toContractErrorResponse, toForbiddenResponse } from './client';

// 00012：已登录但缺少权限（403）。api 层统一归一化为
// { code: 'FORBIDDEN', message: '无权访问' }，页面不再展示后端原始
// 错误码/英文 message；专用 403（来源校验、职责分离等）保持原样。

async function bodyOf(response: Response): Promise<{
  error: { code: string; message: string; request_id: string; details: unknown };
}> {
  return (await response.json()) as {
    error: { code: string; message: string; request_id: string; details: unknown };
  };
}

describe('toForbiddenResponse：403 归一化', () => {
  it('通用 FORBIDDEN 改写为「无权访问」并保留 request_id', async () => {
    const original = new Response(
      JSON.stringify({
        error: {
          code: 'FORBIDDEN',
          message: 'missing permission users:read',
          request_id: 'req-403',
          details: { permission: 'users:read' },
        },
      }),
      { status: 403 },
    );
    const next = await toForbiddenResponse(original);
    expect(next).not.toBe(original);
    expect(next.status).toBe(403);
    expect(next.headers.get('Content-Type')).toContain('application/json');
    const body = await bodyOf(next);
    expect(body.error.code).toBe('FORBIDDEN');
    expect(body.error.message).toBe(FORBIDDEN_MESSAGE);
    expect(body.error.message).toBe('无权访问');
    expect(body.error.request_id).toBe('req-403');
    expect(body.error.details).toBeNull();
  });

  it('代理规范化后的 403（NETWORK_ERROR 体）也归一化为「无权访问」', async () => {
    const proxy = new Response('forbidden', { status: 403 });
    const contract = await toContractErrorResponse(proxy);
    const next = await toForbiddenResponse(contract);
    const body = await bodyOf(next);
    expect(body.error.code).toBe('FORBIDDEN');
    expect(body.error.message).toBe('无权访问');
  });

  it('空 body 403 用 X-Request-ID 头兜底', async () => {
    const original = new Response('', {
      status: 403,
      headers: { 'X-Request-ID': 'req-header' },
    });
    const body = await bodyOf(await toForbiddenResponse(original));
    expect(body.error.code).toBe('FORBIDDEN');
    expect(body.error.request_id).toBe('req-header');
  });

  it('专用 403 错误码原样透传（同对象）', async () => {
    for (const code of [
      'ORIGIN_FORBIDDEN',
      'APPROVAL_SELF_DECISION',
      'COMMAND_FORBIDDEN',
      'PASSWORD_CHANGE_REQUIRED',
      'SELF_REDEEM_FORBIDDEN',
    ]) {
      const original = new Response(
        JSON.stringify({ error: { code, message: 'specific', request_id: 'r' } }),
        { status: 403 },
      );
      expect(await toForbiddenResponse(original), code).toBe(original);
    }
  });

  it('非 403 响应不做处理（401/200 原样返回）', async () => {
    const unauthorized = new Response(
      JSON.stringify({ error: { code: 'FORBIDDEN', message: 'x', request_id: '' } }),
      { status: 401 },
    );
    expect(await toForbiddenResponse(unauthorized)).toBe(unauthorized);

    const ok = new Response('{"items":[]}', { status: 200 });
    expect(await toForbiddenResponse(ok)).toBe(ok);
  });

  it('归一化后的失败经 describeFailure 得到中文文案，不是原始错误码', async () => {
    const original = new Response(
      JSON.stringify({
        error: { code: 'FORBIDDEN', message: 'permission denied', request_id: '' },
      }),
      { status: 403 },
    );
    const body = await bodyOf(await toForbiddenResponse(original));
    const text = describeFailure({
      code: body.error.code,
      message: body.error.message,
      requestId: body.error.request_id,
      details: null,
      status: 403,
    });
    expect(text).toBe('无权访问');
    expect(text).not.toContain('FORBIDDEN');
    expect(text).not.toContain('permission denied');
  });
});
