// F1：终端页（CONTRACT.md 8.1 /term）。
// 服务器选择 → 创建会话 → xterm 实时终端；会话列表与接入。
// 仅 SSH 通道服务器可开终端（契约 2.7/8.7）：WinRM 认证（password_ntlm/
// password_basic）项禁用并提示；windows+SSH 可选但提示需内置 OpenSSH Server。

import { CodeOutlined, PlayCircleOutlined, ReloadOutlined } from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Flex, Select, Space, Table, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useEffect, useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import PageHeader from '../../components/PageHeader';
import '../../styles/admin.css';
import '../../styles/term.css';
import { telnetNetdevNotice, termChannelOf, termServerOptionLabel } from './serverOptions';
import TermLive from './TermLive';
import { describeTermFailure } from './termCodes';
import { markSessionEnded, BYTES_FINALIZING, SESSION_END_REFETCH_DELAY_MS } from './sessionList';
import { formatBytes, formatDurationClock } from './format';

type TerminalSession = components['schemas']['TerminalSession'];

/** 会话创建时的默认尺寸（TermLive 挂载后 fit 会按视口上报真实值）。 */
const DEFAULT_COLS = 120;
const DEFAULT_ROWS = 40;

// 通道判定与下拉文案（termChannelOf / termServerOptionLabel）三期 p3
// sprint 3 抽至 ./serverOptions 共享（ChatPage 复用同一口径，单测导入
// 路径已同步迁移）。

/** 会话时长（ms）；活动会话显示进行中。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出供单测
export function sessionDuration(session: TerminalSession): number | null {
  const started = new Date(session.started_at).getTime();
  if (Number.isNaN(started)) {
    return null;
  }
  const end = session.ended_at ? new Date(session.ended_at).getTime() : Date.now();
  if (Number.isNaN(end)) {
    return null;
  }
  return Math.max(0, end - started);
}

/**
 * 创建会话失败的页内文案。TERM_SESSION_EXISTS 场景化改写：断开后立刻
 * 点「新建会话」时旧会话还在异步收尾（封盘+关行，秒级），通用文案
 * 「该服务器已有活动终端会话」会让刚断开的用户困惑——补充收尾时序的
 * 解释并引导稍候重试；其余错误码回落 describeTermFailure。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出供单测
export function describeTermCreateFailure(failure: ApiFailure): string {
  if (failure.code === 'TERM_SESSION_EXISTS') {
    return '该服务器已有活动终端会话；若您刚断开上一会话，通常数秒内完成收尾，请稍候重试。';
  }
  return describeTermFailure(failure);
}

export default function TermPage() {
  const queryClient = useQueryClient();
  const [serverId, setServerId] = useState<string | null>(null);
  const [active, setActive] = useState<TerminalSession | null>(null);
  const [createError, setCreateError] = useState<ApiFailure | null>(null);
  // 「时长」列对进行中会话用 Date.now() 计算（sessionDuration），列表页
  // 本身不随数据失效刷新——加 1s 心跳驱动重渲染，active 会话的时长不再
  // 冻结在首次渲染值（评审发现；与页头时长的心跳一致）。
  const [, setNowTick] = useState(0);
  useEffect(() => {
    const timer = window.setInterval(() => setNowTick((v) => v + 1), 1000);
    return () => window.clearInterval(timer);
  }, []);

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
    queryKey: qk.termSessions({}),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/term/sessions', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const createMutation = useMutation({
    mutationFn: async (targetServerId: string) => {
      const { data, error, response } = await api.POST('/api/term/sessions', {
        params: { header: writeHeaders() },
        body: { server_id: targetServerId, cols: DEFAULT_COLS, rows: DEFAULT_ROWS },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: async (created) => {
      setCreateError(null);
      setActive(created.session);
      await queryClient.invalidateQueries({ queryKey: qk.termSessions({}) });
    },
    onError: (error) => {
      const failure = failureFromError(error);
      setCreateError(failure);
      // 409 场景：断开后立刻「新建会话」时，服务端封盘+关闭会话行是
      // 异步收尾，旧会话仍占位（契约 5.13 同服务器单会话）。失效会话
      // 列表让用户看到占用中的行；文案（describeTermCreateFailure）
      // 引导稍候重试，不自动重发——无法区分占用者是自己残留还是他人
      // 会话，自动重试会对他人会话反复打请求。
      if (failure.code === 'TERM_SESSION_EXISTS') {
        void queryClient.invalidateQueries({ queryKey: qk.termSessions({}) });
      }
    },
  });

  // 会话结束（closed 帧/异常断开/手动断开）：乐观把列表中该行置为已结束，
  // 并延迟到服务端收尾完成后再失效会话/录制列表（契约 8.4）。断开瞬间
  // 服务端封盘+关行还在异步收尾（秒级），立即 refetch 拿回的仍是 active
  // 行，列表「进行中/录制中…」会与页头「已结束」矛盾残留 2-3 秒（p2
  // sprint 4 实测，与 RDP 同形态）。
  const handleSessionEnded = (sessionId: string) => {
    const endedAtIso = new Date().toISOString();
    queryClient.setQueryData<TerminalSession[]>(qk.termSessions({}), (previous) =>
      previous ? markSessionEnded(previous, sessionId, endedAtIso) : previous,
    );
    // fire-and-forget：页面卸载后 invalidate 只把 query 标记 stale（无
    // observer 不发请求），无害。
    window.setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: qk.termSessions({}) });
      void queryClient.invalidateQueries({ queryKey: qk.termRecordings });
    }, SESSION_END_REFETCH_DELAY_MS);
  };

  // 「新建会话」：同一服务器重建（服务端校验同服务器仅一个活动会话，
  // 旧会话已 closed 才会放行）；无目标服务器时回到表单。
  const handleNewSession = () => {
    setActive(null);
    if (effectiveServerId) {
      createMutation.mutate(effectiveServerId);
    }
  };

  // 未手动选择时默认第一台可开终端的服务器（render 期派生，不经 effect）。
  const effectiveServerId =
    serverId ?? serversQuery.data?.find((server) => termChannelOf(server).ssh)?.id ?? null;
  const selectedServer = useMemo(
    () => serversQuery.data?.find((server) => server.id === effectiveServerId) ?? null,
    [serversQuery.data, effectiveServerId],
  );
  const selectedChannel = selectedServer ? termChannelOf(selectedServer) : null;

  // p3 sprint 5：存在 Telnet 接入的网络设备时，卡片常驻一条说明——此前
  // 该场景只有下拉 disabled 项的 hover title（点击零反馈），选中态警告
  // Alert 因 disabled 不可选而不可达（死代码，已删除）。
  const telnetNotice = useMemo(() => telnetNetdevNotice(serversQuery.data), [serversQuery.data]);

  const serverOptions = useMemo(
    () =>
      (serversQuery.data ?? []).map((server) => {
        const channel = termChannelOf(server);
        const option = {
          value: server.id,
          label: termServerOptionLabel(server),
        };
        if (channel.ssh) {
          return option;
        }
        if (channel.netdev) {
          // 契约 2.8 约定 6：Telnet 接入的网络设备 v1 无终端桥，
          // 服务端 400 TERM_UNSUPPORTED——入口禁用并说明。
          return {
            ...option,
            disabled: true,
            title:
              'Telnet 接入的网络设备暂不支持交互终端（v1 无 telnet 终端桥）；命令执行请走对话或命令预览',
          };
        }
        return {
          ...option,
          disabled: true,
          title:
            '交互终端仅支持 SSH 通道：该服务器使用 WinRM 认证（password_ntlm/password_basic），无 PTY 语义',
        };
      }),
    [serversQuery.data],
  );

  const columns: TableColumnsType<TerminalSession> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string, session) => (
        <Space direction="vertical" size={0}>
          <span className="opsmind-admin-cell-title">{name || '（已删除）'}</span>
          <span className="opsmind-admin-meta">
            {session.os === 'network'
              ? '网络设备 SSH'
              : session.os === 'windows'
                ? 'Windows OpenSSH'
                : 'Linux SSH'}{' '}
            · 由 {session.username} 发起
          </span>
        </Space>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (status: string, session) =>
        status === 'active' ? (
          <Tag color="processing">进行中</Tag>
        ) : (
          <Tag>{session.has_recording ? '已录制' : '已结束'}</Tag>
        ),
    },
    {
      title: '尺寸',
      dataIndex: 'cols',
      width: 96,
      render: (_, session) => `${session.cols}×${session.rows}`,
    },
    {
      title: '时长',
      key: 'duration',
      width: 110,
      render: (_, session) => {
        const ms = sessionDuration(session);
        return ms === null ? '—' : formatDurationClock(ms);
      },
    },
    {
      title: '录制大小',
      dataIndex: 'bytes_written',
      width: 110,
      render: (bytes: number, session) =>
        session.status === 'active'
          ? '录制中…'
          : // 乐观结束行（BYTES_FINALIZING）：封盘未完成，真实大小未知——
            // 显示占位而非误导性的「0 B」（p2 sprint 5），延迟 invalidate
            // 拉回真实值后自然衔接。
            bytes === BYTES_FINALIZING
            ? '封盘中…'
            : formatBytes(bytes),
    },
    {
      title: '开始时间',
      dataIndex: 'started_at',
      width: 170,
      render: (value: string) => (
        <span className="opsmind-admin-meta">{formatDateTime(value)}</span>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 96,
      render: (_, session) =>
        session.status === 'active' ? (
          <Button
            type="link"
            size="small"
            icon={<PlayCircleOutlined />}
            onClick={() => setActive(session)}
          >
            接入
          </Button>
        ) : (
          <Typography.Text type="secondary">—</Typography.Text>
        ),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="终端"
        description="堡垒机式交互终端（SSH 通道：Linux / Windows OpenSSH / 网络设备；全程录制，可在回放页重放）"
        extra={
          <Button
            icon={<ReloadOutlined />}
            onClick={() => {
              void queryClient.invalidateQueries({ queryKey: qk.termSessions({}) });
              void queryClient.invalidateQueries({ queryKey: qk.servers });
            }}
          >
            刷新
          </Button>
        }
      />

      <Card size="small" title="打开终端" style={{ marginBottom: 16 }}>
        <Flex vertical gap={8}>
          <Space wrap>
            <Select
              style={{ minWidth: 320 }}
              placeholder="选择服务器 / 网络设备"
              value={effectiveServerId ?? undefined}
              options={serverOptions}
              onChange={(value) => setServerId(value ?? null)}
              loading={serversQuery.isLoading}
              aria-label="选择服务器"
              // 关闭虚拟滚动：资产量级（个位~百台）无需虚拟化；虚拟列表只
              // 渲染可视区选项，DOM 缺失会让禁用项的 title 提示与无障碍/
              // E2E 对底部选项的定位失效（全量并行跑实测：10 台资产时末位
              // 选项不在 DOM）。
              virtual={false}
            />
            <Button
              type="primary"
              icon={<CodeOutlined />}
              loading={createMutation.isPending}
              disabled={!selectedServer || !selectedChannel?.ssh}
              onClick={() => effectiveServerId && createMutation.mutate(effectiveServerId)}
            >
              打开终端
            </Button>
          </Space>
          {telnetNotice ? <Alert type="info" showIcon message={telnetNotice} /> : null}
          {selectedChannel?.ssh && selectedChannel.windows ? (
            <Alert
              type="info"
              showIcon
              message="Windows 服务器经 OpenSSH 通道提供交互终端，需服务器已安装内置 OpenSSH Server（Server 2019/Win10 1809+ 自带）。"
            />
          ) : null}
          {selectedChannel?.ssh && selectedChannel.netdev ? (
            <Alert
              type="info"
              showIcon
              message="网络设备经 SSH 通道提供交互式 CLI 终端（提示符驱动的行协议），全程录制；Telnet 接入的设备不支持终端。"
            />
          ) : null}
          {selectedServer && !selectedChannel?.ssh ? (
            <Alert
              type="warning"
              showIcon
              message="交互终端仅支持 SSH 通道：该服务器使用 WinRM 认证（NTLM/Basic），无 PTY 语义。"
            />
          ) : null}
          {createError ? (
            <Alert type="error" showIcon message={describeTermCreateFailure(createError)} />
          ) : null}
        </Flex>
      </Card>

      {active ? (
        <Card size="small" style={{ marginBottom: 16 }} styles={{ body: { padding: 12 } }}>
          <TermLive
            key={active.id}
            session={active}
            onNewSession={handleNewSession}
            onSessionEnded={() => handleSessionEnded(active.id)}
          />
        </Card>
      ) : null}

      <Card size="small" title="会话记录">
        {sessionsQuery.isLoading ? (
          <AdminLoading />
        ) : sessionsQuery.error ? (
          <QueryErrorState
            error={sessionsQuery.error}
            onRetry={() => void queryClient.invalidateQueries({ queryKey: qk.termSessions({}) })}
          />
        ) : (
          <Table
            rowKey="id"
            size="small"
            columns={columns}
            dataSource={sessionsQuery.data ?? []}
            pagination={{ pageSize: 10, hideOnSinglePage: true }}
            locale={{ emptyText: '暂无终端会话' }}
          />
        )}
      </Card>
    </div>
  );
}
