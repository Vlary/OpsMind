import createClient, { type Middleware } from 'openapi-fetch';
import type { paths } from './schema';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// 非 2xx 且错误体不符合 ErrorResponse 契约（api/openapi.yaml：{ error: { code,
// message, … } }）时，统一替换为 NETWORK_ERROR 标准错误体。
//
// 背景：openapi-fetch 对非 2xx 响应先 text() 再 JSON.parse（dist/index.mjs
// 末段），反向代理（开发态 vite、生产态 Nginx）在后端不可达时返回 502/503
// 纯文本或空 body，parse 失败后 error 是纯文本或空串（falsy）。调用方惯用的
// `if (error) throw error` 随之失效、data 为 undefined，TanStack Query 会把
// “queryHash data is undefined” / “Cannot read properties of undefined” 这类
// 内部技术错误直接渲染成英文横幅。规范化后走正常 error 路径，由
// toApiFailure / toApiErrorInfo 解析为 NETWORK_ERROR 的中文提示。
export async function toContractErrorResponse(response: Response): Promise<Response> {
  if (response.ok) {
    return response;
  }
  let body: unknown = null;
  try {
    const text = await response.clone().text();
    body = text === '' ? null : (JSON.parse(text) as unknown);
  } catch {
    body = null;
  }
  if (isRecord(body) && isRecord(body['error'])) {
    return response;
  }
  const statusText = response.statusText.trim();
  const detail = `HTTP ${response.status}${statusText ? ` ${statusText}` : ''}`;
  const headers = new Headers(response.headers);
  headers.set('Content-Type', 'application/json');
  return new Response(
    JSON.stringify({
      error: {
        code: 'NETWORK_ERROR',
        // describeFailure 会拼成「网络异常，请检查连接后重试（HTTP 502 …）」。
        message: detail,
        request_id: '',
        details: null,
      },
    }),
    { status: response.status, statusText: response.statusText, headers },
  );
}

const proxyErrorGuard: Middleware = {
  onResponse: ({ response }) => toContractErrorResponse(response),
};

/** 403 的统一中文提示（CONTRACT.md 12.4 第 1 条 / 9.6 FORBIDDEN）。 */
export const FORBIDDEN_MESSAGE = '无权访问';

// 已登录但缺少权限：把后端 403 FORBIDDEN（或代理层规范化出的
// NETWORK_ERROR）统一成 { code: 'FORBIDDEN', message: '无权访问' }。
//
// 只归一无信息量的通用 403；保持职责分离/来源校验等专用 403 错误码原样
// （APPROVAL_SELF_DECISION、ORIGIN_FORBIDDEN、COMMAND_FORBIDDEN、
// PASSWORD_CHANGE_REQUIRED、SELF_REDEEM_FORBIDDEN 等有各自的中文口径），
// 避免把「不能审批自己」误写成「无权访问」。
const SPECIFIC_FORBIDDEN_CODES = new Set([
  'ORIGIN_FORBIDDEN',
  'APPROVAL_SELF_DECISION',
  'APPROVAL_MODE_WIDENING_FORBIDDEN',
  'COMMAND_FORBIDDEN',
  'PASSWORD_CHANGE_REQUIRED',
  'SELF_REDEEM_FORBIDDEN',
  'TERMINAL_GRANT_KIND_MISMATCH',
  'TOOL_FORBIDDEN',
]);

export async function toForbiddenResponse(response: Response): Promise<Response> {
  if (response.status !== 403) {
    return response;
  }

  let body: unknown = null;
  try {
    const text = await response.clone().text();
    body = text === '' ? null : (JSON.parse(text) as unknown);
  } catch {
    body = null;
  }

  const error = isRecord(body) && isRecord(body['error']) ? body['error'] : null;
  const rawCode = error?.['code'];
  const code = typeof rawCode === 'string' ? rawCode : '';
  if (code !== '' && SPECIFIC_FORBIDDEN_CODES.has(code)) {
    return response;
  }

  const rawRequestId = error?.['request_id'];
  const requestId =
    (typeof rawRequestId === 'string' && rawRequestId !== ''
      ? rawRequestId
      : response.headers.get('X-Request-ID')) ?? '';
  const headers = new Headers(response.headers);
  headers.set('Content-Type', 'application/json');
  return new Response(
    JSON.stringify({
      error: {
        code: 'FORBIDDEN',
        message: FORBIDDEN_MESSAGE,
        request_id: requestId,
        details: null,
      },
    }),
    { status: 403, statusText: response.statusText, headers },
  );
}

const forbiddenGuard: Middleware = {
  onResponse: ({ response }) => toForbiddenResponse(response),
};

export const api = createClient<paths>({ baseUrl: '/', credentials: 'same-origin' });
api.use(proxyErrorGuard);
// 顺序：先把非契约错误体规范化为 NETWORK_ERROR，再统一 403。
api.use(forbiddenGuard);
