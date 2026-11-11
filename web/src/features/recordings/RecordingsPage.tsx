// F1：录制页（/recordings）。八期 FE-R：导航与页头标题从「回放」改为
// 「录制」（契约 2.12 约定 1）——它是终端/RDP 会话录像；AI 会话的事件级
// 执行回放在 `/replays`（「回放执行」），两者语义不同不得同名。
// 任务定稿：终端录制可浏览器内回放（.cast），
// RDP 录制仅展示元数据与「下载 .guac」——浏览器内播放按契约明确标注
// v1 不支持（转码不在二期范围）。
//
// 列表列（任务）：服务器、用户、时长、大小、时间；下载走
// /api/term|rdp/recordings/{id}（下载前刷新列表，契约 8.4 失效规则）。

import { CloudDownloadOutlined, PlayCircleOutlined, ReloadOutlined } from '@ant-design/icons';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Space, Table, Tabs, Tooltip, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import ErrorBanner from '../../components/ErrorBanner';
import { AdminLoading } from '../../components/admin/AdminStates';
import { ApiRequestError, failureFromError, toApiFailure } from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import PageHeader from '../../components/PageHeader';
import { formatBytes, formatDurationClock, recordingDurationMs } from '../term/format';
import { describeTermFailure } from '../term/termCodes';
import { message } from '../../components/antdMessage';
import TermPlayer from './TermPlayer';
import '../../styles/admin.css';
import '../../styles/term.css';

type TerminalRecording = components['schemas']['TerminalRecording'];
type RdpRecording = components['schemas']['RdpRecording'];

/** F1 版查询错误态：错误码经 describeTermFailure 映射（RECORDING_* 等二期码）。 */
function RecordingsErrorState({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const failure = failureFromError(error);
  return (
    <ErrorBanner
      code={failure.code}
      message={describeTermFailure(failure)}
      requestId={failure.requestId}
      onRetry={onRetry}
    />
  );
}

function useTermRecordings() {
  return useQuery({
    queryKey: qk.termRecordings,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/term/recordings', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });
}

function useRdpRecordings() {
  return useQuery({
    queryKey: qk.rdpRecordings,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/rdp/recordings', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });
}

/**
 * 下载：fetch 拉取为 blob 后经临时 <a download> 触发保存；下载前刷新
 * 列表（契约 8.4）。失败（RECORDING_UNAVAILABLE 等 JSON 错误体）走
 * describeTermFailure 中文提示——此前的 window.location.assign 整页
 * 导航会把原始 JSON 错误体直接呈现给用户（评审发现）。
 */
function useDownloadRecording() {
  const queryClient = useQueryClient();
  return async (path: string, listKey: readonly unknown[], filename: string) => {
    await queryClient.invalidateQueries({ queryKey: listKey });
    try {
      const response = await fetch(path, { credentials: 'same-origin' });
      if (!response.ok) {
        let code = `HTTP_${response.status}`;
        let errorMessage = '';
        try {
          const body = (await response.json()) as { error?: { code?: string; message?: string } };
          code = body.error?.code ?? code;
          errorMessage = body.error?.message ?? '';
        } catch {
          // 非 JSON 错误体：退回 HTTP 状态码提示。
        }
        message.warning(
          `${describeTermFailure({
            code,
            message: errorMessage,
            requestId: '',
            details: null,
            status: null,
          })}（${code}）`,
        );
        return;
      }
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = filename;
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
      message.success('已开始下载');
    } catch {
      message.warning('网络异常，请检查连接后重试');
    }
  };
}

export default function RecordingsPage() {
  const queryClient = useQueryClient();
  const termQuery = useTermRecordings();
  const rdpQuery = useRdpRecordings();
  const download = useDownloadRecording();
  const [playing, setPlaying] = useState<TerminalRecording | null>(null);

  const termColumns: TableColumnsType<TerminalRecording> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string, recording) => (
        <Space direction="vertical" size={0}>
          <span className="opsmind-admin-cell-title">{name || '（已删除）'}</span>
          <span className="opsmind-admin-meta">
            {recording.os === 'windows' ? 'Windows OpenSSH' : 'Linux SSH'} · 由 {recording.username}{' '}
            发起
          </span>
        </Space>
      ),
    },
    {
      title: '时长',
      key: 'duration',
      width: 100,
      render: (_, recording) => {
        const ms = recordingDurationMs(recording);
        return ms === null ? '—' : formatDurationClock(ms);
      },
    },
    {
      title: '大小',
      dataIndex: 'bytes_written',
      width: 110,
      render: (bytes: number) => formatBytes(bytes),
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
      width: 170,
      render: (_, recording) => (
        <Space>
          <Button
            type="link"
            size="small"
            icon={<PlayCircleOutlined />}
            onClick={() => setPlaying(recording)}
          >
            回放
          </Button>
          <Button
            type="link"
            size="small"
            icon={<CloudDownloadOutlined />}
            onClick={() => {
              void download(
                `/api/term/recordings/${recording.id}`,
                qk.termRecordings,
                `${recording.id}.cast`,
              );
            }}
          >
            下载 .cast
          </Button>
        </Space>
      ),
    },
  ];

  const rdpColumns: TableColumnsType<RdpRecording> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string) => (
        <span className="opsmind-admin-cell-title">{name || '（已删除）'}</span>
      ),
    },
    {
      title: '大小',
      dataIndex: 'bytes_written',
      width: 110,
      render: (bytes: number) => formatBytes(bytes),
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
      title: '结束时间',
      dataIndex: 'ended_at',
      width: 170,
      render: (value: string) => (
        <span className="opsmind-admin-meta">{formatDateTime(value)}</span>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 130,
      render: (_, recording) => (
        <Tooltip title="浏览器内播放 v1 不支持，请下载后离线处理">
          <Button
            type="link"
            size="small"
            icon={<CloudDownloadOutlined />}
            onClick={() => {
              void download(
                `/api/rdp/recordings/${recording.id}`,
                qk.rdpRecordings,
                `${recording.id}.guac`,
              );
            }}
          >
            下载 .guac
          </Button>
        </Tooltip>
      ),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="录制"
        description="终端会话录制（asciinema v2）回放与下载；RDP 会话录制下载"
        extra={
          <Button
            icon={<ReloadOutlined />}
            onClick={() => {
              void queryClient.invalidateQueries({ queryKey: qk.termRecordings });
              void queryClient.invalidateQueries({ queryKey: qk.rdpRecordings });
            }}
          >
            刷新
          </Button>
        }
      />

      {playing ? (
        <Card size="small" style={{ marginBottom: 16 }} styles={{ body: { padding: 12 } }}>
          <TermPlayer
            key={playing.id}
            src={`/api/term/recordings/${playing.id}`}
            title={`${playing.server_name || '终端会话'} · ${formatDateTime(playing.started_at)}`}
            onClose={() => setPlaying(null)}
          />
        </Card>
      ) : null}

      <Card size="small">
        <Tabs
          defaultActiveKey="term"
          items={[
            {
              key: 'term',
              label: (
                <span data-testid="term-recordings-tab">
                  终端录制{termQuery.data ? `（${termQuery.data.length}）` : ''}
                </span>
              ),
              children: termQuery.isLoading ? (
                <AdminLoading />
              ) : termQuery.error ? (
                <RecordingsErrorState
                  error={termQuery.error}
                  onRetry={() =>
                    void queryClient.invalidateQueries({ queryKey: qk.termRecordings })
                  }
                />
              ) : (
                <Table
                  rowKey="id"
                  size="small"
                  columns={termColumns}
                  dataSource={termQuery.data ?? []}
                  pagination={{ pageSize: 10, hideOnSinglePage: true }}
                  locale={{ emptyText: '暂无终端录制' }}
                />
              ),
            },
            {
              key: 'rdp',
              label: (
                <span data-testid="rdp-recordings-tab">
                  RDP 录制{rdpQuery.data ? `（${rdpQuery.data.length}）` : ''}
                </span>
              ),
              children: (
                <>
                  <Alert
                    type="info"
                    showIcon
                    style={{ marginBottom: 12 }}
                    message="RDP 录像为 guacd 原生 .guac 格式，浏览器内播放 v1 不支持；请下载后离线转码/处理。"
                  />
                  {rdpQuery.isLoading ? (
                    <AdminLoading />
                  ) : rdpQuery.error ? (
                    <RecordingsErrorState
                      error={rdpQuery.error}
                      onRetry={() =>
                        void queryClient.invalidateQueries({ queryKey: qk.rdpRecordings })
                      }
                    />
                  ) : (
                    <Table
                      rowKey="id"
                      size="small"
                      columns={rdpColumns}
                      dataSource={rdpQuery.data ?? []}
                      pagination={{ pageSize: 10, hideOnSinglePage: true }}
                      locale={{ emptyText: '暂无 RDP 录制' }}
                    />
                  )}
                </>
              ),
            },
          ]}
        />
      </Card>

      <Typography.Paragraph type="secondary" style={{ marginTop: 12, fontSize: 12 }}>
        录制文件名为会话 UUID，不可预测；终端录制含完整输出流，RDP 录制由网关侧写入共享卷。
      </Typography.Paragraph>
    </div>
  );
}
