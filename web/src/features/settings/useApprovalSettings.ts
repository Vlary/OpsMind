import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { message } from '../../components/antdMessage';
import { approvalModeLabel } from '../../components/approvalMode';

// 六期 FE-P1（契约 2.11 约定 1 / 8.4）：审批门限的读写 hooks。
// - 全局：GET/PUT /api/settings（公开键 v1 仅 approval_mode）；
// - 会话覆盖：PATCH /api/conversations/:id（approval_mode null=清除覆盖）。
// 失效规则（8.4 六期追加）：PUT settings 成功 → 失效 settings 与 conversations
// （列表行 effective_approval_mode 随全局变化）；PATCH conversation 携带
// approval_mode → 失效 conversation(id) 与 conversations。门限变更不失效
// approvals（已建审批按快照展示，2.11 约定 1）。

type ApprovalMode = components['schemas']['ApprovalMode'];

/** 全局审批门限（GET /api/settings；401 由各页统一处理，这里只读不拦）。 */
export function useGlobalApprovalMode() {
  return useQuery({
    queryKey: qk.settings,
    queryFn: async () => {
      const { data, error } = await api.GET('/api/settings');
      if (error) {
        throw error;
      }
      return data;
    },
  });
}

/** 写全局门限（PUT /api/settings）。保存即时生效并 toast。 */
export function useUpdateGlobalApprovalMode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (mode: ApprovalMode) => {
      const { data, error } = await api.PUT('/api/settings', {
        params: { header: { Origin: window.location.origin } },
        body: { approval_mode: mode },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    onSuccess: (result) => {
      message.success(`全局审批门限已设为「${approvalModeLabel(result.approval_mode)}」`);
      void queryClient.invalidateQueries({ queryKey: qk.settings });
      void queryClient.invalidateQueries({ queryKey: qk.conversations });
    },
    onError: () => {
      message.error('全局审批门限保存失败，请重试。');
    },
  });
}

/** 会话覆盖门限（PATCH /api/conversations/:id；null=跟随全局）。保存即时生效并 toast。 */
export function useUpdateConversationApprovalMode(conversationId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (mode: ApprovalMode | null) => {
      if (!conversationId) {
        throw new Error('missing conversation id');
      }
      const { data, error } = await api.PATCH('/api/conversations/{id}', {
        params: {
          path: { id: conversationId },
          header: { Origin: window.location.origin },
        },
        body: { approval_mode: mode },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    onSuccess: (conversation) => {
      if (conversation.approval_mode === null) {
        message.success(
          `本会话已恢复跟随全局门限（当前：${approvalModeLabel(conversation.effective_approval_mode)}）`,
        );
      } else {
        message.success(`本会话审批门限已设为「${approvalModeLabel(conversation.approval_mode)}」`);
      }
      void queryClient.invalidateQueries({ queryKey: qk.conversation(conversationId ?? '') });
      void queryClient.invalidateQueries({ queryKey: qk.conversations });
    },
    onError: () => {
      message.error('会话门限保存失败，请重试。');
    },
  });
}
