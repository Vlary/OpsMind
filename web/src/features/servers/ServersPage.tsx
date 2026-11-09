import {
  DeleteOutlined,
  EditOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
} from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Button,
  Flex,
  Input,
  Popconfirm,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { TableColumnsType } from 'antd';
import { message } from '../../components/antdMessage';
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
import { isWinRMAuthMethod, osLabel } from '../../components/admin/meta';
import '../../styles/admin.css';
import ServerFormModal, { isNetworkDevice, netdevBrandLabel } from './ServerFormDrawer';
import ServerTestButton from './ServerTestButton';
// C2 四期：行内容器运行时徽标（契约 8.7 服务器列表页；'' 不显示）。
import RuntimeBadge from '../containers/RuntimeBadge';

type Server = components['schemas']['Server'];

interface LastTest {
  ok: boolean;
  latencyMs: number;
}

/**
 * 服务器列表关键词过滤（导出供单测）。地址列渲染 host:port，端口同样
 * 参与匹配——用户按「10.0.0.99:2222」或裸端口「2222」直觉搜索时不漏行
 * （第 10 轮打磨：此前 haystack 无 port 字段，按端口搜索恒 0 行）。
 * 大小写不敏感；关键词去首尾空白后为空时视为不过滤。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function serverMatchesKeyword(server: Server, keyword: string): boolean {
  const normalized = keyword.trim().toLowerCase();
  if (!normalized) {
    return true;
  }
  // 网络设备专属字段（品牌中文名、接入协议）只对 network_device 追加：
  // server 类资产 netdev_brand 恒 ''、access_proto 恒 'ssh'，若一律拼进
  // haystack，「通用」「ssh」等常量会匹配所有服务器行（三期评审）。
  const netdev = server.device_kind === 'network_device';
  const haystack = [
    server.name,
    server.host,
    server.username,
    server.os,
    server.device_kind,
    server.netdev_brand,
    ...(netdev ? [server.access_proto, netdevBrandLabel(server.netdev_brand)] : []),
    ...server.tags,
    String(server.port),
    String(server.winrm_port),
    `${server.host}:${server.port}`,
  ]
    .join(' ')
    .toLowerCase();
  return haystack.includes(normalized);
}

// 三期（N3）：服务器/网络设备连接通道描述（契约 2.7/2.8）。
// - server：SSH（password/private_key）或 WinRM（NTLM/Basic，winrm_port）；
// - network_device：SSH（PTY+expect，复用 sshx）或 Telnet（明文行协议）。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数）
export function serverChannelOf(server: Server): {
  kind: 'ssh' | 'winrm' | 'netdev_ssh' | 'netdev_telnet';
  label: string;
  hasHostKey: boolean;
} {
  if (isNetworkDevice(server)) {
    return server.access_proto === 'telnet'
      ? { kind: 'netdev_telnet', label: 'Telnet', hasHostKey: false }
      : { kind: 'netdev_ssh', label: 'SSH', hasHostKey: true };
  }
  if (server.os === 'windows' && isWinRMAuthMethod(server.auth_method)) {
    return { kind: 'winrm', label: 'WinRM', hasHostKey: false };
  }
  return { kind: 'ssh', label: 'SSH', hasHostKey: true };
}

// 三期错误码（契约 9.6）：NETCLI_* 无通用中文映射（apiError.ts 不归 N3），
// 测试弹窗展示前先把 message 本地化，保留 code/request_id 小字。
const NETCLI_CODE_MESSAGES: Record<string, string> = {
  NETCLI_LOGIN_FAILED: '网络设备登录失败',
  NETCLI_EXEC_FAILED: '网络设备命令执行失败',
};

// p3 sprint 4：Telnet 明文告警（契约 2.6 必须显示）是后端单一事实源英文
// 常量 telnetx.CleartextWarning（internal/netdev/telnetx/telnetx.go:29，
// 创建确认/审计/结果透传共用，不动后端）；测试成功弹窗其余内容全中文，
// 英文整句直达用户与全站 zh_CN 口径相悖（走查 W2）——已知文案在此映射
// 中文，未知值回退原文（后端语义不吞）。
const CLEARTEXT_WARNING_MESSAGES: Record<string, string> = {
  'telnet transmits the login password and enable secret in cleartext; prefer ssh when the device supports it':
    'Telnet 以明文传输登录与特权密码，设备支持时建议改用 SSH',
};

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数）
export function localizeCleartextWarning(warning: string | null | undefined): string | null {
  if (warning === null || warning === undefined) {
    return null;
  }
  return CLEARTEXT_WARNING_MESSAGES[warning.trim()] ?? warning;
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数）
export function localizeTestFailure(failure: ApiFailure): ApiFailure {
  const localized = NETCLI_CODE_MESSAGES[failure.code];
  if (!localized) {
    return failure;
  }
  // p3 sprint 3：NETCLI_* 与 SERVER_TEST_FAILED 同源（servers.go
  // describeConnTestFailure），message 头部同样带英文通道前缀
  // "network device connection test failed: …"。标题已是中文（如「网络
  // 设备登录失败」），保留前缀会拼出中英重复（实测走查 sprint 3）——
  // 对齐 apiError.ts describeFailure 对 SERVER_TEST_FAILED 的剥离。
  const prefix = 'network device connection test failed';
  let detail = failure.message.trim();
  if (detail.toLowerCase().startsWith(prefix)) {
    detail = detail.slice(prefix.length).replace(/^[\s:：]+/, '');
  }
  return {
    ...failure,
    message: detail !== '' ? `${localized}：${detail}` : localized,
  };
}

export default function ServersPage() {
  const queryClient = useQueryClient();
  const [keyword, setKeyword] = useState('');
  const [actionError, setActionError] = useState<ApiFailure | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Server | null>(null);
  const [deleting, setDeleting] = useState<Server | null>(null);
  const [lastTests, setLastTests] = useState<Record<string, LastTest>>({});
  const [testSubject, setTestSubject] = useState<Server | null>(null);
  const [testSuccess, setTestSuccess] = useState<ConnectionTestSuccess | null>(null);
  const [testError, setTestError] = useState<ApiFailure | null>(null);

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

  const deleteMutation = useMutation({
    mutationFn: async (server: Server) => {
      const { error, response } = await api.DELETE('/api/servers/{id}', {
        params: { path: { id: server.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onSuccess: async (_data, server) => {
      setDeleting(null);
      message.success('服务器已删除');
      await queryClient.invalidateQueries({ queryKey: qk.servers });
      queryClient.removeQueries({ queryKey: qk.server(server.id) });
    },
    onError: (error) => {
      setDeleting(null);
      setActionError(failureFromError(error));
    },
  });

  const autoExecMutation = useMutation({
    mutationFn: async (input: { server: Server; enabled: boolean }) => {
      const { error, response } = await api.PATCH('/api/servers/{id}', {
        params: { path: { id: input.server.id }, header: writeHeaders() },
        // 该路径不改接入协议，但服务端对终态 telnet 的任意 PATCH 都要求
        // 显式确认（契约 2.6 跨请求终态组合强制）——恒 false 会让 telnet
        // 网络设备的开关永远被 400 拒绝（三期评审），按终态传确认值。
        body: {
          auto_exec_enabled: input.enabled,
          confirm_telnet_cleartext:
            input.server.device_kind === 'network_device' && input.server.access_proto === 'telnet',
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onSuccess: async (_data, input) => {
      message.success(input.enabled ? '已开启自动执行' : '已关闭自动执行');
      await queryClient.invalidateQueries({ queryKey: qk.servers });
      await queryClient.invalidateQueries({ queryKey: qk.server(input.server.id) });
    },
    onError: (error) => {
      setActionError(failureFromError(error));
    },
  });

  const testMutation = useMutation({
    mutationFn: async (server: Server) => {
      const { data, error, response } = await api.POST('/api/servers/{id}/test', {
        params: { path: { id: server.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data, server) => {
      const latencyMs = data?.latency_ms ?? 0;
      const channel = serverChannelOf(server);
      const netdev = isNetworkDevice(server);
      setLastTests((previous) => ({ ...previous, [server.id]: { ok: true, latencyMs } }));
      setTestSubject(server);
      setTestError(null);
      setTestSuccess({
        latencyMs,
        lines: [
          {
            label: netdev ? '网络设备' : '服务器',
            value: netdev
              ? `${server.name}（${server.host}:${server.port} · ${netdevBrandLabel(server.netdev_brand)} · ${channel.label}）`
              : `${server.name}（${server.host}:${channel.kind === 'winrm' ? server.winrm_port : server.port}${channel.kind === 'winrm' ? ' · WinRM' : ''}）`,
          },
          {
            label: netdev ? '软件版本（品牌探针）' : '系统信息',
            value: data?.server_version ?? '—',
          },
          {
            label: '主机密钥指纹',
            // 契约 2.7/2.8：WinRM 与 Telnet 通道 fingerprint 恒为空串、
            // first_seen 恒 false（无主机密钥语义）。
            value: channel.hasHostKey
              ? (data?.fingerprint ?? '—')
              : `—（${channel.label} 通道无主机密钥语义）`,
          },
        ],
        note:
          channel.hasHostKey && data?.first_seen
            ? '首次连接：已记录主机密钥指纹并写入审计。'
            : null,
        // 契约 2.6「UI 必须显示明文风险提示」：telnet 接入的网络设备由
        // 后端在测试结果透传 cleartext_warning（三期评审：曾整链丢弃），
        // 英文单一事实源常量先经 localizeCleartextWarning 映射中文。
        warning: localizeCleartextWarning(data?.cleartext_warning),
      });
    },
    onError: (error, server) => {
      setLastTests((previous) => ({
        ...previous,
        [server.id]: { ok: false, latencyMs: 0 },
      }));
      setTestSubject(server);
      setTestSuccess(null);
      setTestError(localizeTestFailure(failureFromError(error)));
    },
  });

  const servers = serversQuery.data ?? [];
  const filtered = keyword.trim()
    ? servers.filter((server) => serverMatchesKeyword(server, keyword))
    : servers;

  const columns: TableColumnsType<Server> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      width: 190,
      render: (_value, record) => (
        <div>
          <div className="opsmind-admin-cell-title">
            {record.name}{' '}
            {isNetworkDevice(record) ? (
              <>
                <Tag color="cyan">网络设备</Tag>
                <Tooltip
                  title={`接入协议 ${record.access_proto.toUpperCase()}${record.has_enable_secret ? ' · 已配置特权密码' : ''}`}
                >
                  <Tag color={record.access_proto === 'telnet' ? 'volcano' : 'geekblue'}>
                    {netdevBrandLabel(record.netdev_brand)} · {record.access_proto.toUpperCase()}
                  </Tag>
                </Tooltip>
              </>
            ) : (
              <Tag color={record.os === 'windows' ? 'geekblue' : 'green'}>{osLabel(record.os)}</Tag>
            )}
          </div>
          {/* 契约 8.7 ①（FE-P2 六期）：服务器 ID 复制。 */}
          <div className="opsmind-admin-cell-sub">
            ID {shortId(record.id, 12)} <CopyButton text={record.id} label="复制服务器 ID" />
          </div>
          {record.device_kind === 'server' ? (
            <RuntimeBadge runtime={record.container_runtime} />
          ) : null}
        </div>
      ),
    },
    {
      title: '地址',
      key: 'address',
      width: 220,
      render: (_value, record) => (
        <div>
          {/* 契约 8.7 ①（FE-P2 六期）：主机与 host:port 组合串复制。 */}
          <div className="opsmind-admin-mono">
            {record.host}:{record.port}{' '}
            <CopyButton text={`${record.host}:${record.port}`} label="复制地址" />
          </div>
          <div className="opsmind-admin-cell-sub">{record.username}</div>
        </div>
      ),
    },
    {
      title: '标签',
      dataIndex: 'tags',
      key: 'tags',
      width: 200,
      render: (_value, record) => {
        if (record.tags.length === 0) {
          return <Typography.Text type="secondary">—</Typography.Text>;
        }
        const visible = record.tags.slice(0, 3);
        const rest = record.tags.slice(3);
        return (
          <Space size={[4, 4]} wrap>
            {visible.map((tag) => (
              <Tag key={tag}>{tag}</Tag>
            ))}
            {rest.length > 0 ? (
              <Tooltip title={rest.join('、')}>
                <Tag>+{rest.length}</Tag>
              </Tooltip>
            ) : null}
          </Space>
        );
      },
    },
    {
      title: '自动执行',
      key: 'auto_exec_enabled',
      width: 120,
      render: (_value, record) => (
        <Popconfirm
          title="开启自动执行？"
          description="模型将可直接执行被判定为安全的命令，不再逐条审批；风险命令仍需审批。"
          okText="开启"
          cancelText="取消"
          onConfirm={() => autoExecMutation.mutate({ server: record, enabled: true })}
          disabled={record.auto_exec_enabled}
        >
          <Switch
            size="small"
            checked={record.auto_exec_enabled}
            loading={
              autoExecMutation.isPending && autoExecMutation.variables?.server.id === record.id
            }
            onChange={(checked) => {
              if (!checked) {
                autoExecMutation.mutate({ server: record, enabled: false });
              }
            }}
          />
        </Popconfirm>
      ),
    },
    {
      title: '状态',
      key: 'status',
      width: 210,
      render: (_value, record) => {
        const lastTest = lastTests[record.id];
        // 契约 2.7/2.8：WinRM 与 Telnet 认证无主机密钥语义（不参与 TOFU），
        // 状态列只标识通道。
        const channel = serverChannelOf(record);
        return (
          <Space size={[4, 4]} wrap>
            {!channel.hasHostKey ? (
              <Tooltip title={`${channel.label} 通道没有主机密钥语义，不参与首次信任（TOFU）。`}>
                <Tag color={channel.kind === 'netdev_telnet' ? 'volcano' : 'purple'}>
                  {channel.label} 通道
                </Tag>
              </Tooltip>
            ) : record.host_key_fingerprint ? (
              <Tooltip
                title={`${record.host_key_fingerprint}${
                  record.host_key_verified_at
                    ? `（验证于 ${formatDateTime(record.host_key_verified_at)}）`
                    : ''
                }`}
              >
                <Tag color="green">已信任密钥</Tag>
              </Tooltip>
            ) : (
              <Tag color="gold">待首次连接</Tag>
            )}
            {lastTest ? (
              <Tag color={lastTest.ok ? 'green' : 'red'}>
                {lastTest.ok ? `测试通过 ${lastTest.latencyMs} ms` : '测试失败'}
              </Tag>
            ) : null}
          </Space>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      width: 250,
      fixed: 'right',
      render: (_value, record) => (
        <div className="opsmind-admin-actions">
          <ServerTestButton
            loading={testMutation.isPending && testMutation.variables?.id === record.id}
            onClick={() => testMutation.mutate(record)}
          />
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
        title="服务器"
        description="服务器与网络设备资产（SSH / WinRM / Telnet）、凭据与自动执行策略"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              loading={serversQuery.isFetching}
              onClick={() => void serversQuery.refetch()}
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
              新增服务器
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
          placeholder="搜索名称 / 地址 / 端口 / 登录用户 / 标签 / 品牌"
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
        />
        <Typography.Text type="secondary">共 {filtered.length} 台</Typography.Text>
      </Flex>

      {serversQuery.isPending ? <AdminLoading /> : null}
      {serversQuery.isError ? (
        <QueryErrorState error={serversQuery.error} onRetry={() => void serversQuery.refetch()} />
      ) : null}
      {serversQuery.isSuccess ? (
        <Table<Server>
          rowKey="id"
          columns={columns}
          dataSource={filtered}
          size="middle"
          scroll={{ x: 1120 }}
          locale={{
            emptyText: <EmptyState description="还没有服务器，点击右上角“新增服务器”开始配置" />,
          }}
          pagination={{
            pageSize: 10,
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50],
            showTotal: (total) => `共 ${total} 台`,
          }}
        />
      ) : null}

      <ServerFormModal
        key={editing?.id ?? 'new'}
        open={formOpen}
        server={editing}
        onClose={() => {
          setFormOpen(false);
          setEditing(null);
        }}
        onSaved={() => {
          message.success(editing ? '服务器已更新' : '服务器已创建');
          setFormOpen(false);
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: qk.servers });
          if (editing) {
            void queryClient.invalidateQueries({ queryKey: qk.server(editing.id) });
          }
        }}
      />

      <ConfirmDeleteModal
        open={deleting !== null}
        title={`删除服务器「${deleting?.name ?? ''}」？`}
        content="删除后该服务器的凭据与已信任主机密钥将被移除，此操作不可撤销。"
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
