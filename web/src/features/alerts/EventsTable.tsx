// M4 五期：告警事件列表（CONTRACT.md 8.7——firing/resolved 过滤、severity
// Tag、触发值、notify_status；行操作「诊断」）。轮询（8.4）：有 firing 15s、
// 无 firing 60s。before_id 分页加载更多。
//
// 图表化本轮：顶部两图点击产生 rule_id / 触发时段筛选，与表头状态/等级
// 筛选叠加，各以可清除 Tag 展示。筛选变化通过 key 重挂载列表主体来重置
// 分页游标（不用 effect 里 setState 触发级联渲染）。
//
// 走查第 12 轮：规则下钻不是全时口径——它带上 stats.from/to 与柱子同窗口，
// Tag 写「范围：近 24 小时/近 7 天」；默认列表则显式标「全部时间」并说明
// 图表只统计当前窗口。

import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import { formatBps } from '../monitor/chartMath';

import { mergeEventPageHistory } from './eventPages';
import { METRIC_LABELS, ruleDisplayName, SEVERITY_LABELS } from './ruleForm';

type AlertEvent = components['schemas']['AlertEvent'];
type AlertEventStatus = 'firing' | 'resolved';
type AlertEventSeverity = 'info' | 'warning' | 'critical';

type SeverityFilter = 'all' | AlertEventSeverity;
type StatusFilter = 'all' | AlertEventStatus;

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
  /**
   * 图表下钻筛选（图表化本轮）。与表头状态/等级筛选、before_id 游标叠加；
   * 每个维度独立可清除（父组件负责置空）。
   */
  ruleId?: string | null;
  ruleLabel?: string | null;
  /**
   * 规则下钻所属图表窗口（stats.from/to，走查第 12 轮）。规则筛选不再是
   * 全时口径：事件表带上同一窗口边界，柱上「触发 N 次」与表内行数一致。
   * 窗口未就绪前不渲染列表，绝不回退成无提示的全时查询。
   */
  ruleWindowAfter?: string | null;
  ruleWindowBefore?: string | null;
  ruleWindowLabel?: string | null;
  /** 规则已选中但图表窗口统计加载失败：给出可操作提示而不是全时列表。 */
  ruleWindowError?: boolean;
  /** 「近 24 小时 / 近 7 天」，用于说明默认全时列表与图表窗口的差异。 */
  chartWindowLabel?: string;
  firedAfter?: string | null;
  firedBefore?: string | null;
  timeLabel?: string | null;
  onClearRule?: () => void;
  onClearTime?: () => void;
}

export default function EventsTable({
  onDiagnose,
  ruleId = null,
  ruleLabel = null,
  ruleWindowAfter = null,
  ruleWindowBefore = null,
  ruleWindowLabel = null,
  ruleWindowError = false,
  chartWindowLabel = '近 24 小时',
  firedAfter = null,
  firedBefore = null,
  timeLabel = null,
  onClearRule,
  onClearTime,
}: EventsTableProps) {
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all');
  const [severityFilter, setSeverityFilter] = useState<SeverityFilter>('all');

  // 规则下钻绑定图表窗口；时段桶下钻更窄，优先使用。两者都没有时才是
  // 明确的「全部时间」列表（走查第 12 轮：此前规则下钻会静默落到全时）。
  const bucketTimeActive = firedAfter !== null && firedBefore !== null;
  const effectiveAfter = firedAfter ?? (ruleId ? ruleWindowAfter : null);
  const effectiveBefore = firedBefore ?? (ruleId ? ruleWindowBefore : null);
  const ruleWindowPending = ruleId !== null && effectiveAfter === null;

  // 任一筛选变化都换 key：列表主体重挂载 ⇒ before_id/已加载页自动归零，
  // 不会把上一组条件的游标带进新查询。表头状态因此保留（满足「叠加」）。
  const listKey = [
    statusFilter,
    severityFilter,
    ruleId ?? '',
    effectiveAfter ?? '',
    effectiveBefore ?? '',
  ].join('|');

  const scopeHint = bucketTimeActive
    ? '事件表与图表所选时间桶同口径。'
    : ruleId !== null
      ? `事件表与图表同窗口（${ruleWindowLabel ?? chartWindowLabel}），与柱上触发次数同口径。`
      : `事件表为全部时间；上方图表只统计${chartWindowLabel}。`;

  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      <Space wrap>
        <Select
          aria-label="事件状态过滤"
          value={statusFilter}
          onChange={(value: StatusFilter) => {
            setStatusFilter(value);
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
          onChange={(value: SeverityFilter) => {
            setSeverityFilter(value);
          }}
          style={{ width: 140 }}
          options={[
            { value: 'all', label: '全部等级' },
            { value: 'critical', label: '严重' },
            { value: 'warning', label: '警告' },
            { value: 'info', label: '信息' },
          ]}
        />
        {ruleId ? (
          <Tag
            closable
            color="blue"
            aria-label={`清除规则筛选 ${ruleLabel ?? ruleId}`}
            onClose={() => {
              onClearRule?.();
            }}
          >
            规则：{ruleLabel ?? ruleId.slice(0, 8)}
          </Tag>
        ) : null}
        {bucketTimeActive ? (
          <Tag
            closable
            color="purple"
            aria-label={`清除时段筛选 ${timeLabel ?? ''}`}
            onClose={() => {
              onClearTime?.();
            }}
          >
            时段：{timeLabel ?? '自定义时段'}
          </Tag>
        ) : ruleId !== null && ruleWindowLabel !== null ? (
          // 走查第 12 轮：规则下钻的范围就是图表窗口，Tag 明示「近 24 小时/
          // 近 7 天」；它跟随规则一起清除，因此不做成可单独关闭的筛选。
          <Tag color="cyan" aria-label={`事件表时间范围 ${ruleWindowLabel}，与图表同窗口`}>
            范围：{ruleWindowLabel}
          </Tag>
        ) : ruleId === null && !bucketTimeActive ? (
          // 默认列表是全时的，而图上是窗口计数。显式标出「全部时间」，避免
          // 「图 26 条 / 表 166 条」无提示并列。
          <Tag color="default" aria-label="事件表时间范围 全部时间">
            全部时间
          </Tag>
        ) : null}
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {scopeHint} 触发中事件自动刷新（15s）。
        </Typography.Text>
      </Space>

      {ruleWindowPending ? (
        <Alert
          type={ruleWindowError ? 'warning' : 'info'}
          showIcon
          title={
            ruleWindowError ? '图表窗口统计加载失败，暂无法按窗口筛选事件' : '正在读取图表窗口范围…'
          }
          description={ruleWindowError ? '请重试上方的告警分布统计后再查看事件列表。' : undefined}
        />
      ) : (
        <EventList
          key={listKey}
          statusFilter={statusFilter}
          severityFilter={severityFilter}
          ruleId={ruleId}
          firedAfter={effectiveAfter}
          firedBefore={effectiveBefore}
          onDiagnose={onDiagnose}
        />
      )}
    </Space>
  );
}

interface EventListProps {
  statusFilter: StatusFilter;
  severityFilter: SeverityFilter;
  ruleId: string | null;
  firedAfter: string | null;
  firedBefore: string | null;
  onDiagnose: (event: AlertEvent) => void;
}

function EventList({
  statusFilter,
  severityFilter,
  ruleId,
  firedAfter,
  firedBefore,
  onDiagnose,
}: EventListProps) {
  // 追加式分页：beforeId 由后端 next_before_id（当前页最旧一条）推进，
  // 已加载的更早页保留在 olderPages（此前实现误用 items[0]（最新一条），
  // 点击后几乎原地踏步且整表替换丢失新事件——五期评审发现）。
  const [beforeId, setBeforeId] = useState<number | undefined>(undefined);
  const [olderPages, setOlderPages] = useState<AlertEvent[][]>([]);
  const [, setCursorStack] = useState<number[]>([]);

  const filters = useMemo(() => {
    const query: {
      status?: AlertEventStatus;
      severity?: AlertEventSeverity;
      rule_id?: string;
      fired_after?: string;
      fired_before?: string;
      before_id?: number;
      limit?: number;
    } = { limit: 50 };
    if (statusFilter !== 'all') {
      query.status = statusFilter;
    }
    if (severityFilter !== 'all') {
      query.severity = severityFilter;
    }
    // 图表下钻：规则与时段各自独立，可同时存在。
    if (ruleId) {
      query.rule_id = ruleId;
    }
    if (firedAfter && firedBefore) {
      query.fired_after = firedAfter;
      query.fired_before = firedBefore;
    }
    if (beforeId !== undefined) {
      query.before_id = beforeId;
    }
    return query;
  }, [statusFilter, severityFilter, ruleId, firedAfter, firedBefore, beforeId]);

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
    () => mergeEventPageHistory(olderPages, eventsQuery.data?.items ?? []),
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
        <Space orientation="vertical" size={0}>
          {/* 六期打磨 sprint 4（走查 low）：系统哨兵规则名为英文原码
              （monitor.collector.unreachable），按名映射「采集不可达（系统）」，
              原始名保留在 Tooltip。 */}
          <Tooltip
            title={event.rule_name === ruleDisplayName(event.rule_name) ? null : event.rule_name}
          >
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
        <Space orientation="vertical" size={0}>
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
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
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
          // 走查第 3 轮 medium：390 宽下自动布局把「规则 / 服务器」压成
          // 一字一行（.app-content scrollHeight 14089）。固定 900px 表宽
          // 后横向滚动，主列至少 ~120px，行高恢复正常。
          scroll={{ x: 900 }}
          tableLayout="fixed"
          locale={{
            emptyText: (
              <EmptyState
                description={
                  ruleId || (firedAfter && firedBefore)
                    ? '当前下钻条件下暂无告警事件，可清除上方 Tag 放宽筛选'
                    : '暂无告警事件'
                }
              />
            ),
          }}
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
