// M4 五期：通知渠道列表 hook（规则表单/规则表/渠道 Tab 共用，
// 独立文件避免 react-refresh/only-export-components）。

import { useQuery } from '@tanstack/react-query';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';

type NotificationChannel = components['schemas']['NotificationChannel'];

export function useNotificationChannels() {
  return useQuery({
    queryKey: qk.notificationChannels,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/notification-channels');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });
}

export type { NotificationChannel };
