// M4 五期：监控仪表盘页（CONTRACT.md 8.1 /monitor、8.7 监控页）。
// 结构：顶部概要卡（server 数 / firing 按 severity 分色 / 今日最近巡检均分 /
// 采集器状态）→ 服务器选择 + 时间窗（1h/6h/24h）→ 6 指标 SVG 折线卡 →
// 采集配置卡。
//
// 数据规则（8.4 五期）：折线 30s refetchInterval（collector.enabled=false
// 时不自动刷新）；服务器/时间窗切换直接切 key 不失效旧 key；401 由
// lib/queryClient.ts 全局跳登录。

import { useQuery, useQueryClient } from '@tanstack/react-query';
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
import { useEffect, useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { QueryErrorState } from '../../components/admin/AdminStates';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import { hasAnyPermission, PERMS, type Permission } from '../../lib/rbacPermissions';
import { usePermissions } from '../auth/useMe';
import '../../styles/admin.css';

import CollectorCard from './CollectorCard';
import MetricChart from './MetricChart';
import ClusterTrendChart from './charts/ClusterTrendChart';
import TopServerBarChart from './charts/TopServerBarChart';
import {
  buildServerOptions,
  formatInspectionTime,
  formatSampleAge,
  formatSampleTime,
  isFutureSample,
  isStale,
  latestTimestamp,
  metricChartWindow,
  pickDefaultServerId,
  sampleTimeAheadMs,
  staleAgeMs,
  staleThresholdMs,
  type ChartWindow,
} from './charts/chartModel';
import {
  formatBps,
  formatCoverageDuration,
  formatNumericAxisTick,
  formatSignificant,
} from './chartMath';
import type { SeriesPoint } from './downsample';

type MonitorOverview = components['schemas']['MonitorOverview'];
type MetricPoint = components['schemas']['MetricPoint'];
type CollectorConfig = components['schemas']['CollectorConfig'];

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
  /** Windows 资产的标题（仅负载与 Linux 不同口径）。 */
  windowsTitle?: string;
  /** Windows 资产的副文案（Win32_* 采集源）。 */
  windowsSubtitle: string;
  /** os 缺失（旧接口/旧数据）时的平台中性副文案，不冒充 /proc 口径。 */
  neutralSubtitle: string;
  color: string;
  format: (value: number) => string;
  /** y 轴统一记法（可选；负载用，见 formatNumericAxisTick）。 */
  axisFormat?: (value: number, axisMax: number) => string;
  latest: (row: components['schemas']['MetricLatestRow']) => number;
  /** 百分比指标：y 轴 100% 硬上限（见 MetricChart.percentAxis）。 */
  percentAxis: boolean;
}

/** 指标口径所属平台（走查第 15 轮 medium）。 */
export type MetricPlatform = 'linux' | 'windows' | 'unknown';

/** 把 overview.servers[].os 收敛成三态；缺失/未知一律走中性文案。 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function metricPlatform(os: string | null | undefined): MetricPlatform {
  return os === 'linux' || os === 'windows' ? os : 'unknown';
}

const pct = (value: number) => `${value.toFixed(1)}%`;

const METRIC_CARDS: readonly MetricCardSpec[] = [
  {
    key: 'cpu_pct',
    title: 'CPU 使用率',
    // 走查第 2 轮 low：cpu_pct 是 /proc/stat 的利用率（两次采样差分算出），
    // 不是「与上一轮采集的差值」；旧文案把口径说反。
    subtitle: '/proc/stat 使用率（两次采样差分计算）',
    // 走查第 15 轮 medium：Windows 采集源完全不同（parse_win.go:40），
    // 不能再把 Linux 的 /proc/stat 写在 Windows 资产卡上。
    windowsSubtitle: 'Win32_Processor LoadPercentage（CIM 直取）',
    neutralSubtitle: '采集器上报的 CPU 使用率',
    color: 'var(--color-chart-1)',
    format: pct,
    latest: (row) => row.cpu_pct,
    percentAxis: true,
  },
  {
    key: 'mem_pct',
    title: '内存使用率',
    subtitle: '/proc/meminfo',
    windowsSubtitle: 'Win32_OperatingSystem（TotalVisibleMemorySize / FreePhysicalMemory）',
    neutralSubtitle: '采集器上报的内存使用率',
    color: 'var(--color-chart-2)',
    format: pct,
    latest: (row) => row.mem_pct,
    percentAxis: true,
  },
  {
    key: 'disk_pct',
    title: '磁盘使用率',
    subtitle: '最大文件系统使用率',
    windowsSubtitle: 'Win32_LogicalDisk 最大本地盘使用率',
    neutralSubtitle: '最大文件系统使用率',
    color: 'var(--color-chart-3)',
    format: pct,
    latest: (row) => row.disk_pct,
    percentAxis: true,
  },
  {
    key: 'load_1',
    title: '负载（1m）',
    subtitle: 'loadavg 1 分钟',
    // Windows 的 load_1 是 ProcessorQueueLength（排队线程数），不是
    // 1 分钟负载均值；标题与副文案都要按平台改口。
    windowsTitle: '处理器队列长度',
    windowsSubtitle: 'ProcessorQueueLength（排队线程数，不是 1 分钟负载均值）',
    neutralSubtitle: '采集器上报的负载 / 队列指标',
    color: 'var(--color-chart-4)',
    // 走查第 7 轮 low：float32 最大值曾被 toFixed(2) 展开成
    // 「3.3999999521443642e+38」；3 位有效数字（3.40e38）长度可控。
    format: (value) => formatSignificant(value, 3),
    // 走查第 9 轮 low：轴 0..2e6 曾混排 500000 与 1.00e6；按轴 max 统一
    // 科学计数（0 | 5.00e5 | 1.00e6 | 1.50e6 | 2.00e6）。
    axisFormat: formatNumericAxisTick,
    latest: (row) => row.load_1,
    percentAxis: false,
  },
  {
    key: 'net_in_bps',
    title: '网络入',
    subtitle: '接收速率',
    windowsSubtitle: '接收速率',
    neutralSubtitle: '接收速率',
    color: 'var(--color-chart-5)',
    format: formatBps,
    latest: (row) => row.net_in_bps,
    percentAxis: false,
  },
  {
    key: 'net_out_bps',
    title: '网络出',
    subtitle: '发送速率',
    windowsSubtitle: '发送速率',
    neutralSubtitle: '发送速率',
    color: 'var(--color-chart-6)',
    format: formatBps,
    latest: (row) => row.net_out_bps,
    percentAxis: false,
  },
];

/**
 * 指标卡标题/副文案（走查第 15 轮 medium）。
 *
 * Linux 保留 /proc 与 loadavg 口径；Windows 换成真实采集源
 * （Win32_Processor LoadPercentage / Win32_OperatingSystem /
 * ProcessorQueueLength）；os 缺失时用中性文案，绝不再把 /proc 口径
 * 贴到平台未知的机器上。export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function metricCardText(
  key: MetricCardSpec['key'],
  os: string | null | undefined,
): { title: string; subtitle: string } {
  const card = METRIC_CARDS.find((item) => item.key === key);
  if (!card) {
    return { title: key, subtitle: '' };
  }
  const platform = metricPlatform(os);
  if (platform === 'linux') {
    return { title: card.title, subtitle: card.subtitle };
  }
  if (platform === 'windows') {
    return { title: card.windowsTitle ?? card.title, subtitle: card.windowsSubtitle };
  }
  return {
    title: key === 'load_1' ? '负载 / 处理器队列' : card.title,
    subtitle: card.neutralSubtitle,
  };
}

/**
 * 指标卡头「当前 / 最新」标签（走查第 15 轮 medium）。
 *
 * 旧实现只要窗口内最后一点存在就写「当前」，即使该点已停采 12 小时、
 * 同页已标注「已超过过期阈值 N 分钟」——值班员会把陈旧值当实时值。
 * 过期时保留窗口最后一点，但标签改成「最新（12 小时前）」；没有年龄
 * 摘要时退化为「当前（已过期）」。export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function metricValueLabel(
  windowValue: number | null,
  stale: boolean,
  latestSummary: string | null,
): string {
  const latest =
    latestSummary === null || latestSummary === '' ? '最新' : `最新（${latestSummary}）`;
  if (windowValue === null) {
    return latest;
  }
  if (!stale) {
    return '当前';
  }
  return latestSummary === null || latestSummary === '' ? '当前（已过期）' : latest;
}

/**
 * 单机指标卡头的最终标签（走查第 17 轮 medium）。
 *
 * 窗口内有最后一点时，过期判定与年龄摘要都必须来自**该点自己**；不能
 * 复用 overview.latest 的 selectedSampleStale——latest 时钟超前时它会
 * 被强制为 false，12 小时前的窗口点会被标成「当前 42.0%」。窗口内无点
 * （windowValue=null）时才回落到 overview.latest 的口径。
 * export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function metricCardValueLabel(
  windowValue: number | null,
  windowPointStale: boolean,
  windowPointSummary: string | null,
  selectedSampleStale: boolean,
  selectedLatestSummary: string | null,
): string {
  return metricValueLabel(
    windowValue,
    windowValue !== null ? windowPointStale : selectedSampleStale,
    windowValue !== null ? windowPointSummary : selectedLatestSummary,
  );
}

/**
 * 监控页「无服务器指标」空态文案（RBAC 第 12 轮走查 medium）。
 *
 * 添加资产要求 asset:write、开启采集要求 monitor:manage（CollectorCard
 * 的 canConfigure 同源）。运维工程师两项都没有：此前空态无条件让他去
 * 「服务器」页添加资产并开启采集，而同页采集卡又显示只读，主工作流没有
 * 可执行的下一步。无任一权限时改为「请联系管理员配置资产与采集」；
 * 管理员保留原文案。export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function monitorEmptyDescription(permissions: readonly Permission[]): string {
  // 两个动作各有独立权限：添加资产要 asset:write，开启采集要 monitor:manage。
  // 只有两者都在手时才给出「添加资产并开启采集」的完整下一步；任一缺失
  // 都只能联系管理员，否则仍是把用户引向必然 403 的操作。
  const canAddAssets = hasAnyPermission(permissions, [PERMS.assetsWrite]);
  const canManageCollector = hasAnyPermission(permissions, [PERMS.alertsWrite]);
  return canAddAssets && canManageCollector
    ? '暂无服务器指标：请先在「服务器」页添加资产并开启采集'
    : '暂无服务器指标：请联系管理员配置资产与采集';
}

/**
 * 「暂无服务器指标」资产空态是否渲染（走查第 5 轮 medium）。
 *
 * overview 接口失败时页面已经有错误横幅；此前只排除 isLoading，加载失败后
 * servers 为空会被当成「没有资产」，再叠加一条「请先添加资产并开启采集」，
 * 把接口故障误报成配置缺失。错误态只保留错误横幅。
 * export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function monitorAssetEmptyVisible(
  effectiveServerId: string | null,
  overviewStatus: { isLoading: boolean; isError: boolean },
): boolean {
  return effectiveServerId === null && !overviewStatus.isLoading && !overviewStatus.isError;
}

/**
 * 采集器概要卡状态（走查第 11 轮 medium）。
 *
 * 旧实现 `collectorQuery.data?.enabled ?? true` 在查询慢/失败时一律显示
 * 「运行中」：实际配置已暂停也会被误报，值班人员据此判断采集正常。
 * 现在状态只信真实配置——优先 collector 接口，未就绪时用 overview 响应
 * 里同源的 collector（monitor_handlers.go 一并返回）；两者都没有时，
 * 加载中显示「—」、失败显示「状态未知」，绝不用 true 兜底。
 * export 仅作单测钩子。
 */
export type CollectorSummaryStatus = 'loading' | 'unknown' | 'running' | 'paused';

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function collectorSummaryStatus(
  config: { enabled: boolean } | null | undefined,
  pending: boolean,
  failed: boolean,
): CollectorSummaryStatus {
  if (config) {
    return config.enabled ? 'running' : 'paused';
  }
  if (failed) {
    return 'unknown';
  }
  if (pending) {
    return 'loading';
  }
  return 'unknown';
}

const COLLECTOR_STATUS_LABEL: Record<CollectorSummaryStatus, string> = {
  loading: '—',
  unknown: '状态未知',
  running: '运行中',
  paused: '已暂停',
};

/**
 * 采集配置取值顺序（走查第 13 轮 medium）。
 *
 * overview 每 30s 刷新，是跨会话状态的唯一新鲜来源；collector 接口只在
 * 首屏拉一次，必须退居兜底。旧顺序（collector 优先）会让管理员 B 在
 * 另一会话暂停采集后，A 的页面继续按陈旧的「运行中」渲染横幅、轮询门控
 * 与过期阈值。两者都缺时返回 null，由调用方显示「—」/「状态未知」。
 * export 仅作单测钩子。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function resolveCollectorConfig(
  overviewConfig: CollectorConfig | null | undefined,
  fallback: CollectorConfig | null | undefined,
): CollectorConfig | null {
  return overviewConfig ?? fallback ?? null;
}

export default function MonitorPage() {
  const permissions = usePermissions();
  const [serverId, setServerId] = useState<string | null>(null);
  const [minutes, setMinutes] = useState(60);
  // 集群态势两图（图表化本轮）：趋势窗口与服务端同权（monitor:read）。
  // 两图均只在「已有被采集服务器」时渲染；servers 为空沿用既有空态。
  const canReadMetrics = hasAnyPermission(permissions, [PERMS.metricsRead]);
  const [trendWindow, setTrendWindow] = useState<ChartWindow>('24h');

  // 采集配置查询只保留为首屏兜底：它没有轮询，管理员在另一会话改配置后
  // 会一直停在打开页面时的快照（走查第 13 轮 medium）。展示、轮询门控与
  // 过期阈值都以 30s 轮询的 overview.collector 为准。
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

  // 走查第 2 轮 medium：overview 此前只在进入页面时拉一次，Top12 与
  // 概要卡停在快照；折线/趋势却每 30s 刷新，同页出现新旧两套数据。
  // 走查第 13 轮 medium：overview 恒定 30s 轮询，采集暂停时也不停——
  // 它是跨会话采集状态的唯一新鲜来源，停掉就既感知不到「别的管理员
  // 暂停」也感知不到「恢复」。真正需要停的是单机折线与集群趋势。
  const overviewQuery = useQuery({
    queryKey: qk.monitorOverview,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/monitor/overview');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    refetchInterval: METRIC_REFRESH_MS,
  });
  const overview: MonitorOverview | undefined = overviewQuery.data;

  // 第 11 轮 medium 把「未知」与「运行中」分开：两者都没有时显示「—」/
  // 「状态未知」，绝不用 true 兜底。第 13 轮再改优先级：overview（30s
  // 新鲜）优先，collector 接口只补首屏空档。旧实现优先陈旧的
  // collectorQuery，跨会话暂停后横幅/门控/阈值全部按旧配置计算。
  const collectorConfig = resolveCollectorConfig(overview?.collector, collectorQuery.data);
  // 门控：配置未就绪时保守按开启处理（避免首帧把 30s 刷新关掉）。
  const collectorPollingEnabled = collectorConfig?.enabled ?? true;
  const staleAfterMs = staleThresholdMs(collectorConfig?.interval_seconds);

  // 同一份新鲜配置写回 qk.collector 缓存：页面底部「指标采集」卡与
  // MonitorPage 各持一个 useQuery，同一 key 共享缓存；只更新本页的
  // collectorConfig 会让底部卡继续显示已过期的开关状态。
  const queryClient = useQueryClient();
  const overviewCollector = overview?.collector;
  useEffect(() => {
    if (!overviewCollector) {
      return;
    }
    const cached = queryClient.getQueryData<typeof overviewCollector>(qk.collector);
    if (
      cached?.enabled === overviewCollector.enabled &&
      cached.interval_seconds === overviewCollector.interval_seconds
    ) {
      return;
    }
    queryClient.setQueryData(qk.collector, overviewCollector);
  }, [overviewCollector, queryClient]);

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

  // 第 11 轮 medium：collector 接口慢/失败时用 overview 响应里同源的
  // collector 兜底（monitor_handlers.go 一次返回），两者都没有才显示
  // 「—」/「状态未知」；不再用 `?? true` 把未知状态谎报成运行中。
  const collectorStatus = collectorSummaryStatus(
    collectorConfig,
    collectorQuery.isLoading || overviewQuery.isLoading,
    collectorQuery.isError || overviewQuery.isError,
  );
  // 服务器选择器仅列有 server_metrics_latest 行的资产（8.7：overview.servers
  // 即 latest JOIN 结果）。useMemo 稳定引用（exhaustive-deps）。
  const servers = useMemo(() => overview?.servers ?? [], [overview]);
  // 走查第 2 轮 low：后端按 UUID 返回，选择器此前也按 UUID 排序、不可搜索，
  // 默认还可能落在 0% 的空闲机上。选项按显示名排序（localeCompare）；
  // 未显式选择时默认展示当前 CPU 最高的机器，用户一旦选择就固定下来。
  const serverOptions = useMemo(() => buildServerOptions(servers), [servers]);

  // 采样年龄走 30s 时钟（采集暂停时查询不刷新，过期标记仍需前进）。
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  // 走查第 14 轮 medium：值班机与服务器时钟可能相差几分钟。overview 的
  // server_time 是服务端生成时刻，dataUpdatedAt 是浏览器收到时刻，两者
  // 之差即时钟偏移；所有「N 前/过期」都用 serverNowMs 计算，避免把未来
  // 样本显示成「1 秒前」。overview 缺失时回落浏览器时钟。
  const overviewServerMs = overview?.server_time ? Date.parse(overview.server_time) : Number.NaN;
  const serverClockOffsetMs =
    Number.isFinite(overviewServerMs) && overviewQuery.dataUpdatedAt > 0
      ? overviewServerMs - overviewQuery.dataUpdatedAt
      : 0;
  const serverNowMs = nowMs + serverClockOffsetMs;
  const sampleIntervalMs = collectorConfig ? collectorConfig.interval_seconds * 1000 : null;

  // 采样时间超前服务端时钟（默认容差 1 秒，不再按整个采集间隔）的机器
  // 不参与默认选中与 Top12 排名（其值可能是错误时钟写入的未来样本）。
  // 趋势接口对同一批样本按 now 严格钳制，三处必须同口径（走查第 17 轮
  // medium：600s 间隔下 +9min 样本曾被当作当前值）。全部超前时仍回落
  // 到全量，避免页面变成空选择器。
  const futureSkewedServers = useMemo(
    () => servers.filter((server) => isFutureSample(server.created_at, serverNowMs)),
    [servers, serverNowMs],
  );
  const rankableServers = useMemo(() => {
    if (serverNowMs <= 0) {
      return servers;
    }
    return servers.filter((server) => !isFutureSample(server.created_at, serverNowMs));
  }, [servers, serverNowMs]);
  const defaultServerId = useMemo(
    () => pickDefaultServerId(rankableServers.length > 0 ? rankableServers : servers),
    [rankableServers, servers],
  );

  // 集群压力分桶趋势（GET /api/monitor/trend，窗口 24h/7d 由服务端写死桶宽）。
  // 无服务器/无权限时不发请求；窗口切换只切 key，另一档缓存保留。
  const trendQuery = useQuery({
    queryKey: qk.monitorTrend(trendWindow),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/monitor/trend', {
        params: { query: { window: trendWindow } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: canReadMetrics && servers.length > 0,
    // 聚合桶宽 1h/6h；采集暂停时折线不再刷新（与单机折线同口径）。
    refetchInterval: collectorPollingEnabled ? 60_000 : false,
  });

  // 选中失效（如服务器被删除）时渲染期回落到默认最热机，不经 effect setState。
  const hasExplicitServer =
    serverId !== null && servers.some((server) => server.server_id === serverId);
  const effectiveServerId = hasExplicitServer
    ? serverId
    : (defaultServerId ?? servers[0]?.server_id ?? null);
  const selectedServer = servers.find((server) => server.server_id === effectiveServerId) ?? null;
  const selectedSampleAheadMs = selectedServer
    ? sampleTimeAheadMs(selectedServer.created_at, serverNowMs)
    : null;
  const selectedSampleSkewed = selectedServer
    ? isFutureSample(selectedServer.created_at, serverNowMs)
    : false;
  // 超前样本不是「过期」：不把负年龄钳成 0 后写「1 秒前」。
  const selectedSampleAge =
    selectedServer && !selectedSampleSkewed
      ? staleAgeMs(selectedServer.created_at, serverNowMs)
      : null;
  const selectedSampleStale =
    selectedServer && !selectedSampleSkewed
      ? isStale(selectedServer.created_at, serverNowMs, staleAfterMs)
      : false;
  // 空态/卡头共用的「最近一次采样」摘要：正常给年龄，超前给时钟错误提示。
  const selectedLatestSummary = (() => {
    if (!selectedServer) {
      return null;
    }
    if (selectedSampleSkewed && selectedSampleAheadMs !== null) {
      return `时间超前服务端时钟 ${formatCoverageDuration(selectedSampleAheadMs)}`;
    }
    return selectedSampleAge === null ? null : `${formatSampleAge(selectedSampleAge)}前`;
  })();
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
    refetchInterval: collectorPollingEnabled ? METRIC_REFRESH_MS : false,
  });

  // 走查第 13 轮 medium：采集暂停后折线不再刷新，时间窗却仍随时钟右移——
  // 窗口显示 21:59–22:59、最新采样停在 20:34，70 个点被钳到左缘，横轴
  // 时间与采样时间脱节。暂停时把窗口终点冻结在最后一个采样点；
  // 「最新采样 N 分钟前」仍按服务端 now（serverNowMs）计算，过期标记继续
  // 前进（第 14 轮把时钟基准从浏览器换成服务端）。
  const lastSampleMs = useMemo(
    () => latestTimestamp(metricsQuery.data?.points ?? []),
    [metricsQuery.data],
  );
  const chartWindow = metricChartWindow(
    minutes,
    serverNowMs,
    lastSampleMs,
    collectorConfig?.enabled === false,
  );

  // 卡片「当前」必须与折线最后一点同源（走查第 14 轮 medium）：overview
  // 的 latest 可能包含未来时间戳（另一个进程/客户端时钟写入），而折线
  // 窗口已按服务端时间裁剪。这里从窗口点集倒序取第一个「落在 chartWindow
  // 内且不超前」的点；没有窗口点时卡片改显示「最新 + 年龄」。
  const lastWindowPoint = useMemo(() => {
    const points = metricsQuery.data?.points ?? [];
    for (let index = points.length - 1; index >= 0; index--) {
      const point = points[index];
      if (!point) {
        continue;
      }
      const time = Date.parse(point.created_at);
      if (!Number.isFinite(time) || time < chartWindow.start || time > chartWindow.end) {
        continue;
      }
      if (!isFutureSample(point.created_at, serverNowMs)) {
        return point;
      }
    }
    return null;
  }, [metricsQuery.data, serverNowMs, chartWindow.start, chartWindow.end]);

  // 卡头「当前/最新」只看窗口内最后一点自己的年龄（走查第 17 轮
  // medium）：overview.latest 时钟超前时 selectedSampleStale 被强制为
  // false，若复用它，12 小时前的窗口点会被标成「当前 42.0%」，与同页
  // 「采样时间超前 9 分钟」和「仅 1 个采样点（11/20 18:10）」矛盾。
  // lastWindowPoint 已在上面排除超前点，这里按它相对服务端 now 的年龄
  // 单独判过期，并生成同源的年龄摘要。
  const lastWindowPointAgeMs = lastWindowPoint
    ? staleAgeMs(lastWindowPoint.created_at, serverNowMs)
    : null;
  const lastWindowPointStale = lastWindowPointAgeMs !== null && lastWindowPointAgeMs > staleAfterMs;
  const lastWindowPointSummary =
    lastWindowPointAgeMs === null ? null : `${formatSampleAge(lastWindowPointAgeMs)}前`;

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

  // 截断时页头写明「仅最近 X 小时」，与卡片覆盖提示同口径；不把接口
  // 上限造成的缺失说成采集中断（第 10 轮 high）。
  const truncatedSpanText = useMemo(() => {
    const points = metricsQuery.data?.points ?? [];
    if (!metricsQuery.data?.truncated || points.length < 2) {
      return null;
    }
    const first = points[0];
    const last = points[points.length - 1];
    if (!first || !last) {
      return null;
    }
    const from = Date.parse(first.created_at);
    const to = Date.parse(last.created_at);
    if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) {
      return null;
    }
    return formatCoverageDuration(to - from);
  }, [metricsQuery.data]);

  const firing = overview?.firing;
  const lastInspection = overview?.last_inspection ?? null;
  // 第 11 轮 low：概要卡必须能看出报告是不是今天的。巡检 5 天没跑时
  // 仍显示「87分」会让人误判为刚巡检过；第二行给报告时间，超过 24h
  // 用 warning 色并注明「N 天前」。
  const inspectionCreatedMs = lastInspection
    ? Date.parse(lastInspection.created_at ?? '')
    : Number.NaN;
  const inspectionAgeMs = Number.isFinite(inspectionCreatedMs)
    ? Math.max(0, serverNowMs - inspectionCreatedMs)
    : null;
  const inspectionStale = inspectionAgeMs !== null && inspectionAgeMs > 24 * 3_600_000;
  const inspectionAgeText =
    inspectionAgeMs !== null && inspectionAgeMs >= 3_600_000
      ? formatCoverageDuration(inspectionAgeMs)
      : null;

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
              {lastInspection ? (
                <Typography.Text
                  data-testid="inspection-report-time"
                  type={inspectionStale ? 'warning' : 'secondary'}
                  style={{ fontSize: 12 }}
                >
                  报告 {formatInspectionTime(lastInspection.created_at)}
                  {inspectionAgeText ? `（${inspectionAgeText}前）` : ''}
                </Typography.Text>
              ) : null}
            </Card>
          </Col>
          <Col xs={12} md={6}>
            <Card size="small">
              <Statistic
                title="采集器"
                value={COLLECTOR_STATUS_LABEL[collectorStatus]}
                styles={{ content: { fontSize: 22 } }}
                suffix={
                  collectorConfig ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      每 {collectorConfig.interval_seconds}s 一轮
                    </Typography.Text>
                  ) : collectorStatus === 'unknown' ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      采集配置查询失败
                    </Typography.Text>
                  ) : null
                }
              />
            </Card>
          </Col>
        </Row>
      )}

      {/* 集群态势（图表化本轮）：servers[] 为空时不渲染，沿用下方既有空态。
          两图随 /monitor 收敛到 monitor:read；无权限整块不渲染。 */}
      {canReadMetrics && servers.length > 0 ? (
        <>
          <Typography.Title level={4} className="opsmind-admin-section-title">
            集群态势
          </Typography.Title>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            先看整体压力有没有恶化，再定位此刻最紧张的机器。
          </Typography.Text>
          <Row gutter={[12, 12]} style={{ marginTop: 8 }}>
            {/* 并排断点用 antd xl(≥1200)：lg(992–1199) 时两卡各只剩约 346px
                绘图区，Top 12 的服务器名会被截到 7 个字符（batch-04/batch-05
                无法区分）——xl 以下改为上下堆叠，两图都保留完整宽度。
                更窄的屏幕仍由 TopServerBarChart 的中段截断保留尾部编号。 */}
            <Col xs={24} xl={12}>
              <Card size="small" title="集群压力趋势">
                {/* key 随「首份数据就绪」切换一次：图表组件在加载态只渲染
                    skeleton（测量 ref 未挂载），若挂载过早 ResizeObserver /
                    宽度测量会一直停在 fallback 宽度，窄屏下 SVG 按 560 布局。
                    数据到达后重挂载，让容器宽度测量从真实 DOM 开始。 */}
                <ClusterTrendChart
                  key={trendQuery.data?.points.length ? 'trend-ready' : 'trend-pending'}
                  points={trendQuery.data?.points ?? []}
                  window={trendWindow}
                  {...(trendQuery.data ? { bucketSeconds: trendQuery.data.bucket_seconds } : {})}
                  firstSample={trendQuery.data?.first_sample ?? null}
                  lastSample={trendQuery.data?.last_sample ?? null}
                  now={serverNowMs}
                  futureSkewedServers={futureSkewedServers.length}
                  onWindowChange={setTrendWindow}
                  loading={trendQuery.isLoading}
                  error={
                    trendQuery.isError ? (
                      <QueryErrorState
                        error={trendQuery.error}
                        onRetry={() => {
                          void trendQuery.refetch();
                        }}
                      />
                    ) : undefined
                  }
                />
              </Card>
            </Col>
            <Col xs={24} xl={12}>
              <Card size="small" title="最紧张服务器 Top 12">
                <TopServerBarChart
                  servers={servers}
                  loading={overviewQuery.isLoading}
                  staleAfterMs={staleAfterMs}
                  now={serverNowMs}
                  height={260}
                />
              </Card>
            </Col>
          </Row>
        </>
      ) : null}

      <Typography.Title level={4} className="opsmind-admin-section-title">
        单机详情
      </Typography.Title>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        选择服务器与时间窗，查看 CPU / 内存 / 磁盘 / 负载 / 网络六项历史折线。
      </Typography.Text>

      <Flex gap={12} wrap="wrap" align="center" style={{ marginTop: 4 }}>
        <Select
          style={{ minWidth: 260 }}
          placeholder="选择服务器"
          aria-label="选择服务器"
          showSearch
          optionFilterProp="label"
          value={effectiveServerId}
          onChange={(value) => {
            setServerId(value);
          }}
          options={serverOptions}
          notFoundContent={<EmptyState description="暂无已采集的服务器（等待首轮采集）" />}
        />
        <Segmented
          aria-label="选择单机时间范围"
          value={minutes}
          onChange={(value) => {
            setMinutes(value as number);
          }}
          options={TIME_WINDOWS.map((window) => ({ label: window.label, value: window.minutes }))}
        />
        {/* 走查第 2 轮：单机卡只写「当前」时，30 分钟前的采样会被当成实时；
            这里明确给出采样时刻与年龄（超过按采集间隔推导的阈值标警告）。 */}
        {selectedServer ? (
          <Typography.Text
            type={selectedSampleStale || selectedSampleSkewed ? 'warning' : 'secondary'}
            style={{ fontSize: 12 }}
          >
            最新采样 {formatSampleTime(selectedServer.created_at)}
            {selectedSampleSkewed && selectedSampleAheadMs !== null
              ? `（采样时间超前 ${formatCoverageDuration(selectedSampleAheadMs)}，请检查时钟）`
              : selectedSampleAge !== null
                ? `（${formatSampleAge(selectedSampleAge)}前${
                    selectedSampleStale
                      ? `，已超过过期阈值 ${Math.max(1, Math.round(staleAfterMs / 60_000))} 分钟`
                      : ''
                  }）`
                : ''}
          </Typography.Text>
        ) : null}
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
            返回点集已截断（{truncatedSpanText ? `仅最近 ${truncatedSpanText}，` : ''}
            {metricsQuery.data.points.length} 点）
          </Typography.Text>
        ) : null}
      </Flex>

      {monitorAssetEmptyVisible(effectiveServerId, overviewQuery) ? (
        <EmptyState description={monitorEmptyDescription(permissions)} />
      ) : metricsQuery.isError ? (
        <QueryErrorState
          error={metricsQuery.error}
          onRetry={() => {
            void metricsQuery.refetch();
          }}
        />
      ) : null}

      {collectorConfig?.enabled === false && effectiveServerId !== null ? (
        <Alert type="warning" showIcon title="采集已暂停：折线展示历史数据，不再自动刷新。" />
      ) : null}

      {effectiveServerId !== null && !metricsQuery.isError ? (
        <Row gutter={[12, 12]}>
          {METRIC_CARDS.map((card) => {
            // 走查第 15 轮 medium：副文案随资产平台切换（Windows 不能
            // 再写 /proc/stat、/proc/meminfo、loadavg 1 分钟）。
            const copy = metricCardText(card.key, selectedServer?.os);
            const overviewValue = selectedServer ? card.latest(selectedServer) : null;
            const windowValue =
              lastWindowPoint && typeof lastWindowPoint[card.key] === 'number'
                ? (lastWindowPoint[card.key] as number)
                : null;
            // 窗口内的最后一点优先：与折线同源，未来样本已被倒序扫描排除。
            // 窗口内无点时不再拿 overview.latest 冒充「当前」，改为
            // 「最新 + 年龄」（走查第 14 轮 low）。
            const shownValue = windowValue ?? overviewValue;
            // 走查第 15 轮 medium：停采 12 小时但 24h 窗口内仍有历史点时，
            // 旧实现仍写「当前 42.0%」，与同页「已超过过期阈值」矛盾。
            // 走查第 17 轮 medium：过期判定必须用窗口最后一点自己的年龄，
            // 不能复用 overview.latest 的 selectedSampleStale——latest 时钟
            // 超前时后者被强制为 false，会把 12 小时前的点标成「当前」。
            const valueLabel = metricCardValueLabel(
              windowValue,
              lastWindowPointStale,
              lastWindowPointSummary,
              selectedSampleStale,
              selectedLatestSummary,
            );
            return (
              <Col xs={24} lg={12} xl={8} key={card.key}>
                <Card
                  size="small"
                  title={
                    <Space size={8}>
                      <span>{copy.title}</span>
                      {shownValue !== null ? (
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                          {/* 契约 8.7 ⑥（FE-P2 六期）：指标当前值（含单位）复制。
                              过期值时按钮说明同步改「最新值」，不再称当前。 */}
                          {valueLabel} {card.format(shownValue)}{' '}
                          <CopyButton
                            text={card.format(shownValue)}
                            label={`复制${copy.title}${
                              valueLabel.startsWith('当前') ? '当前值' : '最新值'
                            }`}
                          />
                        </Typography.Text>
                      ) : null}
                    </Space>
                  }
                >
                  <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 4 }}>
                    {copy.subtitle}
                  </Typography.Paragraph>
                  <MetricChart
                    points={seriesByMetric[card.key] ?? []}
                    label={copy.title}
                    formatValue={card.format}
                    {...(card.axisFormat ? { formatAxisTick: card.axisFormat } : {})}
                    color={card.color}
                    percentAxis={card.percentAxis}
                    loading={metricsQuery.isLoading}
                    windowStart={chartWindow.start}
                    windowEnd={chartWindow.end}
                    truncated={metricsQuery.data?.truncated ?? false}
                    // 断档判定优先用观测到的中位采样间隔；点数太少时用
                    // 采集器配置兜底（见 metricSeries.ts）。配置取与门控
                    // 同源的 collectorConfig，避免跨会话改间隔后用旧值。
                    sampleIntervalMs={sampleIntervalMs}
                    hasAnySample={selectedServer !== null}
                    latestSampleSummary={selectedLatestSummary}
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
