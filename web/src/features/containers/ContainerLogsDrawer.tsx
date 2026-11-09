// C2 四期：容器日志抽屉（契约 8.7 容器页 / 5.2C logs 模板）。
// tail 选择 100/200/500/1000（缺省 200）、手动刷新、复制、按行渲染、
// truncated 截断提示。查询按需触发（打开才取），不自动轮询——契约 8.7
// 「container-logs 不失效（按需重取）」与容器页「不自动高频轮询」口径一致。

import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Drawer, Select, Space, Spin, Typography } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { useState, type CSSProperties } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { ApiRequestError, failureFromError, toApiFailure } from '../../components/admin/apiError';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import ErrorBanner from '../../components/ErrorBanner';
import { describeContainerFailure } from './containerFailures';
import { DEFAULT_TAIL, TAIL_CHOICES, normalizeTail, splitLogLines } from './logsView';

type Container = components['schemas']['Container'];

export interface ContainerLogsDrawerProps {
  serverId: string;
  container: Container | null;
  onClose: () => void;
}

const LOG_LINE_STYLE: CSSProperties = {
  fontFamily: "'JetBrains Mono','Fira Code',monospace",
  fontSize: 12,
  lineHeight: '20px',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-all',
};

export default function ContainerLogsDrawer({
  serverId,
  container,
  onClose,
}: ContainerLogsDrawerProps) {
  const [tail, setTail] = useState<number>(DEFAULT_TAIL);

  const open = container !== null;
  const logsQuery = useQuery({
    queryKey: qk.containerLogs(serverId, container?.id ?? '', tail),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers/{id}/containers/{cid}/logs', {
        params: {
          path: { id: serverId, cid: container?.id ?? '' },
          query: { tail: normalizeTail(tail) },
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    // 打开才取；tail 变化即换 key 重取。不设轮询。
    enabled: open,
  });

  if (!container) {
    return null;
  }

  const output = logsQuery.data?.output ?? '';
  const lines = splitLogLines(output);

  return (
    <Drawer
      open={open}
      width={720}
      title={`容器日志 · ${container.name !== '' ? container.name : container.id.slice(0, 12)}`}
      onClose={onClose}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Space wrap>
          <Typography.Text type="secondary">最近行数</Typography.Text>
          <Select
            style={{ width: 110 }}
            value={tail}
            aria-label="最近行数"
            options={TAIL_CHOICES.map((value) => ({
              value,
              label: `${value} 行`,
            }))}
            onChange={(value) => setTail(value)}
          />
          <Button
            icon={<ReloadOutlined />}
            loading={logsQuery.isFetching}
            onClick={() => void logsQuery.refetch()}
          >
            刷新
          </Button>
          {/* 六期打磨 sprint 2：日志复制改走全站统一 CopyButton（图标 +
              1.5s 冷却 + 两级剪贴板降级 + 可选中文本面板），删除手写
              handleCopy 与「日志已复制」局部 toast（契约 8.2/8.7 唯一实现）。 */}
          <CopyButton text={output} label="复制日志" size="middle" />
        </Space>

        {logsQuery.data?.truncated ? (
          <Alert
            type="warning"
            showIcon
            message="日志过长已截断（单次输出上限 256KB），仅显示末尾部分"
          />
        ) : null}

        {logsQuery.isFetching && output === '' ? (
          <div style={{ padding: '32px 0', textAlign: 'center' }}>
            <Spin />
          </div>
        ) : logsQuery.isError ? (
          <ErrorBanner
            code={failureFromError(logsQuery.error).code}
            message={describeContainerFailure(failureFromError(logsQuery.error))}
            requestId={failureFromError(logsQuery.error).requestId}
            onRetry={() => void logsQuery.refetch()}
          />
        ) : lines.length === 0 ? (
          <EmptyState description="该容器暂无日志输出" />
        ) : (
          <div
            style={{
              background: 'var(--color-bg-code)',
              borderRadius: 8,
              padding: '12px 16px',
              maxHeight: '70vh',
              overflow: 'auto',
            }}
          >
            {lines.map((line, index) => (
              // 保留空行（日志语义）；index 作 key——行内容可能重复。
              <div key={index} style={LOG_LINE_STYLE}>
                {line === '' ? '\u00A0' : line}
              </div>
            ))}
          </div>
        )}
      </Space>
    </Drawer>
  );
}
