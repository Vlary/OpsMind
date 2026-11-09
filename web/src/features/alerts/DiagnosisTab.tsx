// M4 五期：AI 诊断 Tab（CONTRACT.md 8.7 告警页「诊断报告入口」）。
// 会话列表（created_at DESC）+ 手动发起（选服务器 device_kind=server）；
// 点击行打开 DiagnosisDrawer（报告/建议/审批）。

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Flex, Select, Space, Table, Tag } from 'antd';
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

import { describePhase5Failure } from './phase5Codes';
import { DIAGNOSIS_STATUS_LABELS } from './diagnosisStatus';

type DiagnosisSession = components['schemas']['DiagnosisSession'];
type Server = components['schemas']['Server'];

export interface DiagnosisTabProps {
  onOpen: (sessionId: string) => void;
}

export default function DiagnosisTab({ onOpen }: DiagnosisTabProps) {
  const queryClient = useQueryClient();
  const [serverId, setServerId] = useState<string | null>(null);

  const sessionsQuery = useQuery({
    queryKey: qk.diagnosisSessions({}),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/diagnosis/sessions', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const serversQuery = useQuery({
    queryKey: qk.servers,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const serverOptions = (serversQuery.data ?? [])
    .filter((server: Server) => server.device_kind === 'server')
    .map((server: Server) => ({ value: server.id, label: server.name }));

  const createMutation = useMutation({
    mutationFn: async (targetServerId: string) => {
      const { data, error, response } = await api.POST('/api/diagnosis/sessions', {
        params: { header: writeHeaders() },
        body: { server_id: targetServerId },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (session: DiagnosisSession | undefined) => {
      message.success('诊断已发起');
      // 8.4：创建成功 → 失效 diagnosisSessions；直接打开详情（3s 轮询至终态）。
      void queryClient.invalidateQueries({ queryKey: qk.diagnosisSessions({}) });
      if (session) {
        onOpen(session.id);
      }
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const columns: TableColumnsType<DiagnosisSession> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string, session) => name || session.server_id.slice(0, 8),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (status: string) => (
        <Tag color={status === 'failed' ? 'red' : status === 'done' ? 'green' : 'default'}>
          {DIAGNOSIS_STATUS_LABELS[status] ?? status}
        </Tag>
      ),
    },
    {
      title: '触发',
      dataIndex: 'trigger',
      width: 90,
      render: (trigger: string) =>
        trigger === 'alert' ? '告警自动' : trigger === 'manual' ? '手动' : '对话',
    },
    {
      title: '严重度',
      key: 'severity',
      width: 90,
      render: (_, session) =>
        session.report ? (
          <Tag
            color={
              session.report.severity === 'critical'
                ? 'red'
                : session.report.severity === 'warning'
                  ? 'gold'
                  : 'blue'
            }
          >
            {session.report.severity}
          </Tag>
        ) : (
          '—'
        ),
    },
    {
      title: '摘要',
      key: 'summary',
      ellipsis: true,
      render: (_, session) => session.report?.summary ?? session.error_code ?? '—',
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      width: 170,
      render: (value: string) => formatDateTime(value),
    },
    {
      title: '操作',
      key: 'actions',
      width: 90,
      render: (_, session) => (
        <Button
          size="small"
          onClick={() => {
            onOpen(session.id);
          }}
        >
          查看报告
        </Button>
      ),
    },
  ];

  if (sessionsQuery.isError) {
    return (
      <QueryErrorState
        error={sessionsQuery.error}
        onRetry={() => {
          void sessionsQuery.refetch();
        }}
      />
    );
  }

  return (
    <Flex vertical gap={12}>
      <Flex gap={12} wrap="wrap">
        <Select
          style={{ minWidth: 260 }}
          placeholder="选择服务器发起诊断"
          aria-label="诊断目标服务器"
          value={serverId}
          onChange={setServerId}
          options={serverOptions}
          notFoundContent={<EmptyState description="暂无服务器资产" />}
        />
        <Button
          type="primary"
          disabled={serverId === null}
          loading={createMutation.isPending}
          onClick={() => {
            if (serverId) {
              createMutation.mutate(serverId);
            }
          }}
        >
          发起 AI 诊断
        </Button>
        <Space size={4}>
          <span style={{ fontSize: 12, color: 'var(--color-text-tertiary)' }}>
            诊断引擎自动执行安全采集命令（仅 safe
            级），产出根因分析与修复建议；修复命令需审批后执行。
          </span>
        </Space>
      </Flex>
      <Table<DiagnosisSession>
        rowKey="id"
        size="small"
        loading={sessionsQuery.isLoading}
        columns={columns}
        dataSource={sessionsQuery.data ?? []}
        locale={{ emptyText: <EmptyState description="暂无诊断会话" /> }}
        pagination={false}
      />
    </Flex>
  );
}
