// p4 sprint 4：全局查询重试策略单测。
// - 谓词逻辑：4xx/5xx 契约错误（ApiRequestError 带 status）不重试；无响应
//   失败（fetch TypeError / 无状态码）保留一次重试、第二次不再重试。
// - 行为集成：与生产同谓词的 QueryClient.fetchQuery，4xx 错误 queryFn 仅
//   执行一次（不重试）；网络瞬断错误执行两次（初查 + 一次重试）。
// - 全局 queryClient 实际挂载该谓词（防回归回 retry:1）。

import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';

import { ApiRequestError, makeFailure } from '../components/admin/apiError';
import { queryClient, shouldRetryQuery } from './queryClient';

function apiErrorWithStatus(status: number): ApiRequestError {
  return new ApiRequestError({ ...makeFailure('CONTAINER_UNSUPPORTED', 'unsupported'), status });
}

describe('shouldRetryQuery 谓词', () => {
  it('带 HTTP 状态码的 4xx/5xx 契约错误不重试（含代理规范化的 NETWORK_ERROR）', () => {
    for (const status of [400, 401, 403, 404, 409, 422, 500, 502, 503, 504]) {
      expect(shouldRetryQuery(0, apiErrorWithStatus(status)), `status=${status}`).toBe(false);
    }
  });

  it('无响应失败（fetch TypeError，网络瞬断）保留一次重试', () => {
    const networkError = new TypeError('Failed to fetch');
    expect(shouldRetryQuery(0, networkError)).toBe(true);
  });

  it('无状态码的 ApiRequestError 同样保留一次重试（安全默认）', () => {
    expect(shouldRetryQuery(0, new ApiRequestError(makeFailure('INTERNAL', 'x')))).toBe(true);
  });

  it('重试上限仍为一次：第二次失败后不再重试', () => {
    expect(shouldRetryQuery(1, new TypeError('Failed to fetch'))).toBe(false);
    expect(shouldRetryQuery(1, apiErrorWithStatus(503))).toBe(false);
    expect(shouldRetryQuery(2, new TypeError('Failed to fetch'))).toBe(false);
  });
});

describe('QueryClient 行为集成（生产同谓词）', () => {
  function makeClient(): QueryClient {
    return new QueryClient({
      defaultOptions: {
        // retryDelay 归零：跳过默认指数退避（首次重试 1s），仅验证重试次数。
        queries: { retry: shouldRetryQuery, retryDelay: () => 0 },
      },
    });
  }

  it('4xx 业务错误 queryFn 只执行一次（不重试）', async () => {
    const client = makeClient();
    const fetcher = vi.fn(async () => {
      throw apiErrorWithStatus(400);
    });
    await expect(
      client.fetchQuery({ queryKey: ['retry-test-4xx'], queryFn: fetcher }),
    ).rejects.toBeInstanceOf(ApiRequestError);
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('5xx 业务错误同样只执行一次（SSH 慢超时 504 不再双倍等待）', async () => {
    const client = makeClient();
    const fetcher = vi.fn(async () => {
      throw apiErrorWithStatus(504);
    });
    await expect(
      client.fetchQuery({ queryKey: ['retry-test-5xx'], queryFn: fetcher }),
    ).rejects.toBeInstanceOf(ApiRequestError);
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('网络瞬断错误初查 + 一次重试（共两次）后失败', async () => {
    const client = makeClient();
    const fetcher = vi.fn(async () => {
      throw new TypeError('Failed to fetch');
    });
    await expect(
      client.fetchQuery({ queryKey: ['retry-test-network'], queryFn: fetcher }),
    ).rejects.toThrow('Failed to fetch');
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});

it('全局 queryClient 默认查询重试策略即 shouldRetryQuery（非盲目 retry:1）', () => {
  expect(queryClient.getDefaultOptions().queries?.retry).toBe(shouldRetryQuery);
});
