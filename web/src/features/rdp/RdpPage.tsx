// F1：RDP 桌面页（/rdp）。CONTRACT.md 8.1/8.7：
// - 创建时选 Windows 服务器与分辨率（预设 1280×800/1600×900/1920×1080，
//   dpi 固定 96）；会话进行中不提供 resize 控件（分辨率创建时固定）；
// - 画布由 guacamole-common-js 管理（Client + WebSocketTunnel 直连
//   /api/rdp/sessions/{id}/ws，不经 ws.ts；契约 6.5 guacamole 子协议、文本帧透传）；
// - 连接/断开状态显示；剪贴板（双向）与全屏按钮；空闲提示；
// - .guac 录制只在回放页展示元数据与下载，本页不做回放入口；
// - 卸载：client.disconnect（内部关 WS）与键鼠/display 全量清理。

import {
  DesktopOutlined,
  DisconnectOutlined,
  ExpandOutlined,
  ReloadOutlined,
} from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  Button,
  Card,
  Flex,
  Input,
  Modal,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { TableColumnsType } from 'antd';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Guacamole from 'guacamole-common-js';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import { message } from '../../components/antdMessage';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import PageHeader from '../../components/PageHeader';
import { formatBytes, formatDurationClock } from '../term/format';
import { markSessionEnded, SESSION_END_REFETCH_DELAY_MS } from '../term/sessionList';
import { describeTermFailure } from '../term/termCodes';
import {
  RdpSessionController,
  rdpWsUrl,
  type GuacamoleFactory,
  type RdpSessionState,
} from './rdpSession';
import '../../styles/admin.css';
import '../../styles/term.css';

type Server = components['schemas']['Server'];
type RdpSession = components['schemas']['RdpSession'];

/** 分辨率预设（契约 8.7；dpi 固定 96）。 */
const RESOLUTION_PRESETS = [
  { label: '1280×800', value: '1280x800' },
  { label: '1600×900', value: '1600x900' },
  { label: '1920×1080', value: '1920x1080' },
];

const RDP_DPI = 96;

/**
 * 创建会话失败的页内文案（对齐 TermPage 的 describeTermCreateFailure，
 * p2 sprint 4）：RDP_SESSION_EXISTS 场景化改写——断开后立刻点「新建会话」
 * 时旧会话还在异步收尾（rdpx bridge 停止+封盘+关行，日志
 * "bridge goroutines did not stop in time"，实测约 2.3s），通用文案
 * 「该服务器已有活动 RDP 会话（server already has an active rdp
 * session）」会让刚断开的用户困惑——补充收尾时序的解释；第 4 轮走查
 * medium 起服务端 409 不再消耗 grant，文案明确「本次授权未被消耗、
 * 无需重新申请」，避免运维误以为要再找审批人。
 * 其余错误码回落 describeTermFailure。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出供单测
export function describeRdpCreateFailure(failure: ApiFailure): string {
  if (failure.code === 'RDP_SESSION_EXISTS') {
    return '该服务器已有活动 RDP 会话；若您刚断开上一会话，通常数秒内完成收尾。本次授权未被消耗，关闭现有会话后可直接重试（无需重新申请）。';
  }
  return describeTermFailure(failure);
}

/**
 * 服务器下拉选项（p3 sprint 2）：网络设备（device_kind=network_device，
 * 即交换机/路由器，os 恒 'network'）直接过滤不渲染——契约 3.2 约定 6
 * 「RDP 对一切 network_device 返回 400 RDP_UNSUPPORTED」服务端已拦
 * （rdpx.Open 非 windows 即 ErrUnsupported），此前前端只按 os!=='windows'
 * 禁用，网络设备以禁用项混入「远程桌面」选择器，且「仅 Windows 服务器
 * 支持 RDP」的禁用提示对网络设备语义错位（它们不是桌面系统，谈不上
 * Windows 与否）。Linux 服务器保留禁用项：提示语对其准确，且能看到
 * 资产全貌。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出供单测
export function buildRdpServerOptions(servers: Server[]) {
  return servers
    .filter((server) => server.device_kind !== 'network_device')
    .map((server) =>
      server.os === 'windows'
        ? { value: server.id, label: `${server.name}（${server.host}）` }
        : {
            value: server.id,
            label: `${server.name}（${server.host}）`,
            disabled: true,
            title: '仅 Windows 服务器支持 RDP',
          },
    );
}

function parseResolution(value: string): { width: number; height: number } {
  const match = /^(\d+)x(\d+)$/.exec(value);
  const width = match ? Number(match[1]) : 1280;
  const height = match ? Number(match[2]) : 800;
  return { width, height };
}

/** 生产注入：guacamole-common-js 适配到 GuacamoleFactory。 */
const guacamoleFactory: GuacamoleFactory = {
  createTunnel: (wsUrl) => new Guacamole.WebSocketTunnel(wsUrl),
  createClient: (tunnel) => new Guacamole.Client(tunnel),
  createKeyboard: (element) => new Guacamole.Keyboard(element as unknown as Element),
  createMouse: (element) => new Guacamole.Mouse(element as unknown as Element),
};

export default function RdpPage() {
  const queryClient = useQueryClient();
  const [serverId, setServerId] = useState<string | null>(null);
  const [resolution, setResolution] = useState('1280x800');
  const [active, setActive] = useState<RdpSession | null>(null);
  const [state, setState] = useState<RdpSessionState | null>(null);
  const [createError, setCreateError] = useState<ApiFailure | null>(null);
  // 00012：JIT RDP grant（与终端同流程：申请 → 他人批准 → 单次消费）。
  const [grantApprovalId, setGrantApprovalId] = useState<string | null>(null);
  const [grantServerId, setGrantServerId] = useState<string | null>(null);
  const [clipboardOpen, setClipboardOpen] = useState(false);
  const [clipboardDraft, setClipboardDraft] = useState('');
  const stageRef = useRef<HTMLDivElement>(null);
  const controllerRef = useRef<RdpSessionController | null>(null);

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

  const sessionsQuery = useQuery({
    queryKey: qk.rdpSessions({}),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/rdp/sessions', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const windowsServers = useMemo(
    () => (serversQuery.data ?? []).filter((server) => server.os === 'windows'),
    [serversQuery.data],
  );

  // 未手动选择时默认第一台 Windows 服务器（render 期派生，不经 effect）。
  const effectiveServerId = serverId ?? windowsServers[0]?.id ?? null;

  const grantMutation = useMutation({
    mutationFn: async (targetServerId: string) => {
      const { data, error, response } = await api.POST('/api/rdp/grants', {
        params: { header: writeHeaders() },
        body: { server_id: targetServerId },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data, targetServerId) => {
      setGrantApprovalId(data?.approval?.id ?? null);
      setGrantServerId(targetServerId);
      message.info('RDP 授权已提交，等待另一位审批者批准。');
    },
    onError: (error) => setCreateError(failureFromError(error)),
  });

  const grantQuery = useQuery({
    queryKey: ['approvals', 'grant', 'rdp', grantApprovalId],
    enabled: grantApprovalId !== null,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status === 'pending' ? 3000 : false;
    },
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/approvals/{id}', {
        params: { path: { id: grantApprovalId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
  });

  const grantApproved = grantQuery.data?.status === 'approved';
  const grantRejected =
    grantQuery.data?.status === 'denied' || grantQuery.data?.status === 'expired';

  // 第 2 轮走查（low）：他人批准后审批队列里该行消失，发起人侧此前没有
  // 「你有 1 条已批准、30 分钟内有效的授权」入口。这里读 GET /api/rdp/grants
  // （只返回本人 approved + 未消费 + 未过期）。
  const availableGrantsQuery = useQuery({
    queryKey: ['rdp', 'available-grants'],
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/rdp/grants', {});
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    refetchInterval: 15_000,
  });
  const availableGrants = availableGrantsQuery.data ?? [];
  // 主「连接」按钮保持既有交互：只回传本页刚批准的那条（服务端在 id 为空
  // 时消费该服务器上最新一条已批准 grant）。授权提示卡另带显式 grant_id。
  const approvedGrantIdFor = (targetServerId: string | null): string | undefined =>
    targetServerId && grantApproved && grantServerId === targetServerId && grantApprovalId
      ? grantApprovalId
      : undefined;
  // 该服务器上最早到期的一条可用授权（后端已按 grant_expires_at 升序）。
  const earliestGrantIdFor = (targetServerId: string | null): string | undefined =>
    targetServerId
      ? availableGrants.find((grant) => grant.server_id === targetServerId)?.id
      : undefined;

  const createMutation = useMutation({
    mutationFn: async (target: {
      serverId: string;
      width: number;
      height: number;
      grantId?: string;
    }) => {
      const { data, error, response } = await api.POST('/api/rdp/sessions', {
        params: { header: writeHeaders() },
        body: {
          server_id: target.serverId,
          width: target.width,
          height: target.height,
          dpi: RDP_DPI,
          grant_id: target.grantId ?? '',
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: async (created) => {
      setCreateError(null);
      setState(null);
      setGrantApprovalId(null);
      setActive(created.session);
      await queryClient.invalidateQueries({ queryKey: qk.rdpSessions({}) });
    },
    onError: (error) => {
      const failure = failureFromError(error);
      setCreateError(failure);
      // 409 场景（对齐 TermPage）：断开后立刻「新建会话」时，服务端
      // bridge 停止+封盘+关行是异步收尾，旧会话仍占位（契约 5.14 同
      // 服务器单会话）。失效会话列表让用户看到占用中的行；文案
      // （describeRdpCreateFailure）引导稍候重试，不自动重发——无法
      // 区分占用者是自己残留还是他人会话，自动重试会对他人会话反复
      // 打请求。
      if (failure.code === 'RDP_SESSION_EXISTS') {
        void queryClient.invalidateQueries({ queryKey: qk.rdpSessions({}) });
      }
    },
  });

  // 会话视图生命周期：active 变化 → 新建 controller（连接、挂画布、桥输入），
  // 断开/卸载 → dispose（WS/键鼠/display 全清）。
  useEffect(() => {
    const stage = stageRef.current;
    if (!active || !stage) {
      return undefined;
    }
    const controller = new RdpSessionController({
      session: active,
      wsUrl: rdpWsUrl(active.id),
      mountCanvas: (canvas) => {
        stage.appendChild(canvas as unknown as HTMLElement);
      },
      keyboardElement: stage,
      factory: guacamoleFactory,
    });
    controllerRef.current = controller;
    const unsubscribe = controller.subscribe(setState);

    const observer = new ResizeObserver(() => {
      controller.applyScale(stage.clientWidth, stage.clientHeight);
    });
    observer.observe(stage);
    // 初次按容器尺寸缩放一次。
    controller.applyScale(stage.clientWidth, stage.clientHeight);

    return () => {
      observer.disconnect();
      unsubscribe();
      controller.dispose();
      controllerRef.current = null;
    };
  }, [active]);

  // 会话断开（任何原因）：乐观把列表中当前行置为已结束（页头已提示
  // 「连接已断开」，列表不再残留「进行中/录制中…」与之矛盾——p2
  // sprint 4），并延迟到服务端收尾完成后再失效会话/录制列表（契约
  // 8.4）拉取真实数据。断开瞬间 bridge 收尾未完成（实测约 2.3s），
  // 立即 refetch 拿回的仍是 active 行，会把乐观值盖回「进行中」。
  const ended = state !== null && (state.phase === 'disconnected' || state.phase === 'failed');
  useEffect(() => {
    if (!ended || !active) {
      return;
    }
    const endedAtIso = new Date().toISOString();
    queryClient.setQueryData<RdpSession[]>(qk.rdpSessions({}), (previous) =>
      previous ? markSessionEnded(previous, active.id, endedAtIso) : previous,
    );
    // fire-and-forget：页面卸载后 invalidate 只把 query 标记 stale（无
    // observer 不发请求），无害；而清掉定时器会丢掉录制列表的补偿刷新
    // （新建会话成功只 invalidate 会话列表）。
    window.setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: qk.rdpSessions({}) });
      void queryClient.invalidateQueries({ queryKey: qk.rdpRecordings });
    }, SESSION_END_REFETCH_DELAY_MS);
  }, [ended, active, queryClient]);

  const connectWith = (serverId: string, grantId: string | undefined) => {
    const { width, height } = parseResolution(resolution);
    createMutation.mutate({
      serverId,
      width,
      height,
      ...(grantId !== undefined ? { grantId } : {}),
    });
  };

  const handleConnect = () => {
    if (!effectiveServerId) {
      return;
    }
    connectWith(effectiveServerId, approvedGrantIdFor(effectiveServerId));
  };

  // 授权提示卡：显式消费该服务器上最早到期的那条 grant。
  const handleConnectWithGrant = (grantId: string | undefined) => {
    if (!effectiveServerId) {
      return;
    }
    connectWith(effectiveServerId, grantId);
  };

  const handleFullscreen = useCallback(() => {
    const stage = stageRef.current;
    if (!stage) {
      return;
    }
    if (document.fullscreenElement) {
      void document.exitFullscreen();
    } else {
      void stage.requestFullscreen();
    }
  }, []);

  const handleSendClipboard = () => {
    controllerRef.current?.sendClipboard(clipboardDraft);
    setClipboardOpen(false);
  };

  const serverOptions = useMemo(
    () => buildRdpServerOptions(serversQuery.data ?? []),
    [serversQuery.data],
  );

  const columns: TableColumnsType<RdpSession> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string) => (
        <span className="opsmind-admin-cell-title">{name || '（已删除）'}</span>
      ),
    },
    { title: '分辨率', width: 110, render: (_, session) => `${session.width}×${session.height}` },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      render: (status: string) =>
        status === 'active' ? <Tag color="processing">进行中</Tag> : <Tag>已结束</Tag>,
    },
    {
      title: '时长',
      key: 'duration',
      width: 110,
      render: (_, session) => {
        const start = new Date(session.started_at).getTime();
        const end = session.ended_at ? new Date(session.ended_at).getTime() : Date.now();
        return Number.isNaN(start) || Number.isNaN(end)
          ? '—'
          : formatDurationClock(Math.max(0, end - start));
      },
    },
    {
      title: '录制',
      key: 'recording',
      width: 120,
      render: (_, session) =>
        session.status === 'active'
          ? '录制中…'
          : session.has_recording
            ? formatBytes(session.bytes_written)
            : '—',
    },
    {
      title: '开始时间',
      dataIndex: 'started_at',
      width: 170,
      render: (value: string) => (
        <span className="opsmind-admin-meta">{formatDateTime(value)}</span>
      ),
    },
  ];

  const phaseText: Record<string, string> = {
    connecting: '正在连接网关…',
    waiting: '等待远端桌面响应…',
    connected: '已连接',
    disconnected: '连接已断开',
    failed: '连接失败',
  };

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="桌面"
        description="Windows 远程桌面网关（guacamole；全程录制为 .guac，回放页提供下载）"
        extra={
          <Button
            icon={<ReloadOutlined />}
            onClick={() => {
              void queryClient.invalidateQueries({ queryKey: qk.rdpSessions({}) });
              void queryClient.invalidateQueries({ queryKey: qk.servers });
            }}
          >
            刷新
          </Button>
        }
      />

      <Card size="small" title="连接桌面" style={{ marginBottom: 16 }}>
        <Flex vertical gap={8}>
          <Space wrap>
            <Select
              style={{ minWidth: 300 }}
              placeholder="选择 Windows 服务器"
              value={effectiveServerId ?? undefined}
              options={serverOptions}
              onChange={(value) => setServerId(value ?? null)}
              loading={serversQuery.isLoading}
              aria-label="选择 Windows 服务器"
              // 同 TermPage：资产量级无需虚拟化，避免底部选项缺 DOM
              //（无障碍/E2E 定位失效）。
              virtual={false}
            />
            <Select
              style={{ minWidth: 140 }}
              value={resolution}
              options={RESOLUTION_PRESETS}
              onChange={setResolution}
              disabled={Boolean(active)}
              aria-label="分辨率"
            />
            <Button
              type="primary"
              icon={<DesktopOutlined />}
              loading={createMutation.isPending}
              disabled={!effectiveServerId || Boolean(active)}
              onClick={handleConnect}
            >
              连接
            </Button>
          </Space>
          {active ? (
            <Alert
              type="info"
              showIcon
              message={`当前会话分辨率 ${active.width}×${active.height}（dpi ${active.dpi}）。RDP 不支持运行中调整分辨率；浏览器窗口变化由画面自动缩放适配。`}
            />
          ) : null}
          {createError ? (
            <Alert
              type="error"
              showIcon
              message={describeRdpCreateFailure(createError)}
              action={
                createError.code === 'TERMINAL_APPROVAL_REQUIRED' ? (
                  <Button
                    size="small"
                    loading={grantMutation.isPending}
                    disabled={!effectiveServerId}
                    onClick={() => effectiveServerId && grantMutation.mutate(effectiveServerId)}
                  >
                    申请 RDP 授权
                  </Button>
                ) : undefined
              }
            />
          ) : null}
          {grantApprovalId && !grantApproved && !grantRejected ? (
            <Alert type="info" showIcon message="RDP 授权审批中，批准后请点击「打开桌面」。" />
          ) : null}
          {grantRejected ? (
            <Alert type="warning" showIcon message="RDP 授权未获批准，请重新申请。" />
          ) : null}
          {/* 第 2 轮走查（low）：已批准、待消费的 RDP 授权在审批队列里不再
              可见，发起人侧需要明确入口。 */}
          {availableGrants.length > 0 ? (
            <Alert
              type="success"
              showIcon
              data-testid="rdp-available-grants"
              message={`你有 ${availableGrants.length} 条已批准、尚未使用的 RDP 授权`}
              description={`最早一条 ${formatDateTime(availableGrants[0]?.grant_expires_at ?? '')} 到期；点「使用该授权」即单次消费该服务器上的授权，用后失效。`}
              action={
                <Button
                  size="small"
                  type="primary"
                  icon={<DesktopOutlined />}
                  disabled={!effectiveServerId || Boolean(active)}
                  onClick={() =>
                    effectiveServerId && handleConnectWithGrant(earliestGrantIdFor(effectiveServerId))
                  }
                >
                  使用该授权
                </Button>
              }
            />
          ) : null}
        </Flex>
      </Card>

      {active ? (
        <Card size="small" style={{ marginBottom: 16 }} styles={{ body: { padding: 12 } }}>
          <div className="opsmind-rdp-shell">
            <div className="opsmind-term-header">
              <div className="opsmind-term-header-meta">
                <Typography.Text strong>{active.server_name || 'RDP 会话'}</Typography.Text>
                <Tag
                  color={
                    state?.phase === 'connected'
                      ? 'green'
                      : state?.phase === 'failed'
                        ? 'red'
                        : 'processing'
                  }
                >
                  {state ? (phaseText[state.phase] ?? state.phase) : '正在初始化…'}
                </Tag>
                {state?.phase === 'connected' ? <Tag color="green">● 录制中</Tag> : null}
                {state?.error ? (
                  <Typography.Text type="danger">{state.error}</Typography.Text>
                ) : null}
              </div>
              <Space>
                <Button
                  icon={<ExpandOutlined />}
                  disabled={state?.phase !== 'connected'}
                  onClick={handleFullscreen}
                >
                  全屏
                </Button>
                <Button
                  icon={<DisconnectOutlined />}
                  disabled={ended}
                  onClick={() => {
                    // 不在此 invalidate 会话列表：断开瞬间服务端还在收尾
                    // （约 2.3s），立即 refetch 拿回 active 行会盖掉下方
                    // ended effect 的乐观更新——统一由该 effect 处理。
                    controllerRef.current?.disconnectByUser();
                  }}
                >
                  断开
                </Button>
                <Button
                  type="primary"
                  icon={<ReloadOutlined />}
                  onClick={() => {
                    setActive(null);
                    setState(null);
                    if (effectiveServerId) {
                      const { width, height } = parseResolution(resolution);
                      createMutation.mutate({
                        serverId: effectiveServerId,
                        width,
                        height,
                        ...(grantApproved && grantServerId === effectiveServerId && grantApprovalId
                          ? { grantId: grantApprovalId }
                          : {}),
                      });
                    }
                  }}
                >
                  新建会话
                </Button>
              </Space>
            </div>

            {state?.phase === 'waiting' || state?.phase === 'connecting' ? (
              <div className="opsmind-rdp-idle-hint" role="status">
                <Typography.Text strong>正在建立 RDP 连接</Typography.Text>
                <span>经 guacd 网关握手并注入凭据，通常需要数秒。</span>
              </div>
            ) : null}
            {ended ? (
              <Alert
                type={state?.phase === 'failed' ? 'error' : 'warning'}
                showIcon
                message={
                  state?.phase === 'failed'
                    ? `连接失败：${state.error ?? '未知错误'}。请确认 guacd 已配置、服务器凭据可用后新建会话。`
                    : '连接已断开。录制将出现在「回放」页（仅下载）；如需继续操作请新建会话。'
                }
              />
            ) : null}

            <div
              ref={stageRef}
              className="opsmind-rdp-stage"
              tabIndex={0}
              data-testid="rdp-stage"
              aria-label="远程桌面画面"
            >
              {state?.phase === 'connected' ? (
                <div
                  className="opsmind-rdp-idle-hint"
                  style={{
                    position: 'absolute',
                    top: 8,
                    right: 8,
                    bottom: 'auto',
                    inset: 'auto',
                    left: 'auto',
                    background: 'var(--color-overlay-media-bg)',
                    padding: '4px 10px',
                    fontSize: 12,
                  }}
                  role="note"
                >
                  点击画面以启用键盘输入；空闲 30 分钟自动断开
                </div>
              ) : null}
            </div>

            <Space wrap>
              <Button onClick={() => setClipboardOpen(true)} disabled={ended}>
                剪贴板
              </Button>
              {state?.remoteClipboard !== null && state?.remoteClipboard !== undefined ? (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  远端剪贴板已同步（在剪贴板对话框查看/复制）
                </Typography.Text>
              ) : null}
            </Space>
          </div>
        </Card>
      ) : null}

      <Card size="small" title="会话记录">
        {sessionsQuery.isLoading ? (
          <AdminLoading />
        ) : sessionsQuery.error ? (
          <QueryErrorState
            error={sessionsQuery.error}
            onRetry={() => void queryClient.invalidateQueries({ queryKey: qk.rdpSessions({}) })}
          />
        ) : (
          <Table
            rowKey="id"
            size="small"
            columns={columns}
            dataSource={sessionsQuery.data ?? []}
            pagination={{ pageSize: 10, hideOnSinglePage: true }}
            locale={{ emptyText: '暂无 RDP 会话' }}
          />
        )}
      </Card>

      <Modal
        title="远程剪贴板"
        open={clipboardOpen}
        onCancel={() => setClipboardOpen(false)}
        onOk={handleSendClipboard}
        okText="发送到远端"
        cancelText="关闭"
      >
        <Flex vertical gap={8}>
          <Typography.Text type="secondary">
            发送：把下方文本写入远端剪贴板；接收：远端复制的内容会显示在此。
          </Typography.Text>
          <Input.TextArea
            rows={4}
            value={clipboardDraft}
            onChange={(event) => setClipboardDraft(event.target.value)}
            placeholder="输入要发送到远端桌面的文本"
            aria-label="剪贴板文本"
          />
          {state?.remoteClipboard ? (
            <Tooltip title="远端最近一次复制的内容">
              <Input.TextArea
                rows={3}
                value={state.remoteClipboard}
                readOnly
                aria-label="远端剪贴板内容"
              />
            </Tooltip>
          ) : null}
        </Flex>
      </Modal>
    </div>
  );
}
