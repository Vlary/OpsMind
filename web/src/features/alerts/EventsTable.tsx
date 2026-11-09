// M4 五期：告警事件列表（CONTRACT.md 8.7——firing/resolved 过滤、severity
// Tag、触发值、notify_status；行操作「诊断」）。轮询（8.4）：有 firing 15s、
// 无 firing 60s。before_id 分页加载更多。

import { useQuery } from '@tanstack/react-query';
import { Button, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import { formatBps } from '../monitor/chartMath';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';

import { METRIC_LABELS, ruleDisplayName, SEVERITY_LABELS } from './ruleForm';

type AlertEvent = components['schemas']['AlertEvent'];
type AlertEventStatus = 'firing' | 'resolved';

const severityColor: Record<string, string> = {
  critical: 'red',
  warning: 'gold',
  info: 'blue',
};

const notifyStatusText: Record<string, string> = {
  none: '未投递',
  pending: '投递中',
  sent: '已投递',
  failed: '投递失败',
  skipped: '静默跳过',
};

export interface EventsTableProps {
  onDiagnose: (event: AlertEvent) => void;
}

export default function EventsTable({ onDiagnose }: EventsTableProps) {
  const [statusFilter, setStatusFilter] = useState<'all' | AlertEventStatus>('all');
  const [severityFilter, setSeverityFilter] = useState<'all' | 'info' | 'warning' | 'critical'>(
    'all',
  );
  // 追加式分页：beforeId 由后端 next_before_id（当前页最旧一条）推进，
  // 已加载的更早页保留在 olderPages（此前实现误用 items[0]（最新一条），
  // 点击后几乎原地踏步且整表替换丢失新事件——五期评审发现）。
  const [beforeId, setBeforeId] = useState<number | undefined>(undefined);
  const [olderPages, setOlderPages] = useState<AlertEvent[][]>([]);
  const [, setCursorStack] = useState<number[]>([]);

  const filters = useMemo(() => {
    const query: {
      status?: 'firing' | 'resolved';
      severity?: 'info' | 'warning' | 'critical';
      before_id?: number;
      limit?: number;
    } = { limit: 50 };
    if (statusFilter !== 'all') {
      query.status = statusFilter;
    }
    if (severityFilter !== 'all') {
      query.severity = severityFilter;
    }
    if (beforeId !== undefined) {
      query.before_id = beforeId;
    }
    return query;
  }, [statusFilter, severityFilter, beforeId]);

  const eventsQuery = useQuery({
    queryKey: qk.alertEvents(filters),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/alert-events', {
        params: { query: filters },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    // 契约 8.4：有 firing 15s，无 firing 60s。
    refetchInterval: (query) => {
      const items = query.state.data?.items ?? [];
      return items.some((event) => event.status === 'firing') ? 15_000 : 60_000;
    },
  });

  const items = useMemo(
    () => [...olderPages.flat().reverse(), ...(eventsQuery.data?.items ?? [])].reverse(),
    [olderPages, eventsQuery.data],
  );
  const hasMore = eventsQuery.data?.has_more ?? false;
  const nextCursor = eventsQuery.data?.next_before_id ?? null;

  const columns: TableColumnsType<AlertEvent> = [
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
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (status: string) =>
        status === 'firing' ? <Tag color="red">触发中</Tag> : <Tag color="green">已恢复</Tag>,
    },
    {
      title: '规则 / 服务器',
      key: 'target',
      render: (_, event) => (
        <Space direction="vertical" size={0}>
          {/* 六期打磨 sprint 4（走查 low）：系统哨兵规则名为英文原码
              （monitor.collector.unreachable），按名映射「采集不可达（系统）」，
              原始名保留在 Tooltip。 */}
          <Tooltip title={event.rule_name === ruleDisplayName(event.rule_name) ? null : event.rule_name}>
            <span className="opsmind-admin-cell-title">
              {ruleDisplayName(event.rule_name)}{' '}
              {/* 契约 8.7 ⑦（FE-P2 六期）：事件 ID 复制（openapi 中为 int64）。 */}
              <CopyButton text={String(event.id)} label="复制事件 ID" />
            </span>
          </Tooltip>
          <span className="opsmind-admin-cell-sub">
            {event.server_name || event.server_id.slice(0, 8)}
          </span>
        </Space>
      ),
    },
    {
      title: '指标 / 触发值',
      key: 'value',
      render: (_, event) => (
        <span className="opsmind-admin-mono">
          {METRIC_LABELS[event.metric] ?? event.metric} = {formatEventValue(event)}
        </span>
      ),
    },
    {
      title: '消息',
      dataIndex: 'message',
      ellipsis: true,
      render: (text: string) => <Typography.Text type="secondary">{text}</Typography.Text>,
    },
    {
      title: '触发 / 恢复',
      key: 'time',
      width: 180,
      render: (_, event) => (
        <Space direction="vertical" size={0}>
          <span>{formatDateTime(event.fired_at)}</span>
          <span className="opsmind-admin-cell-sub">
            {event.resolved_at ? `恢复于 ${formatDateTime(event.resolved_at)}` : '—'}
          </span>
        </Space>
      ),
    },
    {
      title: '通知',
      dataIndex: 'notify_status',
      width: 100,
      render: (status: string) => notifyStatusText[status] ?? status,
    },
    {
      title: '操作',
      key: 'actions',
      width: 90,
      render: (_, event) => (
        <Button
          size="small"
          onClick={() => {
            onDiagnose(event);
          }}
        >
          AI 诊断
        </Button>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Space wrap>
        <Select
          aria-label="事件状态过滤"
          value={statusFilter}
          onChange={(value) => {
            setStatusFilter(value);
            setBeforeId(undefined);
          }}
          style={{ width: 140 }}
          options={[
            { value: 'all', label: '全部状态' },
            { value: 'firing', label: '触发中' },
            { value: 'resolved', label: '已恢复' },
          ]}
        />
        <Select
          aria-label="事件等级过滤"
          value={severityFilter}
          onChange={(value) => {
            setSeverityFilter(value);
            setBeforeId(undefined);
          }}
          style={{ width: 140 }}
          options={[
            { value: 'all', label: '全部等级' },
            { value: 'critical', label: '严重' },
            { value: 'warning', label: '警告' },
            { value: 'info', label: '信息' },
          ]}
        />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          触发中事件自动刷新（15s）
        </Typography.Text>
      </Space>

      {eventsQuery.isError ? (
        <QueryErrorState
          error={eventsQuery.error}
          onRetry={() => {
            void eventsQuery.refetch();
          }}
        />
      ) : (
        <Table<AlertEvent>
          rowKey="id"
          size="small"
          loading={eventsQuery.isLoading}
          columns={columns}
          dataSource={items}
          locale={{ emptyText: <EmptyState description="暂无告警事件" /> }}
          pagination={false}
        />
      )}

      {hasMore && (eventsQuery.data?.items?.length ?? 0) > 0 ? (
        <Button
          block
          disabled={nextCursor === null}
          onClick={() => {
            if (nextCursor === null) {
              return;
            }
            // 把当前页压入已加载栈、游标推进到后端给的 next_before_id。
            setOlderPages((pages) => [...pages, eventsQuery.data?.items ?? []]);
            setCursorStack((stack) => [...stack, beforeId ?? 0]);
            setBeforeId(nextCursor);
          }}
        >
          加载更早事件
        </Button>
      ) : null}
      {olderPages.length > 0 ? (
        <Button
          block
          onClick={() => {
            // 返回上一页：弹出已加载栈顶，游标回退。
            setOlderPages((pages) => pages.slice(0, -1));
            setCursorStack((stack) => {
              const prev = stack[stack.length - 1] ?? 0;
              setBeforeId(prev === 0 ? undefined : prev);
              return stack.slice(0, -1);
            });
          }}
        >
          返回较新事件
        </Button>
      ) : null}
    </Space>
  );
}

function formatEventValue(event: AlertEvent): string {
  if (event.metric === 'net_in_bps' || event.metric === 'net_out_bps') {
    // 字段语义是字节/秒（契约 7.7），标签用 byte 单位（五期评审：此前
    // 误标 bit 单位 Kbps/Mbps，读数相差 8 倍）。
    return formatBps(event.value);
  }
  return event.value.toFixed(1);
}
