// M4 五期：监控仪表盘页（CONTRACT.md 8.1 /monitor、8.7 监控页）。
// 结构：顶部概要卡（server 数 / firing 按 severity 分色 / 今日最近巡检均分 /
// 采集器状态）→ 服务器选择 + 时间窗（1h/6h/24h）→ 6 指标 SVG 折线卡 →
// 采集配置卡。
//
// 数据规则（8.4 五期）：折线 30s refetchInterval（collector.enabled=false
// 时不自动刷新）；服务器/时间窗切换直接切 key 不失效旧 key；401 由
// lib/queryClient.ts 全局跳登录。

import { useQuery } from '@tanstack/react-query';
import {
  Alert,
  Card,
  Col,
  Flex,
  Row,
  Segmented,
  Select,
  Space,
  Statistic,
  Tag,
  Typography,
} from 'antd';
import { useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { QueryErrorState } from '../../components/admin/AdminStates';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import '../../styles/admin.css';

import CollectorCard from './CollectorCard';
import MetricChart from './MetricChart';
import { formatBps } from './chartMath';
import type { SeriesPoint } from './downsample';

type MonitorOverview = components['schemas']['MonitorOverview'];
type MetricPoint = components['schemas']['MetricPoint'];

const METRIC_REFRESH_MS = 30_000;

const TIME_WINDOWS: ReadonlyArray<{ label: string; minutes: number }> = [
  { label: '1 小时', minutes: 60 },
  { label: '6 小时', minutes: 360 },
  { label: '24 小时', minutes: 1440 },
];

interface MetricCardSpec {
  key: 'cpu_pct' | 'mem_pct' | 'disk_pct' | 'load_1' | 'net_in_bps' | 'net_out_bps';
  title: string;
  subtitle: string;
  color: string;
  format: (value: number) => string;
  latest: (row: components['schemas']['MetricLatestRow']) => number;
}

const pct = (value: number) => `${value.toFixed(1)}%`;

const METRIC_CARDS: readonly MetricCardSpec[] = [
  {
    key: 'cpu_pct',
    title: 'CPU 使用率',
    subtitle: '与上一轮采集的差值',
    color: 'var(--color-chart-1)',
    format: pct,
    latest: (row) => row.cpu_pct,
  },
  {
    key: 'mem_pct',
    title: '内存使用率',
    subtitle: '/proc/meminfo',
    color: 'var(--color-chart-2)',
    format: pct,
    latest: (row) => row.mem_pct,
  },
  {
    key: 'disk_pct',
    title: '磁盘使用率',
    subtitle: '最大文件系统使用率',
    color: 'var(--color-chart-3)',
    format: pct,
    latest: (row) => row.disk_pct,
  },
  {
    key: 'load_1',
    title: '负载（1m）',
    subtitle: 'loadavg 1 分钟',
    color: 'var(--color-chart-4)',
    format: (value) => value.toFixed(2),
    latest: (row) => row.load_1,
  },
  {
    key: 'net_in_bps',
    title: '网络入',
    subtitle: '接收速率',
    color: 'var(--color-chart-5)',
    format: formatBps,
    latest: (row) => row.net_in_bps,
  },
  {
    key: 'net_out_bps',
    title: '网络出',
    subtitle: '发送速率',
    color: 'var(--color-chart-6)',
    format: formatBps,
    latest: (row) => row.net_out_bps,
  },
];

export default function MonitorPage() {
  const [serverId, setServerId] = useState<string | null>(null);
  const [minutes, setMinutes] = useState(60);

  const overviewQuery = useQuery({
    queryKey: qk.monitorOverview,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/monitor/overview');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
  });

  const collectorQuery = useQuery({
    queryKey: qk.collector,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/monitor/collector');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
  });

  // FE-P2（六期，契约 8.7 ⑥）：服务器 IP 复制——MonitorOverview 不含 host
  // 字段，这里复用全站 servers 缓存（qk.servers，管理页已拉取时零请求）
  // 把选中服务器映射回 host:port。
  const serversForIpQuery = useQuery({
    queryKey: qk.servers,
    queryFn: async () => {
      const { data, error } = await api.GET('/api/servers');
      if (error) {
        throw error;
      }
      return data.items;
    },
  });

  const overview: MonitorOverview | undefined = overviewQuery.data;
  // 服务器选择器仅列有 server_metrics_latest 行的资产（8.7：overview.servers
  // 即 latest JOIN 结果）。useMemo 稳定引用（exhaustive-deps）。
  const servers = useMemo(() => overview?.servers ?? [], [overview]);
  // 选中失效（如服务器被删除）时渲染期回落到第一台，不经 effect setState。
  const effectiveServerId =
    serverId !== null && servers.some((server) => server.server_id === serverId)
      ? serverId
      : (servers[0]?.server_id ?? null);
  const selectedServer = servers.find((server) => server.server_id === effectiveServerId) ?? null;
  // 选中服务器的主机地址（servers 缓存命中时即时可用；未加载时为 null）。
  const selectedServerAddress =
    serversForIpQuery.data?.find((server) => server.id === effectiveServerId) ?? null;

  const metricsQuery = useQuery({
    queryKey: qk.serverMetrics(effectiveServerId ?? '', minutes),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers/{id}/metrics', {
        params: { path: { id: effectiveServerId ?? '' }, query: { minutes } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: effectiveServerId !== null,
    // 契约 8.4：30s 自动刷新，仅在采集开启时（enabled=false 折线停止刷新）。
    refetchInterval: collectorQuery.data?.enabled ? METRIC_REFRESH_MS : false,
  });

  const seriesByMetric = useMemo(() => {
    const series = metricsQuery.data;
    const points: MetricPoint[] = series?.points ?? [];
    const result: Record<string, SeriesPoint[]> = {};
    for (const card of METRIC_CARDS) {
      result[card.key] = points.map((point) => ({
        t: Date.parse(point.created_at),
        v: point[card.key] as number,
      }));
    }
    return result;
  }, [metricsQuery.data]);

  const firing = overview?.firing;
  const lastInspection = overview?.last_inspection ?? null;
  const collectorEnabled = collectorQuery.data?.enabled ?? true;

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="监控"
        description="服务器 CPU / 内存 / 磁盘 / 网络指标、告警状态与采集配置。"
      />

      {overviewQuery.isError ? (
        <QueryErrorState
          error={overviewQuery.error}
          onRetry={() => {
            void overviewQuery.refetch();
          }}
        />
      ) : (
        <Row gutter={[12, 12]}>
          <Col xs={12} md={6}>
            <Card size="small">
              <Statistic title="服务器（已采集）" value={servers.length} suffix="台" />
            </Card>
          </Col>
          <Col xs={12} md={6}>
            <Card size="small">
              <Statistic
                title="触发中告警"
                value={firing?.total ?? 0}
                suffix={
                  <Space size={4} wrap>
                    {firing && firing.critical > 0 ? (
                      <Tag color="red">严重 {firing.critical}</Tag>
                    ) : null}
                    {firing && firing.warning > 0 ? (
                      <Tag color="gold">警告 {firing.warning}</Tag>
                    ) : null}
                    {firing && firing.info > 0 ? <Tag color="blue">信息 {firing.info}</Tag> : null}
                  </Space>
                }
              />
            </Card>
          </Col>
          <Col xs={12} md={6}>
            <Card size="small">
              <Statistic
                title="最近巡检均分"
                value={lastInspection?.avg_score ?? '—'}
                suffix={lastInspection ? '分' : ''}
              />
            </Card>
          </Col>
          <Col xs={12} md={6}>
            <Card size="small">
              <Statistic
                title="采集器"
                value={collectorEnabled ? '运行中' : '已暂停'}
                valueStyle={{ fontSize: 22 }}
                suffix={
                  collectorQuery.data ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      每 {collectorQuery.data.interval_seconds}s 一轮
                    </Typography.Text>
                  ) : null
                }
              />
            </Card>
          </Col>
        </Row>
      )}

      <Flex gap={12} wrap="wrap" align="center" style={{ marginTop: 4 }}>
        <Select
          style={{ minWidth: 260 }}
          placeholder="选择服务器"
          aria-label="选择服务器"
          value={effectiveServerId}
          onChange={(value) => {
            setServerId(value);
          }}
          options={servers.map((server) => ({
            value: server.server_id,
            label: server.server_name || server.server_id.slice(0, 8),
          }))}
          notFoundContent={<EmptyState description="暂无已采集的服务器（等待首轮采集）" />}
        />
        <Segmented
          value={minutes}
          onChange={(value) => {
            setMinutes(value as number);
          }}
          options={TIME_WINDOWS.map((window) => ({ label: window.label, value: window.minutes }))}
        />
        {/* FE-P2（六期，契约 8.7 ⑥）：选中服务器 IP 复制。 */}
        {selectedServerAddress ? (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {selectedServerAddress.host}:{selectedServerAddress.port}{' '}
            <CopyButton
              text={`${selectedServerAddress.host}:${selectedServerAddress.port}`}
              label="复制服务器 IP"
            />
          </Typography.Text>
        ) : null}
        {metricsQuery.data?.truncated ? (
          <Typography.Text type="warning" style={{ fontSize: 12 }}>
            返回点集已截断（{metricsQuery.data.points.length} 点）
          </Typography.Text>
        ) : null}
      </Flex>

      {effectiveServerId === null && !overviewQuery.isLoading ? (
        <EmptyState description="暂无服务器指标：请先在「服务器」页添加资产并开启采集" />
      ) : metricsQuery.isError ? (
        <QueryErrorState
          error={metricsQuery.error}
          onRetry={() => {
            void metricsQuery.refetch();
          }}
        />
      ) : null}

      {!collectorEnabled && effectiveServerId !== null ? (
        <Alert type="warning" showIcon message="采集已暂停：折线展示历史数据，不再自动刷新。" />
      ) : null}

      {effectiveServerId !== null && !metricsQuery.isError ? (
        <Row gutter={[12, 12]}>
          {METRIC_CARDS.map((card) => {
            const latestValue = selectedServer ? card.latest(selectedServer) : null;
            return (
              <Col xs={24} lg={12} xl={8} key={card.key}>
                <Card
                  size="small"
                  title={
                    <Space size={8}>
                      <span>{card.title}</span>
                      {latestValue !== null ? (
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                          {/* 契约 8.7 ⑥（FE-P2 六期）：指标当前值（含单位）复制。 */}
                          当前 {card.format(latestValue)}{' '}
                          <CopyButton
                            text={card.format(latestValue)}
                            label={`复制${card.title}当前值`}
                          />
                        </Typography.Text>
                      ) : null}
                    </Space>
                  }
                >
                  <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 4 }}>
                    {card.subtitle}
                  </Typography.Paragraph>
                  <MetricChart
                    points={seriesByMetric[card.key] ?? []}
                    formatValue={card.format}
                    color={card.color}
                  />
                </Card>
              </Col>
            );
          })}
        </Row>
      ) : null}

      <CollectorCard />
    </div>
  );
}
