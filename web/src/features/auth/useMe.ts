// 当前登录用户 + 权限（`GET /api/auth/me`，CONTRACT.md 12.4）。
//
// qk.me() 缓存的是完整 MeResponse（user + permissions）。所有消费方
// （RequireAuth / AppShell / 权限门 / 登录页）共用同一 queryFn，避免
// 各处 `if (error) throw error` 的写法漂移，也保证缓存里始终带权限。

import { useQuery } from '@tanstack/react-query';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { ApiRequestError, makeFailure, toApiFailure } from '../../components/admin/apiError';
import type { Permission } from '../../lib/rbacPermissions';

export type CurrentUser = components['schemas']['User'];
export type MeResponse = components['schemas']['MeResponse'];

const NO_PERMISSIONS: Permission[] = [];

export async function fetchMe(): Promise<MeResponse> {
  const { data, error, response } = await api.GET('/api/auth/me');
  if (error) {
    throw new ApiRequestError(toApiFailure(error, response));
  }
  if (!data) {
    throw new ApiRequestError(makeFailure('INTERNAL', 'empty me response'));
  }
  return data;
}

export function useMe() {
  return useQuery({
    queryKey: qk.me(),
    queryFn: fetchMe,
    // 401 由 queryClient 统一跳登录；认证门不重试。
    retry: false,
  });
}

/** 当前用户权限；未加载完成时返回空集合（导航为空，不会误放行）。 */
export function usePermissions(): readonly Permission[] {
  const meQuery = useMe();
  return meQuery.data?.permissions ?? NO_PERMISSIONS;
}
