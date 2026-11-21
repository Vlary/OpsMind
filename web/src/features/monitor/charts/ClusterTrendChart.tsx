// 集群压力分桶趋势（近 24 小时 / 7 天）。
//
// 数据：GET /api/monitor/trend 的 points[]（桶宽由服务端写死：
// 24h→3600s、7d→21600s；无样本桶为 null，折线在此断开，不连点）。
// 交互：24h/7d 两档切换；hover 或键盘聚焦单桶显示 P95 / 均值 / 峰值 /
// 采样台数 / 样本数。

import { Segmented } from 'antd';
import { useMemo, useState } from 'react';
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';

import { buildPolyline, formatChartTime, mapY, percentYScale, tickValues } from '../chartMath';
import ChartFrame from './ChartFrame';
import ChartTooltip from './ChartTooltip';
import {
  axisLabelPlacement,
  bandLayout,
  defaultBucketSeconds,
  estimateTextWidth,
  finiteOrNull,
  labelTickIndices,
  nearestBandIndex,
  percentText,
  sortClusterTrend,
  splitLineRuns,
  trendCoverageNote,
  type AxisLabelEdge,
  type ChartWindow,
  type ClusterTrendPoint,
} from './chartModel';
import { useChartWidth } from './useChartWidth';
import { useNowMs } from './useNowMs';

export interface ClusterTrendChartProps {
  /** monitor/trend 的 points（nullable 桶）。 */
  points: readonly ClusterTrendPoint[];
  /** 受控时间窗；不传则组件内部维护。 */
  window?: ChartWindow;
  defaultWindow?: ChartWindow;
  onWindowChange?: (window: ChartWindow) => void;
  /** 服务端返回的桶宽（秒）；缺省按窗口取契约写死值。 */
  bucketSeconds?: number;
  loading?: boolean;
  error?: ReactNode;
  emptyMessage?: ReactNode;
  /** 当前时刻（epoch ms）；生产传服务端时间，缺省走浏览器 60s 时钟。 */
  now?: number;
  /**
   * 采样时间超前服务端时钟的服务器数（与 Top 12 同口径）。后端聚合已按
   * 服务端 now 排除这些机器的未来样本，副标题明说口径，避免值班员看到
   * 趋势台数少于 Top 12 时以为丢数据（走查第 16 轮 medium）。
   */
  futureSkewedServers?: number;
  /**
   * 窗口内实际最早/最晚采样时刻（GET /api/monitor/trend 的
   * first_sample/last_sample，已按服务端 now 钳制）。用于计算真实数据
   * 跨度；缺失时覆盖提示退回「覆盖桶跨度约 …（首末桶未满）」。
   */
  firstSample?: string | null;
  lastSample?: string | null;
  height?: number;
}

type SeriesKey = 'cpu_p95' | 'cpu_avg' | 'cpu_max';

interface SeriesSpec {
  key: SeriesKey;
  label: string;
  color: string;
  strokeWidth: number;
}

const SERIES: readonly SeriesSpec[] = [
  { key: 'cpu_p95', label: 'P95', color: 'var(--color-primary)', strokeWidth: 2 },
  { key: 'cpu_avg', label: '均值', color: 'var(--color-success)', strokeWidth: 1.4 },
  { key: 'cpu_max', label: '峰值', color: 'var(--color-warning)', strokeWidth: 1.2 },
];

const FALLBACK_WIDTH = 560;
const PADDING_LEFT = 40;
const PADDING_RIGHT = 10;
const PADDING_TOP = 26;
const PADDING_BOTTOM = 22;

const monthDayFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
});

const fullTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

function formatBucketLabel(iso: string, window: ChartWindow): string {
  const time = Date.parse(iso);
  if (!Number.isFinite(time)) {
    return '—';
  }
  // 24h 窗口跨午夜（23:00 属于昨天），与单机折线同口径带月/日；
  // 7d 保留 MM/DD，避免 6 小时桶标签过长。
  return window === '7d' ? monthDayFormatter.format(time) : formatChartTime(time, 24 * 3_600_000);
}

function formatFullTime(iso: string): string {
  const time = Date.parse(iso);
  return Number.isFinite(time) ? fullTimeFormatter.format(time) : '时间未知';
}

function windowBucketLabel(seconds: number): string {
  if (seconds % 3600 === 0) {
    return `${seconds / 3600} 小时`;
  }
  return `${Math.round(seconds / 60)} 分钟`;
}

export default function ClusterTrendChart({
  points,
  window,
  defaultWindow = '24h',
  onWindowChange,
  bucketSeconds,
  loading = false,
  error,
  emptyMessage = '暂无集群采集数据',
  now,
  futureSkewedServers = 0,
  firstSample = null,
  lastSample = null,
  height = 260,
}: ClusterTrendChartProps) {
  const [containerRef, measuredWidth] = useChartWidth();
  const nowMs = useNowMs(now);
  const [innerWindow, setInnerWindow] = useState<ChartWindow>(defaultWindow);
  const [hovered, setHovered] = useState<number | null>(null);
  // 触屏没有 hover：点按某个桶钉住 tooltip（再点同一个桶取消，点绘图区
  // 外取消）。用 bucket_start 做身份，窗口切换/刷新重排后仍指向同一个桶。
  const [pinnedBucket, setPinnedBucket] = useState<string | null>(null);

  const activeWindow = window ?? innerWindow;
  const providedBucketSeconds = finiteOrNull(bucketSeconds);
  const activeBucketSeconds =
    providedBucketSeconds !== null && providedBucketSeconds > 0
      ? providedBucketSeconds
      : defaultBucketSeconds(activeWindow);
  const sorted = useMemo(() => sortClusterTrend(points), [points]);

  const width = measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH;
  const plotWidth = Math.max(0, width - PADDING_LEFT - PADDING_RIGHT);
  const plotHeight = Math.max(0, height - PADDING_TOP - PADDING_BOTTOM);
  const bands = bandLayout(sorted.length, plotWidth, 0.18);

  const valuesBySeries = useMemo(() => {
    const result: Record<SeriesKey, Array<number | null>> = {
      cpu_p95: [],
      cpu_avg: [],
      cpu_max: [],
    };
    for (const point of sorted) {
      result.cpu_p95.push(finiteOrNull(point.cpu_p95));
      result.cpu_avg.push(finiteOrNull(point.cpu_avg));
      result.cpu_max.push(finiteOrNull(point.cpu_max));
    }
    return result;
  }, [sorted]);

  let dataMax = 0;
  for (const series of SERIES) {
    for (const value of valuesBySeries[series.key]) {
      if (value !== null && value > dataMax) {
        dataMax = value;
      }
    }
  }
  // CPU 使用率恒为百分比：99% 的满载必须贴着图顶，不能被 niceCeil 扩到
  // 200% 压成半高；只有数据真超过 100% 才允许扩张。
  const scale = percentYScale(dataMax);

  // 采集停摆后服务端仍补满 24/28 个 null 桶（只有 bucket_start 有效）。
  // 此时渲染 SVG 会只剩坐标轴、没有任何折线，运维无法区分「采集停了」
  // 与「图表坏了」——按「窗口内无样本」走空态。
  const hasAnySample = useMemo(
    () => SERIES.some((series) => valuesBySeries[series.key].some((value) => value !== null)),
    [valuesBySeries],
  );

  // 走查第 15 轮 low：非空桶少于总桶数时解释空桶（刚上线/保留不足/
  // 采集中断），只在部分空桶时出现，全空仍走空态。
  // 走查第 17 轮 low：数据跨度优先用接口附带的实际首末采样时刻；桶跨度
  // 只作兜底并明确写「覆盖桶跨度…（首末桶未满）」。
  const sampleSpanMs = useMemo(() => {
    const first = firstSample ? Date.parse(firstSample) : Number.NaN;
    const last = lastSample ? Date.parse(lastSample) : Number.NaN;
    if (!Number.isFinite(first) || !Number.isFinite(last) || last < first) {
      return null;
    }
    return last - first;
  }, [firstSample, lastSample]);
  const coverageNote = useMemo(() => {
    const nonEmpty = sorted.map((_, index) =>
      SERIES.some((series) => valuesBySeries[series.key][index] !== null),
    );
    return trendCoverageNote(nonEmpty, activeBucketSeconds, sampleSpanMs);
  }, [sorted, valuesBySeries, activeBucketSeconds, sampleSpanMs]);

  // 走查第 16 轮 medium：时钟超前的机器已由后端按服务端 now 排除，副标题
  // 必须明说「有几台未参与」，否则趋势台数比 Top 12 少时无法自证口径。
  const futureSkewedNote =
    Number.isFinite(futureSkewedServers) && futureSkewedServers > 0
      ? `${Math.round(
          futureSkewedServers,
        )} 台采样时间超前服务端时钟，未参与趋势统计（与 Top 12 同口径）。`
      : null;

  const tickIndices = labelTickIndices(sorted.length, width < 520 ? 4 : 7);
  const pinnedIndex =
    pinnedBucket === null ? -1 : sorted.findIndex((point) => point.bucket_start === pinnedBucket);
  // hover 优先（鼠标/键盘），离开后回落到点按钉住的桶。
  const activeIndex = hovered ?? (pinnedIndex >= 0 ? pinnedIndex : null);
  const activePoint = activeIndex !== null ? sorted[activeIndex] : undefined;

  // 未走完的最后一桶（end > now）标「进行中」（走查第 14 轮 low）：
  // 服务端把 to 对齐到当前桶结束，最后桶只统计了已过去的部分，不能与
  // 完整桶看起来同口径。now 未初始化（首帧/SSR）时不标。
  const lastBucket = sorted[sorted.length - 1];
  const lastBucketEnd = lastBucket
    ? Date.parse(lastBucket.bucket_start) + activeBucketSeconds * 1000
    : Number.NaN;
  const inProgressIndex =
    nowMs > 0 && Number.isFinite(lastBucketEnd) && lastBucketEnd > nowMs ? sorted.length - 1 : -1;
  const ariaLabel = `集群压力分桶趋势，近 ${activeWindow === '7d' ? '7 天' : '24 小时'}，每桶 ${windowBucketLabel(
    activeBucketSeconds,
  )}，共 ${sorted.length} 个桶，纵轴 0 到 ${Math.round(scale.max)}%，显示 CPU P95、均值与峰值${
    inProgressIndex >= 0 ? '，最后一桶进行中' : ''
  }`;

  function changeWindow(next: ChartWindow): void {
    if (window === undefined) {
      setInnerWindow(next);
    }
    onWindowChange?.(next);
  }

  /**
   * 触点 → 最近桶下标（第 11 轮 medium）。与告警分布同一套 nearest-x
   * 语义：指针事件由根 svg 统一接收，窄屏下由触点 x 决定桶，而不是由
   * 某个约 11px 宽的透明矩形决定。
   */
  function indexFromSvgPointer(event: ReactMouseEvent<SVGSVGElement>): number {
    const rect = event.currentTarget.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0 || plotWidth <= 0) {
      return -1;
    }
    const svgX = ((event.clientX - rect.left) / rect.width) * width;
    const svgY = ((event.clientY - rect.top) / rect.height) * height;
    if (
      svgX < PADDING_LEFT ||
      svgX > PADDING_LEFT + plotWidth ||
      svgY < PADDING_TOP ||
      svgY > PADDING_TOP + plotHeight
    ) {
      return -1;
    }
    return nearestBandIndex(svgX - PADDING_LEFT, plotWidth, sorted.length);
  }

  const windowSwitch = (
    <div role="group" aria-label="选择趋势时间范围">
      <Segmented
        size="small"
        value={activeWindow}
        options={[
          { label: '近 24 小时', value: '24h' },
          { label: '近 7 天', value: '7d' },
        ]}
        onChange={(value) => {
          changeWindow(value as ChartWindow);
        }}
      />
    </div>
  );

  return (
    <ChartFrame
      loading={loading}
      error={error}
      empty={sorted.length === 0 || !hasAnySample}
      emptyMessage={
        sorted.length === 0
          ? emptyMessage
          : '窗口内暂无样本：采集可能已停摆，请检查采集器状态与最近上报时间'
      }
      height={height}
      actions={windowSwitch}
      subtitle={
        <>
          {`CPU 使用率分桶（每桶 ${windowBucketLabel(
            activeBucketSeconds,
          )}）；无样本的桶断线，不以 0 连接。`}
          {futureSkewedNote ? (
            <span data-testid="cluster-trend-future-note"> {futureSkewedNote}</span>
          ) : null}
          {coverageNote ? (
            <span data-testid="cluster-trend-coverage-note"> {coverageNote}</span>
          ) : null}
        </>
      }
    >
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          width={width}
          height={height}
          style={{ display: 'block', width: '100%' }}
          role="img"
          aria-label={ariaLabel}
          onMouseMove={(event) => {
            const index = indexFromSvgPointer(event);
            setHovered(index >= 0 ? index : null);
          }}
          onMouseLeave={() => {
            setHovered(null);
          }}
          onClick={(event) => {
            const index = indexFromSvgPointer(event);
            if (index >= 0) {
              const bucket = sorted[index];
              if (!bucket) {
                return;
              }
              // 触屏点按：钉住该桶 tooltip（同桶再点取消）。
              setHovered(null);
              setPinnedBucket((previous) =>
                previous === bucket.bucket_start ? null : bucket.bucket_start,
              );
              return;
            }
            // 点在绘图区外（图例/坐标轴/空白）：取消钉住。
            setHovered(null);
            setPinnedBucket(null);
          }}
        >
          <title>{ariaLabel}</title>

          {/* 图例。 */}
          {SERIES.map((series, index) => (
            <g key={series.key} transform={`translate(${PADDING_LEFT + index * 86}, 12)`}>
              <line x1={0} x2={14} y1={0} y2={0} stroke={series.color} strokeWidth={2} />
              <text x={19} y={4} fontSize={10} fill="var(--color-text-secondary)">
                {series.label}
              </text>
            </g>
          ))}

          {/* 横向网格 + y 轴百分比刻度。 */}
          {tickValues(scale).map((tick) => {
            const y = PADDING_TOP + mapY(tick, scale.max, plotHeight);
            return (
              <g key={tick}>
                <line
                  x1={PADDING_LEFT}
                  x2={PADDING_LEFT + plotWidth}
                  y1={y}
                  y2={y}
                  stroke="var(--color-border)"
                  strokeWidth={1}
                />
                <text
                  x={PADDING_LEFT - 6}
                  y={y + 3}
                  textAnchor="end"
                  fontSize={10}
                  fill="var(--color-text-tertiary)"
                >
                  {Math.round(tick)}%
                </text>
              </g>
            );
          })}

          {/* 折线：null 处断开，单点段画圆点。 */}
          {SERIES.map((series) => {
            const values = valuesBySeries[series.key];
            return (
              <g key={series.key}>
                {splitLineRuns(values).map((run, runIndex) => {
                  const coordinates = run.map((index) => {
                    const band = bands[index];
                    const value = values[index];
                    return {
                      x: PADDING_LEFT + (band ? band.center : 0),
                      y:
                        PADDING_TOP +
                        mapY(
                          value === null || value === undefined ? 0 : value,
                          scale.max,
                          plotHeight,
                        ),
                    };
                  });
                  if (coordinates.length === 1) {
                    const single = coordinates[0];
                    return single ? (
                      <circle
                        key={`${series.key}-dot-${runIndex}`}
                        cx={single.x}
                        cy={single.y}
                        r={3}
                        fill={series.color}
                      />
                    ) : null;
                  }
                  return (
                    <polyline
                      key={`${series.key}-line-${runIndex}`}
                      points={buildPolyline(coordinates)}
                      fill="none"
                      stroke={series.color}
                      strokeWidth={series.strokeWidth}
                      strokeLinejoin="round"
                      strokeLinecap="round"
                    />
                  );
                })}
              </g>
            );
          })}

          {/* x 轴标签。走查第 15 轮 low：末标签原为 middle 锚点，
              "11/21 01:00" 右缘越出 SVG 被裁成 "11/21 01:0"。改用与
              巡检均分/告警分布同口径的 axisLabelPlacement：放得下就
              跟随分带中心，放不下才贴边并改 start/end 锚点。 */}
          <g data-testid="cluster-trend-x-axis">
            {tickIndices.map((index) => {
              const point = sorted[index];
              const band = bands[index];
              if (!point || !band) {
                return null;
              }
              const label = formatBucketLabel(point.bucket_start, activeWindow);
              const edge: AxisLabelEdge =
                sorted.length === 1
                  ? 'middle'
                  : index === 0
                    ? 'first'
                    : index === sorted.length - 1
                      ? 'last'
                      : 'middle';
              const placement = axisLabelPlacement(
                PADDING_LEFT + band.center,
                estimateTextWidth(label, 10),
                PADDING_LEFT,
                Math.max(PADDING_LEFT, width - PADDING_RIGHT),
                edge,
              );
              return (
                <text
                  key={`x-${index}`}
                  x={placement.x}
                  y={PADDING_TOP + plotHeight + 15}
                  textAnchor={placement.anchor}
                  fontSize={10}
                  fill="var(--color-text-tertiary)"
                >
                  {label}
                </text>
              );
            })}
          </g>

          {/* 键盘/读屏热区：每桶保留可聚焦节点（Tab 序与 aria-label 不变）；
              pointer 事件冒泡给根 svg，由触点 x 取最近桶。 */}
          {sorted.map((point, index) => {
            const band = bands[index];
            if (!band) {
              return null;
            }
            const bucketStep = plotWidth / sorted.length;
            const p95 = valuesBySeries.cpu_p95[index] ?? null;
            const average = valuesBySeries.cpu_avg[index] ?? null;
            const max = valuesBySeries.cpu_max[index] ?? null;
            return (
              <rect
                key={`hit-${index}`}
                x={PADDING_LEFT + index * bucketStep}
                y={PADDING_TOP}
                width={Math.max(1, bucketStep)}
                height={plotHeight}
                fill="transparent"
                tabIndex={0}
                role="img"
                data-in-progress={index === inProgressIndex ? 'true' : undefined}
                aria-label={`${formatFullTime(point.bucket_start)} 桶${
                  index === inProgressIndex ? '（进行中）' : ''
                }，P95 ${percentText(p95)}，均值 ${percentText(average)}，峰值 ${percentText(
                  max,
                )}，采样台数 ${point.servers ?? '—'}，样本数 ${point.samples ?? '—'}`}
                onFocus={() => {
                  setHovered(index);
                }}
                onBlur={() => {
                  setHovered(null);
                }}
              />
            );
          })}

          {/* 绘图区几何标记（第 11 轮 medium）：仅用于 E2E 断言命中范围覆盖
              整张绘图区；pointer-events:none 不拦截上面的每桶热区。 */}
          <rect
            data-testid="cluster-trend-hit-surface"
            x={PADDING_LEFT}
            y={PADDING_TOP}
            width={plotWidth}
            height={plotHeight}
            fill="transparent"
            pointerEvents="none"
          />

          {/* hover/点按十字线 + 三个序列的圆点。 */}
          {activePoint && activeIndex !== null && bands[activeIndex] ? (
            <g pointerEvents="none">
              <line
                x1={PADDING_LEFT + (bands[activeIndex]?.center ?? 0)}
                x2={PADDING_LEFT + (bands[activeIndex]?.center ?? 0)}
                y1={PADDING_TOP}
                y2={PADDING_TOP + plotHeight}
                stroke="var(--color-border-strong)"
                strokeDasharray="3 3"
              />
              {SERIES.map((series) => {
                const value = valuesBySeries[series.key][activeIndex] ?? null;
                if (value === null) {
                  return null;
                }
                return (
                  <circle
                    key={`hover-${series.key}`}
                    cx={PADDING_LEFT + (bands[activeIndex]?.center ?? 0)}
                    cy={PADDING_TOP + mapY(value, scale.max, plotHeight)}
                    r={3}
                    fill={series.color}
                  />
                );
              })}
            </g>
          ) : null}
        </svg>

        {activePoint && activeIndex !== null ? (
          <ChartTooltip
            x={PADDING_LEFT + (bands[activeIndex]?.center ?? 0)}
            containerWidth={width}
            testId="cluster-trend-tooltip"
          >
            <div style={{ fontWeight: 600 }}>
              {formatFullTime(activePoint.bucket_start)}
              {activeIndex === inProgressIndex ? '（进行中）' : ''}
            </div>
            {SERIES.map((series) => {
              const value = valuesBySeries[series.key][activeIndex] ?? null;
              return (
                <div key={series.key}>
                  <span
                    aria-hidden="true"
                    style={{
                      display: 'inline-block',
                      width: 8,
                      height: 8,
                      borderRadius: 2,
                      background: series.color,
                      marginRight: 6,
                    }}
                  />
                  {series.label} <strong>{percentText(value)}</strong>
                </div>
              );
            })}
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              采样台数 {activePoint.servers ?? '—'} · 样本数 {activePoint.samples ?? '—'}
            </div>
          </ChartTooltip>
        ) : null}
      </div>
    </ChartFrame>
  );
}
