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

export const api = createClient<paths>({ baseUrl: '/', credentials: 'same-origin' });
api.use(proxyErrorGuard);
