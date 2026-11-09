import {
  ApiOutlined,
  DeleteOutlined,
  EditOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
  SyncOutlined,
} from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Flex, Input, Space, Table, Tag, Tooltip, Typography } from 'antd';
import { message } from '../../components/antdMessage';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import ConfirmDeleteModal from '../../components/ConfirmDeleteModal';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import ConnectionTestModal, {
  type ConnectionTestSuccess,
} from '../../components/admin/ConnectionTestModal';
import {
  ApiRequestError,
  failureFromError,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { formatDateTime, shortId } from '../../components/admin/format';
import { providerKindLabel } from '../../components/admin/meta';
import '../../styles/admin.css';
import ProviderFormModal from './ProviderFormDrawer';

type Provider = components['schemas']['Provider'];

interface LastTest {
  ok: boolean;
  latencyMs: number;
}

export default function ProvidersPage() {
  const queryClient = useQueryClient();
  const [keyword, setKeyword] = useState('');
  const [actionError, setActionError] = useState<ApiFailure | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [deleting, setDeleting] = useState<Provider | null>(null);
  const [lastTests, setLastTests] = useState<Record<string, LastTest>>({});
  const [testSubject, setTestSubject] = useState<Provider | null>(null);
  const [testSuccess, setTestSuccess] = useState<ConnectionTestSuccess | null>(null);
  const [testError, setTestError] = useState<ApiFailure | null>(null);

  const providersQuery = useQuery({
    queryKey: qk.providers,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/providers');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const presetsQuery = useQuery({
    queryKey: qk.presets,
    staleTime: Infinity,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/providers/presets');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (provider: Provider) => {
      const { error, response } = await api.DELETE('/api/providers/{id}', {
        params: { path: { id: provider.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onSuccess: async (_data, provider) => {
      setDeleting(null);
      message.success('供应商已删除');
      await queryClient.invalidateQueries({ queryKey: qk.providers });
      queryClient.removeQueries({ queryKey: qk.provider(provider.id) });
    },
    onError: (error) => {
      setDeleting(null);
      setActionError(failureFromError(error));
    },
  });

  const testMutation = useMutation({
    mutationFn: async (provider: Provider) => {
      const { data, error, response } = await api.POST('/api/providers/{id}/test', {
        params: { path: { id: provider.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data, provider) => {
      const latencyMs = data?.latency_ms ?? 0;
      setLastTests((previous) => ({ ...previous, [provider.id]: { ok: true, latencyMs } }));
      setTestSubject(provider);
      setTestError(null);
      setTestSuccess({
        latencyMs,
        lines: [
          { label: '供应商', value: `${provider.name}（${providerKindLabel(provider.kind)}）` },
          { label: 'Base URL', value: provider.base_url },
          { label: '可用模型数', value: String(data?.model_count ?? 0) },
        ],
        note: null,
      });
    },
    onError: (error, provider) => {
      setLastTests((previous) => ({
        ...previous,
        [provider.id]: { ok: false, latencyMs: 0 },
      }));
      setTestSubject(provider);
      setTestSuccess(null);
      setTestError(failureFromError(error));
    },
  });

  const refreshMutation = useMutation({
    mutationFn: async (provider: Provider) => {
      const { data, error, response } = await api.POST('/api/providers/{id}/models/refresh', {
        params: { path: { id: provider.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: async (data, provider) => {
      message.success(`已刷新「${provider.name}」的模型列表：${data?.models.length ?? 0} 个`);
      await queryClient.invalidateQueries({ queryKey: qk.providers });
      await queryClient.invalidateQueries({ queryKey: qk.provider(provider.id) });
    },
    onError: (error) => {
      setActionError(failureFromError(error));
    },
  });

  const providers = providersQuery.data ?? [];
  const normalizedKeyword = keyword.trim().toLowerCase();
  const filtered = normalizedKeyword
    ? providers.filter((provider) => {
        const haystack = [
          provider.name,
          provider.preset,
          provider.kind,
          provider.base_url,
          provider.default_model,
          ...provider.models,
        ]
          .join(' ')
          .toLowerCase();
        return haystack.includes(normalizedKeyword);
      })
    : providers;

  const columns: TableColumnsType<Provider> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      width: 200,
      render: (_value, record) => (
        <div>
          <div className="opsmind-admin-cell-title">{record.name}</div>
          <div className="opsmind-admin-cell-sub">
            {record.preset} · ID {shortId(record.id, 12)}
          </div>
        </div>
      ),
    },
    {
      title: '类型',
      dataIndex: 'kind',
      key: 'kind',
      width: 130,
      render: (_value, record) => <Tag color="blue">{providerKindLabel(record.kind)}</Tag>,
    },
    {
      title: 'Base URL',
      dataIndex: 'base_url',
      key: 'base_url',
      width: 280,
      ellipsis: true,
      render: (_value, record) => (
        <Tooltip title={record.base_url}>
          {/* 契约 8.7 ③（FE-P2 六期）：供应商 base URL 复制。 */}
          <span className="opsmind-admin-url">
            {record.base_url} <CopyButton text={record.base_url} label="复制 Base URL" />
          </span>
        </Tooltip>
      ),
    },
    {
      title: '默认模型',
      dataIndex: 'default_model',
      key: 'default_model',
      width: 210,
      render: (_value, record) => (
        <div>
          {/* 契约 8.7 ③（FE-P2 六期）：模型名复制。 */}
          <div className="opsmind-admin-mono">
            {record.default_model} <CopyButton text={record.default_model} label="复制模型名" />
          </div>
          <div className="opsmind-admin-cell-sub">共 {record.models.length} 个模型</div>
        </div>
      ),
    },
    {
      title: '状态',
      key: 'status',
      width: 220,
      render: (_value, record) => {
        const lastTest = lastTests[record.id];
        return (
          <Space size={[4, 4]} wrap>
            {record.has_api_key ? (
              <Tooltip title={record.api_key_masked}>
                <Tag color="green">密钥已配置</Tag>
              </Tooltip>
            ) : record.preset === 'ollama' || record.preset === 'custom' ? (
              <Tag>无需密钥</Tag>
            ) : (
              <Tag color="gold">未配置密钥</Tag>
            )}
            {lastTest ? (
              <Tag color={lastTest.ok ? 'green' : 'red'}>
                {lastTest.ok ? `测试通过 ${lastTest.latencyMs} ms` : '测试失败'}
              </Tag>
            ) : (
              <Tooltip title={`更新于 ${formatDateTime(record.updated_at)}`}>
                <Tag>未测试</Tag>
              </Tooltip>
            )}
          </Space>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      width: 300,
      fixed: 'right',
      render: (_value, record) => (
        <div className="opsmind-admin-actions">
          <Button
            type="link"
            size="small"
            icon={<ApiOutlined />}
            loading={testMutation.isPending && testMutation.variables?.id === record.id}
            onClick={() => testMutation.mutate(record)}
          >
            测试
          </Button>
          <Button
            type="link"
            size="small"
            icon={<SyncOutlined />}
            loading={refreshMutation.isPending && refreshMutation.variables?.id === record.id}
            onClick={() => refreshMutation.mutate(record)}
          >
            拉取模型
          </Button>
          <Button
            type="link"
            size="small"
            icon={<EditOutlined />}
            onClick={() => {
              setEditing(record);
              setFormOpen(true);
            }}
          >
            编辑
          </Button>
          <Button
            type="link"
            size="small"
            danger
            icon={<DeleteOutlined />}
            onClick={() => setDeleting(record)}
          >
            删除
          </Button>
        </div>
      ),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="模型"
        description="大模型供应商、默认模型与连接测试"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              loading={providersQuery.isFetching}
              onClick={() => void providersQuery.refetch()}
            >
              刷新
            </Button>
            <Button
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新增供应商
            </Button>
          </Space>
        }
      />

      {actionError ? (
        <ApiErrorAlert failure={actionError} closable onClose={() => setActionError(null)} />
      ) : null}

      {presetsQuery.isError ? (
        <QueryErrorState error={presetsQuery.error} onRetry={() => void presetsQuery.refetch()} />
      ) : null}

      <Flex wrap gap={12} align="center" className="opsmind-admin-toolbar">
        <Input
          className="opsmind-admin-toolbar-grow"
          allowClear
          prefix={<SearchOutlined />}
          placeholder="搜索名称 / 类型 / Base URL / 模型"
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
        />
        <Typography.Text type="secondary">共 {filtered.length} 个</Typography.Text>
      </Flex>

      {providersQuery.isPending ? <AdminLoading /> : null}
      {providersQuery.isError ? (
        <QueryErrorState
          error={providersQuery.error}
          onRetry={() => void providersQuery.refetch()}
        />
      ) : null}
      {providersQuery.isSuccess ? (
        <Table<Provider>
          rowKey="id"
          columns={columns}
          dataSource={filtered}
          size="middle"
          scroll={{ x: 1340 }}
          locale={{
            emptyText: <EmptyState description="还没有供应商，点击右上角“新增供应商”开始配置" />,
          }}
          pagination={{
            pageSize: 10,
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50],
            showTotal: (total) => `共 ${total} 个`,
          }}
        />
      ) : null}

      <ProviderFormModal
        key={editing?.id ?? 'new'}
        open={formOpen}
        provider={editing}
        presets={presetsQuery.data ?? []}
        onClose={() => {
          setFormOpen(false);
          setEditing(null);
        }}
        onSaved={() => {
          message.success(editing ? '供应商已更新' : '供应商已创建');
          setFormOpen(false);
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: qk.providers });
          if (editing) {
            void queryClient.invalidateQueries({ queryKey: qk.provider(editing.id) });
          }
        }}
      />

      <ConfirmDeleteModal
        open={deleting !== null}
        title={`删除供应商「${deleting?.name ?? ''}」？`}
        content="删除后关联会话的供应商绑定会被置空，历史消息保留。此操作不可撤销。"
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (deleting) {
            deleteMutation.mutate(deleting);
          }
        }}
        onCancel={() => setDeleting(null)}
      />

      <ConnectionTestModal
        open={testSuccess !== null || testError !== null}
        title={`测试连接 · ${testSubject?.name ?? ''}`}
        success={testSuccess}
        error={testError}
        onClose={() => {
          setTestSuccess(null);
          setTestError(null);
          setTestSubject(null);
        }}
      />
    </div>
  );
}
