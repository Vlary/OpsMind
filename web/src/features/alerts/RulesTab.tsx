// M4 五期：规则 Tab（CONTRACT.md 8.7——列 name/metric comparator threshold/
// 窗口/冷却/等级/渠道/enabled（Switch 直接 PATCH）；编辑/删除行操作）。

import { DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Flex, Popconfirm, Space, Switch, Table, Tag, Tooltip, Typography } from 'antd';
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
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import { message } from '../../components/antdMessage';

import { METRIC_LABELS, ruleDisplayName, SEVERITY_LABELS } from './ruleForm';
import { describePhase5Failure } from './phase5Codes';
import { useNotificationChannels } from './useNotificationChannels';

type AlertRule = components['schemas']['AlertRule'];

const severityColor: Record<string, string> = {
  critical: 'red',
  warning: 'gold',
  info: 'blue',
};

export interface RulesTabProps {
  onCreate: () => void;
  onEdit: (rule: AlertRule) => void;
}

export default function RulesTab({ onCreate, onEdit }: RulesTabProps) {
  const queryClient = useQueryClient();
  const [togglingId, setTogglingId] = useState<string | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);

  const rulesQuery = useQuery({
    queryKey: qk.alertRules,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/alert-rules');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });
  const channelsQuery = useNotificationChannels();
  const channelNames = new Map(
    (channelsQuery.data ?? []).map((channel) => [channel.id, channel.name]),
  );

  const toggleMutation = useMutation({
    mutationFn: async (input: { rule: AlertRule; enabled: boolean }) => {
      const { data, error, response } = await api.PATCH('/api/alert-rules/{id}', {
        params: { header: writeHeaders(), path: { id: input.rule.id } },
        body: { enabled: input.enabled },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onMutate: (input) => {
      setTogglingId(input.rule.id);
    },
    onSettled: () => {
      setTogglingId(null);
    },
    onSuccess: (_, input) => {
      message.success(input.enabled ? '规则已启用' : '规则已停用');
      void queryClient.invalidateQueries({ queryKey: qk.alertRules });
      void queryClient.invalidateQueries({ queryKey: qk.alertRule(input.rule.id) });
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (ruleId: string) => {
      const { error, response } = await api.DELETE('/api/alert-rules/{id}', {
        params: { header: writeHeaders(), path: { id: ruleId } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onMutate: (ruleId) => {
      setDeletingId(ruleId);
    },
    onSettled: () => {
      setDeletingId(null);
    },
    onSuccess: () => {
      message.success('规则已删除');
      void queryClient.invalidateQueries({ queryKey: qk.alertRules });
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const rules = rulesQuery.data ?? [];

  const columns: TableColumnsType<AlertRule> = [
    {
      title: '名称',
      dataIndex: 'name',
      render: (name: string, rule) => (
        <Space direction="vertical" size={0}>
          {/* 六期打磨 sprint 4（走查 low）：系统哨兵规则的英文原名按名映射
              中文展示（原始名在 Tooltip 与副行保留，供排查/对日志）。 */}
          <Tooltip title={name === ruleDisplayName(name) ? null : name}>
            <span className="opsmind-admin-cell-title">
              {ruleDisplayName(name)} {/* 契约 8.7 ⑦（FE-P2 六期）：规则 ID 复制。 */}
              <CopyButton text={rule.id} label="复制规则 ID" />
            </span>
          </Tooltip>
          <span className="opsmind-admin-cell-sub">
            {name === ruleDisplayName(name)
              ? `更新于 ${formatDateTime(rule.updated_at)}`
              : `${name} · 更新于 ${formatDateTime(rule.updated_at)}`}
          </span>
        </Space>
      ),
    },
    {
      title: '条件',
      key: 'condition',
      render: (_, rule) => (
        <span className="opsmind-admin-mono">
          {METRIC_LABELS[rule.metric] ?? rule.metric} {rule.comparator === 'gt' ? '>' : '<'}{' '}
          {rule.threshold}
        </span>
      ),
    },
    {
      title: '窗口 / 冷却',
      key: 'window',
      render: (_, rule) => (
        <Space direction="vertical" size={0}>
          <span>持续 {rule.duration_windows} 点</span>
          <span className="opsmind-admin-cell-sub">冷却 {rule.cooldown_seconds}s</span>
        </Space>
      ),
    },
    {
      title: '等级',
      dataIndex: 'severity',
      width: 80,
      render: (severity: string) => (
        <Tag color={severityColor[severity] ?? 'default'}>
          {SEVERITY_LABELS[severity as keyof typeof SEVERITY_LABELS] ?? severity}
        </Tag>
      ),
    },
    {
      title: '渠道',
      key: 'channel',
      width: 140,
      render: (_, rule) =>
        rule.notify_channel_id ? (
          <Tooltip title={rule.notify_channel_id}>
            {channelNames.get(rule.notify_channel_id) ?? rule.notify_channel_id.slice(0, 8)}
          </Tooltip>
        ) : (
          <Typography.Text type="secondary">不通知</Typography.Text>
        ),
    },
    {
      title: '静默至',
      dataIndex: 'silent_until',
      width: 150,
      render: (value: string | null) => (value ? formatDateTime(value) : '—'),
    },
    {
      title: '启用',
      dataIndex: 'enabled',
      width: 72,
      render: (enabled: boolean, rule) => (
        <Switch
          size="small"
          checked={enabled}
          loading={togglingId === rule.id}
          onChange={(checked) => {
            toggleMutation.mutate({ rule, enabled: checked });
          }}
          aria-label={`启用规则 ${rule.name}`}
        />
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 96,
      render: (_, rule) => (
        <Space size={0}>
          <Button
            size="small"
            type="text"
            icon={<EditOutlined />}
            aria-label={`编辑规则 ${rule.name}`}
            onClick={() => {
              onEdit(rule);
            }}
          />
          <Popconfirm
            title="删除规则"
            description={`确定删除「${ruleDisplayName(rule.name)}」？关联事件将级联删除。`}
            okText="删除"
            okButtonProps={{ danger: true }}
            onConfirm={() => {
              deleteMutation.mutate(rule.id);
            }}
          >
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              aria-label={`删除规则 ${rule.name}`}
              loading={deletingId === rule.id}
            />
          </Popconfirm>
        </Space>
      ),
    },
  ];

  if (rulesQuery.isError) {
    return (
      <QueryErrorState
        error={rulesQuery.error}
        onRetry={() => {
          void rulesQuery.refetch();
        }}
      />
    );
  }

  return (
    <Flex vertical gap={12}>
      <Flex justify="flex-end">
        <Button type="primary" icon={<PlusOutlined />} onClick={onCreate}>
          新建规则
        </Button>
      </Flex>
      <Table<AlertRule>
        rowKey="id"
        size="small"
        loading={rulesQuery.isLoading}
        columns={columns}
        dataSource={rules}
        locale={{ emptyText: <EmptyState description="暂无告警规则" /> }}
        pagination={false}
      />
    </Flex>
  );
}
