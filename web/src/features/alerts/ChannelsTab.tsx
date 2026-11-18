// M4 五期：通知渠道 Tab（CONTRACT.md 8.7 渠道表——kind/config_hint/has_config；
// 编辑、删除、测试）。测试按钮在表单抽屉内（需先保存）。

import { DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, Flex, Popconfirm, Space, Table, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import EmptyState from '../../components/EmptyState';
import { message } from '../../components/antdMessage';
import { hasAnyPermission, PERMS } from '../../lib/rbacPermissions';
import { usePermissions } from '../auth/useMe';

import { useNotificationChannels } from './useNotificationChannels';
import { CHANNEL_KIND_LABELS } from './channelForm';
import { describePhase5Failure } from './phase5Codes';

type NotificationChannel = components['schemas']['NotificationChannel'];

export interface ChannelsTabProps {
  onCreate: () => void;
  onEdit: (channel: NotificationChannel) => void;
}

export default function ChannelsTab({ onCreate, onEdit }: ChannelsTabProps) {
  const queryClient = useQueryClient();
  // RBAC 走查 sprint 1（medium）：渠道 CRUD 要求 alerts:write
  //（后端 authz.PermAlertConfig），只读角色不渲染入口。
  const permissions = usePermissions();
  const canConfigure = hasAnyPermission(permissions, [PERMS.alertsWrite]);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const channelsQuery = useNotificationChannels();

  const deleteMutation = useMutation({
    mutationFn: async (channelId: string) => {
      const { error, response } = await api.DELETE('/api/notification-channels/{id}', {
        params: { header: writeHeaders(), path: { id: channelId } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onMutate: (channelId) => {
      setDeletingId(channelId);
    },
    onSettled: () => {
      setDeletingId(null);
    },
    onSuccess: () => {
      message.success('渠道已删除');
      // 8.4：渠道 CRUD → 失效渠道与规则（规则表渠道名展示）。
      void queryClient.invalidateQueries({ queryKey: qk.notificationChannels });
      void queryClient.invalidateQueries({ queryKey: qk.alertRules });
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const columns: TableColumnsType<NotificationChannel> = [
    {
      title: '名称',
      dataIndex: 'name',
      render: (name: string, channel) => (
        <Space direction="vertical" size={0}>
          <span className="opsmind-admin-cell-title">{name}</span>
          <span className="opsmind-admin-cell-sub">
            更新于 {formatDateTime(channel.updated_at)}
          </span>
        </Space>
      ),
    },
    {
      title: '类型',
      dataIndex: 'kind',
      width: 110,
      render: (kind: string) => (
        <Tag>{CHANNEL_KIND_LABELS[kind as keyof typeof CHANNEL_KIND_LABELS] ?? kind}</Tag>
      ),
    },
    {
      title: '配置',
      dataIndex: 'config_hint',
      ellipsis: true,
      render: (hint: string, channel) => (
        <Space size={8}>
          <span className="opsmind-admin-mono">{hint || '—'}</span>
          {!channel.has_config ? <Typography.Text type="warning">未配置</Typography.Text> : null}
        </Space>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 96,
      render: (_, channel) =>
        canConfigure ? (
          <Space size={0}>
            <Button
              size="small"
              type="text"
              icon={<EditOutlined />}
              aria-label={`编辑渠道 ${channel.name}`}
              onClick={() => {
                onEdit(channel);
              }}
            />
            <Popconfirm
              title="删除渠道"
              description={`确定删除「${channel.name}」？引用它的规则将解除绑定。`}
              okText="删除"
              okButtonProps={{ danger: true }}
              onConfirm={() => {
                deleteMutation.mutate(channel.id);
              }}
            >
              <Button
                size="small"
                type="text"
                danger
                icon={<DeleteOutlined />}
                aria-label={`删除渠道 ${channel.name}`}
                loading={deletingId === channel.id}
              />
            </Popconfirm>
          </Space>
        ) : (
          <Typography.Text type="secondary">只读</Typography.Text>
        ),
    },
  ];

  if (channelsQuery.isError) {
    return (
      <QueryErrorState
        error={channelsQuery.error}
        onRetry={() => {
          void channelsQuery.refetch();
        }}
      />
    );
  }

  return (
    <Flex vertical gap={12}>
      <Flex justify="flex-end">
        {canConfigure ? (
          <Button type="primary" icon={<PlusOutlined />} onClick={onCreate}>
            新建渠道
          </Button>
        ) : (
          <Typography.Text type="secondary">当前角色只读，不能新建或修改通知渠道</Typography.Text>
        )}
      </Flex>
      <Table<NotificationChannel>
        rowKey="id"
        size="small"
        loading={channelsQuery.isLoading}
        columns={columns}
        dataSource={channelsQuery.data ?? []}
        locale={{ emptyText: <EmptyState description="暂无通知渠道" /> }}
        pagination={false}
      />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        渠道凭据加密存储且永不回显；编辑时留空表示不修改配置。
      </Typography.Text>
    </Flex>
  );
}
