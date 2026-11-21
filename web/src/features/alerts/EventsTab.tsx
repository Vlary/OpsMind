// 告警「事件」Tab（图表化本轮）：顶部告警分布两图 → 现有状态/等级筛选行
// → 事件表。
//
// 两图共用 24h/7d 切换与一次聚合请求（GET /api/alert-events/stats）；
// 点击柱/条把规则或时段写入事件表筛选，以可单独清除的 Tag 展示，与表格
// 自身筛选叠加。规则下钻同时带上 stats.from/to，事件表与柱子同窗口口径
// （走查第 12 轮）；默认列表则显式标「全部时间」，避免与图表计数无说明
// 并列。图表与事件列表都需要 monitor:read：没有该权限的角色
// （例如仅有 monitor:manage 的配置角色）不渲染统计卡也不发必然 403 的请求。

import { useQuery } from '@tanstack/react-query';
import { Alert, Card, Col, Flex, Row, Segmented, Space, Typography } from 'antd';
import { useReducer } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { hasAnyPermission, PERMS } from '../../lib/rbacPermissions';
import { usePermissions } from '../auth/useMe';
import AlertSeverityStackedBarChart from '../monitor/charts/AlertSeverityStackedBarChart';
import type { AlertBucketSelection } from '../monitor/charts/AlertSeverityStackedBarChart';
import NoisyRuleBarChart from '../monitor/charts/NoisyRuleBarChart';
import type { NoisyRuleSelection } from '../monitor/charts/NoisyRuleBarChart';
import type { ChartWindow } from '../monitor/charts/chartModel';

import EventsTable from './EventsTable';
import {
  eventsFilterReducer,
  eventWindowScope,
  formatRuleDrilldownLabel,
  formatTimeDrilldownLabel,
  formatWindowScopeLabel,
  INITIAL_EVENTS_FILTER_STATE,
} from './eventFilters';

type AlertEvent = components['schemas']['AlertEvent'];

const WINDOW_OPTIONS: ReadonlyArray<{ value: ChartWindow; label: string }> = [
  { value: '24h', label: '24 小时' },
  { value: '7d', label: '7 天' },
];

export interface EventsTabProps {
  onDiagnose: (event: AlertEvent) => void;
}

export default function EventsTab({ onDiagnose }: EventsTabProps) {
  const permissions = usePermissions();
  const canReadMetrics = hasAnyPermission(permissions, [PERMS.metricsRead]);
  // 窗口 + 两类下钻共用一份 reducer：切窗口必须原子地清掉时段筛选，
  // 避免「Tag/事件表仍按 1h 过滤、图上高亮 6h 桶」的错位。
  const [filters, dispatchFilters] = useReducer(eventsFilterReducer, INITIAL_EVENTS_FILTER_STATE);
  const { window, ruleFilter, timeFilter } = filters;

  const statsQuery = useQuery({
    queryKey: qk.alertEventStats(window, 8),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/alert-events/stats', {
        params: { query: { window, top_rules: 8 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: canReadMetrics,
    // 与事件表同源：有数据即 60s 刷新；窗口切换只切 key，另一档缓存保留。
    refetchInterval: 60_000,
  });

  if (!canReadMetrics) {
    return (
      <Alert
        type="info"
        showIcon
        title="当前角色无监控只读权限"
        description="告警分布与事件列表需要 monitor:read；规则与通知渠道管理不受影响。"
      />
    );
  }

  function handleBucketSelect(selection: AlertBucketSelection): void {
    dispatchFilters({
      type: 'time',
      selection: {
        after: selection.after,
        // 后端 fired_before 与图表桶同为半开区间 [after, before)：原样
        // 下发桶终点，不减 1ms（走查第 13 轮 medium）。Tag 文案按原桶
        // 终点展示。
        before: selection.before,
        bucketStart: selection.bucketStart,
        label: formatTimeDrilldownLabel(selection.after, selection.before, window),
      },
    });
  }

  function handleRuleSelect(selection: NoisyRuleSelection): void {
    dispatchFilters({
      type: 'rule',
      selection: {
        id: selection.rule_id,
        name: formatRuleDrilldownLabel(selection.rule_name, selection.rule_id),
      },
    });
  }

  const stats = statsQuery.data;
  // 走查第 12 轮 medium：规则下钻必须与柱子的窗口同口径。柱上「触发 21
  // 次」只统计当前窗口，而事件表此前只带 rule_id，展示全时 71 条，且没有
  // 任何「全部时间/当前窗口」提示。这里用当前 stats 的 from/to 作为事件表
  // 边界（每次 stats 刷新都会跟着滑动），Tag 同步显示「近 24 小时/7 天」。
  const windowScope = stats ? eventWindowScope(window, stats.from, stats.to) : null;
  const ruleWindowActive = ruleFilter !== null;

  const statsError = statsQuery.isError ? (
    <QueryErrorState
      error={statsQuery.error}
      onRetry={() => {
        void statsQuery.refetch();
      }}
    />
  ) : undefined;

  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      <section aria-label="告警分布">
        <Flex align="flex-start" justify="space-between" gap={12} wrap="wrap">
          <div style={{ minWidth: 0 }}>
            <Typography.Title level={5} style={{ margin: 0 }}>
              告警分布
            </Typography.Title>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              最近 {window === '24h' ? '24 小时' : '7 天'}告警集中在哪些时段与规则；
              点击柱/条可下钻到下方事件表。
              {stats
                ? ` ${formatWindowScopeLabel(window)}共 ${stats.total} 条事件 / ${stats.distinct_rules} 条规则。`
                : null}
            </Typography.Text>
          </div>
          <Segmented
            aria-label="告警分布时间范围"
            value={window}
            onChange={(value) => {
              dispatchFilters({ type: 'window', window: value as ChartWindow });
            }}
            options={WINDOW_OPTIONS.map((option) => ({
              label: option.label,
              value: option.value,
            }))}
          />
        </Flex>

        {statsQuery.isError ? (
          <div style={{ marginTop: 12 }}>{statsError}</div>
        ) : (
          <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
            <Col xs={24} lg={12}>
              <Card size="small" title="严重度时间分布">
                {/* 见 MonitorPage 同名注释：数据就绪后重挂载一次，避免
                    图表在 skeleton 阶段挂载导致宽度测量停留在 fallback。 */}
                <AlertSeverityStackedBarChart
                  key={stats?.severity_timeline.length ? 'timeline-ready' : 'timeline-pending'}
                  buckets={stats?.severity_timeline ?? []}
                  window={window}
                  {...(stats ? { bucketSeconds: stats.bucket_seconds } : {})}
                  {...(stats?.server_time ? { now: Date.parse(stats.server_time) } : {})}
                  onBucketSelect={handleBucketSelect}
                  selectedBucketStart={timeFilter?.bucketStart ?? null}
                  loading={statsQuery.isLoading}
                  height={240}
                />
              </Card>
            </Col>
            <Col xs={24} lg={12}>
              <Card size="small" title="最吵规则 Top 8">
                <NoisyRuleBarChart
                  key={stats?.top_rules.length ? 'rules-ready' : 'rules-pending'}
                  rules={stats?.top_rules ?? []}
                  {...(stats ? { totalRules: stats.distinct_rules } : {})}
                  window={window}
                  limit={8}
                  onRuleSelect={handleRuleSelect}
                  selectedRuleId={ruleFilter?.id ?? null}
                  loading={statsQuery.isLoading}
                  height={240}
                />
              </Card>
            </Col>
          </Row>
        )}
      </section>

      <EventsTable
        onDiagnose={onDiagnose}
        ruleId={ruleFilter?.id ?? null}
        ruleLabel={ruleFilter?.name ?? null}
        ruleWindowAfter={ruleWindowActive ? (windowScope?.after ?? null) : null}
        ruleWindowBefore={ruleWindowActive ? (windowScope?.before ?? null) : null}
        ruleWindowLabel={ruleWindowActive ? (windowScope?.label ?? null) : null}
        ruleWindowError={ruleWindowActive && statsQuery.isError && windowScope === null}
        chartWindowLabel={windowScope?.label ?? formatWindowScopeLabel(window)}
        firedAfter={timeFilter?.after ?? null}
        firedBefore={timeFilter?.before ?? null}
        timeLabel={timeFilter?.label ?? null}
        onClearRule={() => {
          dispatchFilters({ type: 'clearRule' });
        }}
        onClearTime={() => {
          dispatchFilters({ type: 'clearTime' });
        }}
      />
    </Space>
  );
}
