import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query';

import { ApiRequestError } from '../components/admin/apiError';

// 401 统一处理：任何经过 TanStack Query 的请求返回 AUTH_REQUIRED 时，
// 清空 me 并跳转登录页（CONTRACT.md 8.4）。放在 query cache 层，避免每个
// 资源页各自遗漏。
function errorCode(error: unknown): string {
  if (typeof error !== 'object' || error === null) {
    return '';
  }
  const record = error as Record<string, unknown>;
  const body = record['error'];
  if (typeof body === 'object' && body !== null) {
    const code = (body as Record<string, unknown>)['code'];
    if (typeof code === 'string') {
      return code;
    }
  }
  const failure = record['failure'];
  if (typeof failure === 'object' && failure !== null) {
    const code = (failure as Record<string, unknown>)['code'];
    if (typeof code === 'string') {
      return code;
    }
  }
  return typeof record['code'] === 'string' ? record['code'] : '';
}

function redirectToLogin(): void {
  if (typeof window === 'undefined' || window.location.pathname.startsWith('/login')) {
    return;
  }
  const next = `${window.location.pathname}${window.location.search}`;
  window.location.assign(`/login?next=${encodeURIComponent(next)}`);
}

function handleAuthError(error: unknown): void {
  if (errorCode(error) === 'AUTH_REQUIRED') {
    redirectToLogin();
  }
}

// p4 sprint 4：带 HTTP 状态码的失败（4xx/5xx 契约错误体，经
// ApiRequestError 携带 status——含代理 502/503 规范化来的 NETWORK_ERROR）
// 重试必然复现：SSH 慢链路下容器接口一次探测超时已达 15s，盲目重试让
// 错误横幅延迟双倍（walk3 实测成对的 400 console 记录）。仅对「无响应
// 的失败」——fetch 抛出的 TypeError（网络瞬断/连接被重置，无从谈状态
// 码，非 ApiRequestError）——保留一次重试。
export function shouldRetryQuery(failureCount: number, error: unknown): boolean {
  if (failureCount >= 1) {
    return false;
  }
  if (error instanceof ApiRequestError) {
    const status = error.failure.status;
    if (status !== null && status >= 400) {
      return false;
    }
  }
  return true;
}

export const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: handleAuthError }),
  mutationCache: new MutationCache({ onError: handleAuthError }),
  defaultOptions: {
    queries: {
      retry: shouldRetryQuery,
      staleTime: 15_000,
      refetchOnWindowFocus: false,
    },
  },
});
