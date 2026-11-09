import {
  ApiOutlined,
  CodeOutlined,
  DeleteOutlined,
  EditOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
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
import { engineLabel } from '../../components/admin/meta';
import { formatDateTime, shortId } from '../../components/admin/format';
import '../../styles/admin.css';
import DatabaseFormModal from './DatabaseFormDrawer';
import SqlConsole from './SqlConsole';
import { databaseConnectionString } from './databaseView';

type Database = components['schemas']['Database'];

interface LastTest {
  ok: boolean;
  latencyMs: number;
}

/**
 * 数据库连接列的地址文本（导出供单测）：
 * - mssql 命名实例显示 `host\instance`（端口经 Browser 解析），否则 `host:port`；
 * - redis 显示 `host:port`，副行以 `db N` 标识逻辑库（无 database_name）；
 * - postgres/mysql/sqlite 沿用一期渲染。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function databaseAddress(database: Database): { main: string; sub: string } {
  if (database.engine === 'sqlite') {
    return { main: database.sqlite_path || '—', sub: '' };
  }
  if (database.engine === 'redis') {
    const main = `${database.host}:${database.port}`;
    const parts = [`db ${database.redis_db}`];
    if (database.username) {
      parts.push(database.username);
    }
    return { main, sub: parts.join(' · ') };
  }
  const main =
    database.engine === 'mssql' && database.instance_name
      ? `${database.host}\\${database.instance_name}`
      : `${database.host}:${database.port}`;
  return { main, sub: `${database.database_name} · ${database.username}` };
}

/**
 * 连接信息组合串（契约 8.7 ②，FE-P2 六期）：`engine://host:port/database_name`；
 * sqlite 为 `sqlite://路径`；redis 为 `redis://host:port`（库名在 redis_db）。
 *
 * 六期打磨 sprint 1：移到 databaseView.ts 由列表与表单抽屉共用（表单只读态
 * 的复制入口需要同一口径的组合串，逐字一致才不误导）。
 */

export default function DatabasesPage() {
  const queryClient = useQueryClient();
  const [keyword, setKeyword] = useState('');
  const [actionError, setActionError] = useState<ApiFailure | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Database | null>(null);
  const [deleting, setDeleting] = useState<Database | null>(null);
  const [sqlTarget, setSqlTarget] = useState<Database | null>(null);
  const [lastTests, setLastTests] = useState<Record<string, LastTest>>({});
  const [testSubject, setTestSubject] = useState<Database | null>(null);
  const [testSuccess, setTestSuccess] = useState<ConnectionTestSuccess | null>(null);
  const [testError, setTestError] = useState<ApiFailure | null>(null);

  const databasesQuery = useQuery({
    queryKey: qk.databases,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/databases');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (database: Database) => {
      const { error, response } = await api.DELETE('/api/databases/{id}', {
        params: { path: { id: database.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onSuccess: async (_data, database) => {
      setDeleting(null);
      message.success('数据库已删除');
      await queryClient.invalidateQueries({ queryKey: qk.databases });
      queryClient.removeQueries({ queryKey: qk.database(database.id) });
    },
    onError: (error) => {
      setDeleting(null);
      setActionError(failureFromError(error));
    },
  });

  const testMutation = useMutation({
    mutationFn: async (database: Database) => {
      const { data, error, response } = await api.POST('/api/databases/{id}/test', {
        params: { path: { id: database.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data, database) => {
      const latencyMs = data?.latency_ms ?? 0;
      setLastTests((previous) => ({ ...previous, [database.id]: { ok: true, latencyMs } }));
      setTestSubject(database);
      setTestError(null);
      setTestSuccess({
        latencyMs,
        lines: [
          { label: '数据库', value: `${database.name}（${engineLabel(database.engine)}）` },
          { label: '服务端版本', value: data?.server_version ?? '—' },
        ],
        note: null,
      });
    },
    onError: (error, database) => {
      setLastTests((previous) => ({
        ...previous,
        [database.id]: { ok: false, latencyMs: 0 },
      }));
      setTestSubject(database);
      setTestSuccess(null);
      setTestError(failureFromError(error));
    },
  });

  const databases = databasesQuery.data ?? [];
  const normalizedKeyword = keyword.trim().toLowerCase();
  const filtered = normalizedKeyword
    ? databases.filter((database) => {
        const haystack = [
          database.name,
          database.host,
          database.database_name,
          database.username,
          database.sqlite_path,
          database.instance_name,
        ]
          .join(' ')
          .toLowerCase();
        return haystack.includes(normalizedKeyword);
      })
    : databases;

  const columns: TableColumnsType<Database> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      width: 200,
      render: (_value, record) => (
        <div>
          <div className="opsmind-admin-cell-title">{record.name}</div>
          {/* 契约 8.7 ②（FE-P2 六期）：数据库 ID 复制。 */}
          <div className="opsmind-admin-cell-sub">
            ID {shortId(record.id, 12)} <CopyButton text={record.id} label="复制数据库 ID" />
          </div>
        </div>
      ),
    },
    {
      title: '类型',
      dataIndex: 'engine',
      key: 'engine',
      width: 120,
      render: (_value, record) => <Tag color="blue">{engineLabel(record.engine)}</Tag>,
    },
    {
      title: '连接',
      key: 'connection',
      width: 280,
      render: (_value, record) => {
        const address = databaseAddress(record);
        return (
          <div>
            {/* 契约 8.7 ②（FE-P2 六期）：连接信息组合串复制。 */}
            <div className="opsmind-admin-mono">
              {address.main}{' '}
              <CopyButton text={databaseConnectionString(record)} label="复制连接信息" />
            </div>
            {address.sub ? <div className="opsmind-admin-cell-sub">{address.sub}</div> : null}
          </div>
        );
      },
    },
    {
      title: 'SSL',
      dataIndex: 'ssl_mode',
      key: 'ssl_mode',
      width: 130,
      render: (_value, record) =>
        record.engine === 'sqlite' ? (
          <Typography.Text type="secondary">—</Typography.Text>
        ) : (
          <span className="opsmind-admin-mono">{record.ssl_mode}</span>
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
            {record.has_password ? (
              <Tag color="green">密码已保存</Tag>
            ) : (
              <Tag color="gold">无密码</Tag>
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
      width: 290,
      fixed: 'right',
      render: (_value, record) => (
        <div className="opsmind-admin-actions">
          <Button
            type="link"
            size="small"
            icon={<CodeOutlined />}
            onClick={() => setSqlTarget(record)}
          >
            查询
          </Button>
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
        title="数据库"
        description="外部数据库连接与只读 SQL 查询"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              loading={databasesQuery.isFetching}
              onClick={() => void databasesQuery.refetch()}
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
              新增数据库
            </Button>
          </Space>
        }
      />

      {actionError ? (
        <ApiErrorAlert failure={actionError} closable onClose={() => setActionError(null)} />
      ) : null}

      <Flex wrap gap={12} align="center" className="opsmind-admin-toolbar">
        <Input
          className="opsmind-admin-toolbar-grow"
          allowClear
          prefix={<SearchOutlined />}
          placeholder="搜索名称 / 主机 / 数据库名 / 用户名"
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
        />
        <Typography.Text type="secondary">共 {filtered.length} 个</Typography.Text>
      </Flex>

      {databasesQuery.isPending ? <AdminLoading /> : null}
      {databasesQuery.isError ? (
        <QueryErrorState
          error={databasesQuery.error}
          onRetry={() => void databasesQuery.refetch()}
        />
      ) : null}
      {databasesQuery.isSuccess ? (
        <Table<Database>
          rowKey="id"
          columns={columns}
          dataSource={filtered}
          size="middle"
          scroll={{ x: 1220 }}
          locale={{
            emptyText: <EmptyState description="还没有数据库，点击右上角“新增数据库”开始配置" />,
          }}
          pagination={{
            pageSize: 10,
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50],
            showTotal: (total) => `共 ${total} 个`,
          }}
        />
      ) : null}

      <DatabaseFormModal
        key={editing?.id ?? 'new'}
        open={formOpen}
        database={editing}
        onClose={() => {
          setFormOpen(false);
          setEditing(null);
        }}
        onSaved={() => {
          message.success(editing ? '数据库已更新' : '数据库已创建');
          setFormOpen(false);
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: qk.databases });
          if (editing) {
            void queryClient.invalidateQueries({ queryKey: qk.database(editing.id) });
          }
        }}
      />

      <ConfirmDeleteModal
        open={deleting !== null}
        title={`删除数据库「${deleting?.name ?? ''}」？`}
        content="删除后该数据库的保存凭据将被移除，此操作不可撤销。"
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (deleting) {
            deleteMutation.mutate(deleting);
          }
        }}
        onCancel={() => setDeleting(null)}
      />

      {sqlTarget ? (
        <SqlConsole
          key={sqlTarget.id}
          database={sqlTarget}
          open
          onClose={() => setSqlTarget(null)}
        />
      ) : null}

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
