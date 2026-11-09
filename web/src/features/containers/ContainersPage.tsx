// C2 四期：容器管理页（CONTRACT.md 8.1 /containers、8.7 容器页）。
// 服务器选择（仅 device_kind=server；none/containerd 禁用并提示）→
// 容器表格（状态徽标/镜像/端口/创建时间）→ 行操作 start/stop/restart 走
// console 审批二段确认（确认弹窗 → 审批卡 → decide 结果回填）→ 日志抽屉
// （tail/刷新/复制）→ 镜像 Tab。加载/空/错误三态齐全；列表不自动轮询
// （手动刷新 + 15s staleTime，不自动高频轮询 stats）；401 经 queryClient
// 全局跳登录（lib/queryClient.ts）。

import {
  CaretRightOutlined,
  FileTextOutlined,
  PoweroffOutlined,
  RedoOutlined,
  ReloadOutlined,
} from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  Button,
  Card,
  Flex,
  Select,
  Space,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { TableColumnsType } from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  describeFailure,
  failureFromError,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import { formatDateTime, shortId } from '../../components/admin/format';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import ErrorBanner from '../../components/ErrorBanner';
import PageHeader from '../../components/PageHeader';
import { formatBytes } from '../term/format';
import '../../styles/admin.css';
import ContainerActionModal, { type ContainerActionTarget } from './ContainerActionModal';
import ContainerLogsDrawer from './ContainerLogsDrawer';
import { describeContainerFailure } from './containerFailures';
import { containerStateColor } from './runtimeBadge';
import RuntimeBadge from './RuntimeBadge';
import { containerServerOptions, pickDefaultServer } from './serverPicker';

type Container = components['schemas']['Container'];
type ContainerImage = components['schemas']['ContainerImage'];
type ContainerAction = components['schemas']['ContainerActionRequest']['action'];
type Server = components['schemas']['Server'];

export default function ContainersPage() {
  const queryClient = useQueryClient();
  const [serverId, setServerId] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<'containers' | 'images'>('containers');
  const [logsTarget, setLogsTarget] = useState<Container | null>(null);
  const [actionTarget, setActionTarget] = useState<ContainerActionTarget | null>(null);

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

  const servers = serversQuery.data;
  const serverOptions = useMemo(() => containerServerOptions(servers ?? []), [servers]);
  const serverList = servers ?? [];
  // 未手动选择时默认挑一台「可管理或可探测」的 server 类资产（render 期派生，
  // 不经 effect）。七期走查：此前取列表第一台且不看 runtime——种子/演示数据的
  // 第一台常是 none，进页即 400 CONTAINER_UNSUPPORTED + 红色横幅。
  const effectiveServerId = serverId ?? pickDefaultServer(serverList) ?? null;
  const selectedServer = serverList.find((server) => server.id === effectiveServerId) ?? null;
  // 无可自动选中的服务器时，仍给「重新探测」留出口：runtime 已确认不可管理的
  // 第一台 server 类资产用于承载该按钮（选中它本身不会发起任何容器请求）。
  const fallbackUnsupportedServer =
    serverList.find(
      (server) =>
        server.device_kind === 'server' &&
        (server.container_runtime === 'none' || server.container_runtime === 'containerd'),
    ) ?? null;
  const reprobeTargetId = effectiveServerId ?? fallbackUnsupportedServer?.id ?? null;

  // 容器列表：selectedServer 存在即可查——runtime='' 时后端先同步探测一次
  // 并写回（契约 2.9 约定 1），故不按 runtime 前置禁用；不自动轮询。
  const containersQuery = useQuery({
    queryKey: qk.containers(effectiveServerId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers/{id}/containers', {
        params: { path: { id: effectiveServerId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: selectedServer !== null,
  });

  const imagesQuery = useQuery({
    queryKey: qk.serverImages(effectiveServerId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers/{id}/images', {
        params: { path: { id: effectiveServerId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    // 镜像 Tab 选中才取。
    enabled: selectedServer !== null && activeTab === 'images',
  });

  // 探测写回失效去重（见下方 effect）：记录最近一次因 runtime 不一致触发
  // 失效的 (serverId, runtime)。
  const runtimeSyncRef = useRef<{ serverId: string; runtime: string } | null>(null);

  // 列表/镜像请求返回 CONTAINER_UNSUPPORTED（network_device 防御 / 探测写回
  // none/containerd）时失效 servers——下拉徽标与禁用态随探测结果更新。
  // 探测成功写回（''→docker/podman）同样失效：库值缓存不刷新会让页头
  // 「运行时」徽标停留「未探测」、操作确认弹窗预览丢运行时前缀（评审
  // 发现预览曾拼出 " stop <cid>"，与批准后逐字节执行的 docker stop 不一致）。
  // 副作用放 effect，不在 render 期触发。
  useEffect(() => {
    const codes = [containersQuery.error, imagesQuery.error]
      .filter((error) => error !== null)
      .map((error) => failureFromError(error).code);
    if (codes.includes('CONTAINER_UNSUPPORTED')) {
      void queryClient.invalidateQueries({ queryKey: qk.servers });
      return;
    }
    // 列表成功且 runtime 非空（后端 200 时只可能 docker/podman，探测已
    // 写回）而库值缓存不一致 → 失效 servers 拉回真实状态。
    const observed = containersQuery.data?.runtime;
    if (
      observed !== undefined &&
      observed !== '' &&
      selectedServer !== null &&
      observed !== selectedServer.container_runtime
    ) {
      // 去重：同一 (serverId, runtime) 只失效一次——后端写回失败会忽略
      // 错误（containers_handlers.go），库值恒 '' 时不去重会陷入
      // 失效-重取循环。
      const last = runtimeSyncRef.current;
      if (last?.serverId !== selectedServer.id || last.runtime !== observed) {
        runtimeSyncRef.current = { serverId: selectedServer.id, runtime: observed };
        void queryClient.invalidateQueries({ queryKey: qk.servers });
      }
    }
  }, [containersQuery.error, containersQuery.data, imagesQuery.error, selectedServer, queryClient]);

  // p4 sprint 4：库值 none/containerd 的「重新探测」改走真实探测——POST
  // test（连接测试顺带探测写回，servers.go probeAndWriteContainerRuntime）。
  // 容器接口对库值 none 直接 400 不再探测（containers_handlers.go 仅对 ''
  // 探测），此前按钮只 refetch 列表，点击后永远无变化（walk8）。成功后失效
  // servers 读最新库值：翻为 docker/podman 则失效列表/镜像展示数据；仍无
  // 受支持运行时给出明确反馈（不再静默）。
  const [reprobeFailure, setReprobeFailure] = useState<ApiFailure | null>(null);
  const [reprobeNote, setReprobeNote] = useState<string | null>(null);
  const reprobeMutation = useMutation({
    mutationFn: async (serverId: string) => {
      const { data, error, response } = await api.POST('/api/servers/{id}/test', {
        params: { path: { id: serverId }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: async (_data, serverId) => {
      setReprobeFailure(null);
      setReprobeNote(null);
      // 失效并等待 servers 重取完成，读探测写回后的最新库值。
      await queryClient.invalidateQueries({ queryKey: qk.servers });
      const items = queryClient.getQueryData<Server[]>(qk.servers) ?? [];
      const runtimeNow = items.find((server) => server.id === serverId)?.container_runtime ?? '';
      if (runtimeNow === 'docker' || runtimeNow === 'podman') {
        void queryClient.invalidateQueries({ queryKey: qk.containers(serverId) });
        void queryClient.invalidateQueries({ queryKey: qk.serverImages(serverId) });
        return;
      }
      setReprobeNote('已重新测试连接并探测运行时：仍未检测到受支持的容器运行时（docker/podman）');
    },
    onError: (error) => {
      setReprobeFailure(failureFromError(error));
    },
  });

  // decide 完成（含拒绝）：失效容器列表/详情与 approvals 相关键
  // （契约 8.4 四期失效规则；container-logs 不失效）。console 审批无
  // conversation 维度，approvals 按前缀整体失效。
  const handleDecided = (target: ContainerActionTarget) => {
    void queryClient.invalidateQueries({ queryKey: qk.containers(target.serverId) });
    void queryClient.invalidateQueries({
      queryKey: qk.container(target.serverId, target.containerId),
    });
    void queryClient.invalidateQueries({ queryKey: ['approvals'] });
  };

  const runtime = selectedServer?.container_runtime ?? '';
  // runtime 已知为 none/containerd：直接空态说明，不发请求（下拉本应禁用，
  // 此处兜底选中后数据变化的场景）。列表 400 时以错误码呈现同文案。
  const unsupportedKnown = runtime === 'none' || runtime === 'containerd';
  const unsupportedText =
    runtime === 'containerd'
      ? '该服务器仅探测到 containerd（调试用途，不支持容器管理面）；仅 docker/podman 支持容器管理'
      : '该服务器未探测到受支持的容器运行时（docker/podman）';

  const openAction = (container: Container, action: ContainerAction) => {
    if (!selectedServer) {
      return;
    }
    setActionTarget({
      serverId: selectedServer.id,
      serverName: selectedServer.name,
      runtime,
      containerId: container.id,
      containerName: container.name,
      action,
      // 六期打磨 sprint 6：confirm 态分流文案需要服务器级 auto_exec 开关
      //（与全局门限共同决定是否真的建审批）。
      autoExecEnabled: selectedServer.auto_exec_enabled,
    });
  };

  const containerColumns: TableColumnsType<Container> = [
    {
      title: 'ID',
      dataIndex: 'id',
      key: 'id',
      width: 130,
      render: (value: string) => (
        <Tooltip title={value}>
          {/* 契约 8.7 ⑤（FE-P2 六期）：容器 ID 复制（Tooltip 全值）。 */}
          <span className="opsmind-admin-mono">
            {shortId(value, 12)} <CopyButton text={value} label="复制容器 ID" />
          </span>
        </Tooltip>
      ),
    },
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      width: 180,
      render: (value: string, record) => (
        <Space size={6}>
          <span className="opsmind-admin-cell-title">
            {value !== '' ? value : '—'} {/* 契约 8.7 ⑤（FE-P2 六期）：容器名复制。 */}
            {value !== '' ? <CopyButton text={value} label="复制容器名" /> : null}
          </span>
          {record.status !== '' ? (
            <Tooltip title={record.status}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {record.status}
              </Typography.Text>
            </Tooltip>
          ) : null}
        </Space>
      ),
    },
    {
      title: '镜像',
      dataIndex: 'image',
      key: 'image',
      render: (value: string) => (
        <span className="opsmind-admin-mono">
          {value || '—'} {/* 契约 8.7 ⑤（FE-P2 六期）：镜像名复制。 */}
          {value !== '' ? <CopyButton text={value} label="复制镜像" /> : null}
        </span>
      ),
    },
    {
      title: '状态',
      dataIndex: 'state',
      key: 'state',
      width: 110,
      render: (value: string) => <Tag color={containerStateColor(value)}>{value || '—'}</Tag>,
    },
    {
      title: '端口',
      dataIndex: 'ports',
      key: 'ports',
      width: 220,
      render: (value: string[]) =>
        value.length === 0 ? (
          <Typography.Text type="secondary">—</Typography.Text>
        ) : (
          <Space size={[4, 4]} wrap>
            {value.map((port) => (
              <Tooltip key={port} title={port}>
                <span className="opsmind-admin-mono" style={{ fontSize: 12 }}>
                  {port}
                </span>
              </Tooltip>
            ))}
          </Space>
        ),
    },
    {
      title: '创建时间',
      dataIndex: 'created',
      key: 'created',
      width: 170,
      render: (value: string) => (
        <span className="opsmind-admin-meta">{value === '' ? '—' : formatDateTime(value)}</span>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 230,
      fixed: 'right',
      render: (_value, record) => (
        <div className="opsmind-admin-actions">
          <Button
            type="link"
            size="small"
            icon={<FileTextOutlined />}
            onClick={() => setLogsTarget(record)}
          >
            日志
          </Button>
          <Button
            type="link"
            size="small"
            icon={<CaretRightOutlined />}
            onClick={() => openAction(record, 'start')}
          >
            启动
          </Button>
          {/* p4 sprint 4：已停止（exited）容器 stop 注定失败（docker CLI 对
              未运行容器 exit≠0 → 审批批准后 CONTAINER_CLI_FAILED，浪费一轮
              审批）——按状态禁用并说明；启动/重启对 exited 合法（restart 对
              已停止容器等价 start）。span 包裹使 Tooltip 在 disabled 按钮上
              仍可悬浮触发。 */}
          <Tooltip title={record.state === 'exited' ? '容器已停止，无需停止' : null}>
            <span>
              <Button
                type="link"
                size="small"
                danger
                disabled={record.state === 'exited'}
                icon={<PoweroffOutlined />}
                onClick={() => openAction(record, 'stop')}
              >
                停止
              </Button>
            </span>
          </Tooltip>
          <Button
            type="link"
            size="small"
            icon={<RedoOutlined />}
            onClick={() => openAction(record, 'restart')}
          >
            重启
          </Button>
        </div>
      ),
    },
  ];

  const imageColumns: TableColumnsType<ContainerImage> = [
    {
      title: '仓库',
      dataIndex: 'repo',
      key: 'repo',
      render: (value: string, record) => (
        <span className="opsmind-admin-mono">
          {value || '—'} {/* 契约 8.7 ⑤（FE-P2 六期）：镜像 repo:tag 组合串复制。 */}
          {value !== '' ? (
            <CopyButton
              text={record.tag !== '' ? `${value}:${record.tag}` : value}
              label="复制镜像"
            />
          ) : null}
        </span>
      ),
    },
    {
      title: '标签',
      dataIndex: 'tag',
      key: 'tag',
      width: 160,
      render: (value: string) => <Tag>{value || '—'}</Tag>,
    },
    {
      title: '大小',
      dataIndex: 'size_bytes',
      key: 'size_bytes',
      width: 120,
      // 1000 进制：size_bytes 由后端按 docker CLI 的 SI human size 解析
      //（parseHumanSize），与 docker images 输出数字保持一致。
      render: (value: number) => formatBytes(value, 1000),
    },
    {
      title: '创建时间',
      dataIndex: 'created',
      key: 'created',
      width: 170,
      render: (value: string) => (
        <span className="opsmind-admin-meta">{value === '' ? '—' : formatDateTime(value)}</span>
      ),
    },
  ];

  // 列表/镜像错误态：CONTAINER_* 错误码本地中文映射（apiError.ts 归 FE2
  // 无容器码，先例同 ServersPage 的 NETCLI_* 本地映射）。传库值 runtime：
  // 探测通道错误（未写回，库值仍 ''）时横幅改「本次探测失败」文案，避免
  // 断言与库中状态矛盾的「当前运行时：none」（见 describeContainerFailure）。
  const renderListError = (error: unknown, onRetry: () => void) => {
    const failure = failureFromError(error);
    return (
      <ErrorBanner
        code={failure.code}
        message={describeContainerFailure(failure, runtime)}
        requestId={failure.requestId}
        onRetry={onRetry}
      />
    );
  };

  const renderUnsupported = (text: string) => (
    <EmptyState description={text}>
      <Space direction="vertical" size={12}>
        {reprobeNote ? <Alert type="info" showIcon message={reprobeNote} /> : null}
        {reprobeFailure ? (
          <ErrorBanner
            code={reprobeFailure.code}
            message={describeFailure(reprobeFailure)}
            requestId={reprobeFailure.requestId}
            onRetry={() => {
              if (reprobeTargetId !== null) {
                reprobeMutation.mutate(reprobeTargetId);
              }
            }}
          />
        ) : null}
        {/* p4 sprint 4：POST test 是唯一真实重探入口（顺带探测写回），不再
            refetch 列表——后端对库值 none 不再探测。 */}
        <Button
          loading={reprobeMutation.isPending}
          disabled={reprobeTargetId === null}
          onClick={() => {
            if (reprobeTargetId !== null) {
              reprobeMutation.mutate(reprobeTargetId);
            }
          }}
        >
          重新探测
        </Button>
        <Typography.Text type="secondary">
          重新测试服务器连接并探测容器运行时（docker/podman）
        </Typography.Text>
      </Space>
    </EmptyState>
  );

  const containersPane = unsupportedKnown ? (
    renderUnsupported(unsupportedText)
  ) : containersQuery.isPending && effectiveServerId !== null ? (
    <AdminLoading />
  ) : containersQuery.isError ? (
    renderListError(containersQuery.error, () => void containersQuery.refetch())
  ) : (
    <Table<Container>
      rowKey="id"
      size="middle"
      columns={containerColumns}
      dataSource={containersQuery.data?.items ?? []}
      scroll={{ x: 1180 }}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      locale={{
        emptyText: <EmptyState description="该服务器暂无容器" />,
      }}
    />
  );

  const imagesPane = unsupportedKnown ? (
    renderUnsupported(unsupportedText)
  ) : imagesQuery.isPending ? (
    <AdminLoading />
  ) : imagesQuery.isError ? (
    renderListError(imagesQuery.error, () => void imagesQuery.refetch())
  ) : (
    <Table<ContainerImage>
      rowKey={(image) => `${image.repo}:${image.tag}`}
      size="middle"
      columns={imageColumns}
      dataSource={imagesQuery.data?.items ?? []}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      locale={{ emptyText: <EmptyState description="该服务器暂无镜像" /> }}
    />
  );

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="容器"
        description="服务器容器与镜像（docker/podman 运行时，经 SSH/WinRM 通道实时查询；操作需审批）"
        extra={
          <Button
            icon={<ReloadOutlined />}
            loading={containersQuery.isFetching || serversQuery.isFetching}
            onClick={() => {
              void containersQuery.refetch();
              if (activeTab === 'images') {
                void imagesQuery.refetch();
              }
              void queryClient.invalidateQueries({ queryKey: qk.servers });
            }}
          >
            刷新
          </Button>
        }
      />

      <Card size="small" style={{ marginBottom: 16 }}>
        <Flex vertical gap={8}>
          <Space wrap>
            <Typography.Text type="secondary">服务器</Typography.Text>
            <Select
              style={{ minWidth: 340 }}
              placeholder="选择服务器"
              value={effectiveServerId ?? undefined}
              loading={serversQuery.isLoading}
              aria-label="选择服务器"
              optionRender={(option) => (
                <Space size={6}>
                  <span>{option.label}</span>
                  <RuntimeBadge runtime={option.data.runtime} showUnknown />
                </Space>
              )}
              options={serverOptions.map((option) => ({
                value: option.value,
                label: option.label,
                runtime: option.runtime,
                disabled: option.disabled,
                // 禁用原因经 title 提示（无容器运行时的服务器禁用并提示）。
                ...(option.title !== null ? { title: option.title } : {}),
              }))}
              onChange={(value) => setServerId(value ?? null)}
            />
            {selectedServer ? (
              <Space size={6}>
                <Typography.Text type="secondary">运行时</Typography.Text>
                <RuntimeBadge runtime={runtime} showUnknown />
              </Space>
            ) : null}
          </Space>
          {/* 探测失败（isError）后隐藏：探测已结束，错误横幅会给出结果，
              「正在自动探测一次」的进行时提示与真实状态不符且永久滞留。 */}
          {runtime === '' && selectedServer && !containersQuery.isError ? (
            <Alert
              type="info"
              showIcon
              message="尚未探测容器运行时：正在自动探测一次（探测失败按无容器处理）"
            />
          ) : null}
          {serversQuery.data && serverOptions.length === 0 ? (
            <Alert
              type="info"
              showIcon
              message="还没有可管理容器的服务器资产；容器是服务器能力，请先在「服务器」页添加 server 类资产"
            />
          ) : null}
        </Flex>
      </Card>

      {selectedServer === null ? (
        serversQuery.isError ? (
          <QueryErrorState error={serversQuery.error} onRetry={() => void serversQuery.refetch()} />
        ) : serverOptions.length > 0 ? (
          // 七期走查：有 server 类资产但运行时全部为 none/containerd——不再
          // 替用户选第一台并发注定 400 的请求（此前整条红色
          // CONTAINER_UNSUPPORTED）。说明原因并保留「重新探测」入口（真实
          // 探测走 POST test，仍是唯一能把库值翻成 docker/podman 的通道）。
          renderUnsupported(
            '请先选择支持容器运行时的服务器：当前 server 类资产的运行时均不可管理（none/containerd）',
          )
        ) : (
          <EmptyState description="选择一台服务器查看容器与镜像" />
        )
      ) : (
        <Card size="small">
          <Tabs
            activeKey={activeTab}
            onChange={(key) => setActiveTab(key === 'images' ? 'images' : 'containers')}
            items={[
              { key: 'containers', label: '容器', children: containersPane },
              { key: 'images', label: '镜像', children: imagesPane },
            ]}
          />
        </Card>
      )}

      <ContainerLogsDrawer
        serverId={effectiveServerId ?? ''}
        container={logsTarget}
        onClose={() => setLogsTarget(null)}
      />

      <ContainerActionModal
        key={actionTarget ? `${actionTarget.containerId}:${actionTarget.action}` : 'closed'}
        target={actionTarget}
        onClose={() => setActionTarget(null)}
        onDecided={handleDecided}
      />
    </div>
  );
}
