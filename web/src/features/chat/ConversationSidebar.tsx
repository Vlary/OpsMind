import {
  DeleteOutlined,
  EditOutlined,
  MoreOutlined,
  PlusOutlined,
  SearchOutlined,
} from '@ant-design/icons';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Dropdown, Input, Modal, Spin, Typography } from 'antd';
import { memo, useCallback, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import ConfirmDeleteModal from '../../components/ConfirmDeleteModal';
import EmptyState from '../../components/EmptyState';
import { describeError, toApiErrorInfo } from './useChatStream';

type Conversation = components['schemas']['Conversation'];

// 会话列表游标（后端 ConversationList.next_cursor 的非空形态）。
type ConversationCursor = {
  last_message_at: string;
  id: string;
};

function errorText(error: unknown): string {
  const info = toApiErrorInfo(error);
  const base = describeError(info.code, info.message);
  return info.requestId ? `${base}（request_id: ${info.requestId}）` : base;
}

export interface ConversationSidebarProps {
  activeId: string | undefined;
  onNavigate: (id: string) => void;
  onNew: () => void;
  onDeleted: (id: string) => void;
  creating: boolean;
}

function formatTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return '';
  }
  const minutes = (Date.now() - date.getTime()) / 60000;
  if (minutes < 1) {
    return '刚刚';
  }
  if (minutes < 60) {
    return `${Math.floor(minutes)} 分钟前`;
  }
  if (minutes < 24 * 60) {
    return `${Math.floor(minutes / 60)} 小时前`;
  }
  return date.toLocaleDateString('zh-CN');
}

function ConversationSidebar({
  activeId,
  onNavigate,
  onNew,
  onDeleted,
  creating,
}: ConversationSidebarProps) {
  const queryClient = useQueryClient();
  const [keyword, setKeyword] = useState('');
  const [renameTarget, setRenameTarget] = useState<Conversation | null>(null);
  const [renameValue, setRenameValue] = useState('');
  const [deleteTarget, setDeleteTarget] = useState<Conversation | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  // 游标分页：后端按 last_message_at DESC, id DESC 返回，首页 limit=200
  // （兼容旧契约首屏），has_more 时通过「加载更早的会话」继续向历史翻页，
  // 避免超过 200 个后旧会话在 UI 上不可达。
  const conversationsQuery = useInfiniteQuery({
    queryKey: qk.conversations,
    initialPageParam: undefined as ConversationCursor | undefined,
    queryFn: async ({ pageParam }) => {
      const query: { limit: number; before_last_message_at?: string; before_id?: string } = {
        limit: 200,
      };
      if (pageParam) {
        query.before_last_message_at = pageParam.last_message_at;
        query.before_id = pageParam.id;
      }
      const { data, error } = await api.GET('/api/conversations', { params: { query } });
      if (error) {
        throw error;
      }
      return data;
    },
    getNextPageParam: (lastPage) =>
      lastPage.has_more && lastPage.next_cursor ? lastPage.next_cursor : undefined,
  });

  const renameMutation = useMutation({
    mutationFn: async (input: { id: string; title: string }) => {
      const { data, error } = await api.PATCH('/api/conversations/{id}', {
        params: {
          path: { id: input.id },
          header: { Origin: window.location.origin },
        },
        body: { title: input.title },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    onSuccess: (conversation) => {
      setActionError(null);
      setRenameTarget(null);
      void queryClient.invalidateQueries({ queryKey: qk.conversations });
      void queryClient.invalidateQueries({ queryKey: qk.conversation(conversation.id) });
    },
    onError: (error) => {
      setActionError(errorText(error));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (id: string) => {
      const { error } = await api.DELETE('/api/conversations/{id}', {
        params: {
          path: { id },
          header: { Origin: window.location.origin },
        },
      });
      if (error) {
        throw error;
      }
      return id;
    },
    onSuccess: (id) => {
      setActionError(null);
      setDeleteTarget(null);
      queryClient.removeQueries({ queryKey: qk.conversation(id) });
      queryClient.removeQueries({ queryKey: qk.messages(id) });
      queryClient.removeQueries({ queryKey: qk.toolCalls(id) });
      queryClient.removeQueries({ queryKey: qk.approvals(id) });
      void queryClient.invalidateQueries({ queryKey: qk.conversations });
      onDeleted(id);
    },
    onError: (error) => {
      setActionError(errorText(error));
      // 409 STREAM_ACTIVE 等失败时也要关闭确认弹窗（与 ServersPage 对齐）：
      // 遮罩不撤会挡住错误 Alert 与整页交互。
      setDeleteTarget(null);
    },
  });

  const openRename = useCallback((conversation: Conversation) => {
    setRenameTarget(conversation);
    setRenameValue(conversation.title);
  }, []);

  const submitRename = useCallback(() => {
    if (!renameTarget) {
      return;
    }
    const title = renameValue.trim();
    if (!title) {
      setActionError('会话标题不能为空。');
      return;
    }
    renameMutation.mutate({ id: renameTarget.id, title });
  }, [renameMutation, renameTarget, renameValue]);

  const conversations = conversationsQuery.data?.pages.flatMap((page) => page.items) ?? [];
  // 会话列表按标题前端过滤当前已加载页（单管理员量级无需后端支持）；
  // 会话超过一屏后不再需要翻列表肉眼找。
  const normalizedKeyword = keyword.trim().toLowerCase();
  const visibleConversations = normalizedKeyword
    ? conversations.filter((conversation) =>
        conversation.title.toLowerCase().includes(normalizedKeyword),
      )
    : conversations;
  const conversationsError = conversationsQuery.error
    ? toApiErrorInfo(conversationsQuery.error)
    : null;
  const hasMoreConversations = conversationsQuery.hasNextPage === true;
  // 搜索无匹配且仍有未加载页：空态改为「继续向历史翻页」的联动入口，
  // 底部的常规翻页按钮此时隐藏（避免同屏两个「加载更早的会话」）。
  const searchEmptyWithMore =
    normalizedKeyword !== '' && visibleConversations.length === 0 && hasMoreConversations;

  return (
    <aside className="chat-sidebar">
      <div className="chat-sidebar-header">
        <Typography.Text strong>会话</Typography.Text>
        <Button
          type="text"
          size="small"
          icon={<PlusOutlined />}
          loading={creating}
          onClick={onNew}
          title="新建会话"
          aria-label="新建会话"
        />
      </div>

      {actionError ? (
        <Alert
          className="chat-sidebar-error"
          type="error"
          showIcon
          closable
          onClose={() => setActionError(null)}
          message={actionError}
        />
      ) : null}

      {conversationsQuery.isSuccess && conversations.length > 0 ? (
        <div className="chat-sidebar-search">
          <Input
            allowClear
            size="small"
            prefix={<SearchOutlined />}
            placeholder="搜索会话标题"
            aria-label="搜索会话"
            value={keyword}
            onChange={(event) => setKeyword(event.target.value)}
          />
        </div>
      ) : null}

      <div className="chat-sidebar-list">
        {conversationsQuery.isLoading ? (
          <div className="chat-sidebar-center">
            <Spin size="small" />
          </div>
        ) : conversationsQuery.isError ? (
          <div className="chat-sidebar-center">
            <Alert
              type="error"
              showIcon
              message={describeError(
                conversationsError?.code ?? 'UNKNOWN',
                conversationsError?.message ?? '会话列表加载失败。',
              )}
              description={
                conversationsError?.requestId
                  ? `request_id: ${conversationsError.requestId}`
                  : undefined
              }
              action={
                <Button size="small" onClick={() => void conversationsQuery.refetch()}>
                  重试
                </Button>
              }
            />
          </div>
        ) : conversations.length === 0 ? (
          <div className="chat-sidebar-center">
            <EmptyState description="还没有会话" />
          </div>
        ) : visibleConversations.length === 0 ? (
          <div className="chat-sidebar-center">
            {/* 搜索只过滤已加载页（首页 limit=200）：无匹配且还有未加载的
                更早会话时，不能断言「没有匹配」——提示已加载数量并提供
                联动入口继续向历史翻页；全部加载完才显示确定空态。 */}
            {searchEmptyWithMore ? (
              <EmptyState
                description={`已加载的 ${conversations.length} 条会话中没有匹配，加载更早的会话？`}
              >
                <Button
                  data-testid="search-load-more"
                  size="small"
                  loading={conversationsQuery.isFetchingNextPage}
                  disabled={conversationsQuery.isFetchNextPageError}
                  onClick={() => void conversationsQuery.fetchNextPage()}
                >
                  加载更早的会话
                </Button>
              </EmptyState>
            ) : (
              <EmptyState description="没有匹配的会话" />
            )}
          </div>
        ) : (
          visibleConversations.map((conversation) => {
            const active = conversation.id === activeId;
            return (
              <div
                key={conversation.id}
                role="button"
                tabIndex={0}
                data-conv-id={conversation.id}
                className={`chat-conv-item${active ? ' is-active' : ''}`}
                onClick={() => onNavigate(conversation.id)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' || event.key === ' ') {
                    event.preventDefault();
                    onNavigate(conversation.id);
                  }
                }}
              >
                <div className="chat-conv-main">
                  <div className="chat-conv-title" title={conversation.title}>
                    {conversation.title}
                  </div>
                  <div className="chat-conv-time">{formatTime(conversation.last_message_at)}</div>
                </div>
                <div className="chat-conv-more" onClick={(event) => event.stopPropagation()}>
                  <Dropdown
                    trigger={['click']}
                    menu={{
                      items: [
                        { key: 'rename', icon: <EditOutlined />, label: '重命名' },
                        { key: 'delete', icon: <DeleteOutlined />, label: '删除', danger: true },
                      ],
                      onClick: ({ key, domEvent }) => {
                        domEvent.stopPropagation();
                        if (key === 'rename') {
                          openRename(conversation);
                        } else if (key === 'delete') {
                          setDeleteTarget(conversation);
                        }
                      },
                    }}
                  >
                    <Button
                      type="text"
                      size="small"
                      icon={<MoreOutlined />}
                      aria-label={`会话操作：${conversation.title}`}
                    />
                  </Dropdown>
                </div>
              </div>
            );
          })
        )}

        {hasMoreConversations && !searchEmptyWithMore ? (
          <div className="chat-sidebar-more">
            <Button
              data-testid="load-more-conversations"
              block
              size="small"
              loading={conversationsQuery.isFetchingNextPage}
              disabled={conversationsQuery.isFetchNextPageError}
              onClick={() => void conversationsQuery.fetchNextPage()}
            >
              加载更早的会话
            </Button>
          </div>
        ) : null}
      </div>

      <Modal
        open={renameTarget !== null}
        title="重命名会话"
        okText="保存"
        cancelText="取消"
        confirmLoading={renameMutation.isPending}
        onOk={submitRename}
        onCancel={() => setRenameTarget(null)}
      >
        <Input
          value={renameValue}
          maxLength={200}
          showCount
          onChange={(event) => setRenameValue(event.target.value)}
          onPressEnter={submitRename}
          placeholder="会话标题"
        />
      </Modal>

      <ConfirmDeleteModal
        open={deleteTarget !== null}
        title={`删除会话「${deleteTarget?.title ?? ''}」？`}
        content="会话中的消息、工具调用与审批将一并删除，审计日志保留。此操作不可撤销。"
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (deleteTarget) {
            deleteMutation.mutate(deleteTarget.id);
          }
        }}
        onCancel={() => setDeleteTarget(null)}
      />
    </aside>
  );
}

// 会话侧栏在流式 token 每帧刷新时不应跟着重渲染；props 均已由调用方 memo 化。
export default memo(ConversationSidebar);
