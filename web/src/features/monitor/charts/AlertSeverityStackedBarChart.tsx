// 告警发生分布（按小时 × 严重度，堆叠柱）。
//
// 数据：GET /api/alert-events/stats 的 severity_timeline[]（桶宽由服务端
// 写死，与集群趋势同档：24h→3600s、7d→21600s）。
// 交互：hover/聚焦显示桶内各等级数与总数；点击（或 Enter/空格）把该桶的
// fired_after / fired_before 交给页面写入事件表筛选（页面用可清除 Tag 展示，
// 本组件不持有筛选状态）。

import { useMemo, useState } from 'react';
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';

import { formatChartTime } from '../chartMath';
import ChartFrame from './ChartFrame';
import ChartTooltip from './ChartTooltip';
import {
  bandLayout,
  bucketRange,
  countScale,
  defaultBucketSeconds,
  finiteOrNull,
  labelTickIndices,
  nearestBandIndex,
  SEVERITY_LABELS,
  SEVERITY_ORDER,
  stackSeverityTimeline,
  type BucketRange,
  type ChartWindow,
  type SeverityBucket,
  type SeverityKey,
} from './chartModel';
import { activateOnKey } from './chartA11y';
import { useChartWidth } from './useChartWidth';
import { useNowMs } from './useNowMs';

export interface AlertBucketSelection extends BucketRange {
  bucketStart: string;
  bucketSeconds: number;
}

export interface AlertSeverityStackedBarChartProps {
  /** stats.severity_timeline。 */
  buckets: readonly SeverityBucket[];
  window?: ChartWindow;
  /** 服务端返回的桶宽（秒）；缺省按窗口取契约写死值。 */
  bucketSeconds?: number;
  onBucketSelect?: (selection: AlertBucketSelection) => void;
  /** 当前已生效的时段筛选桶（用于高亮），由页面传入。 */
  selectedBucketStart?: string | null;
  loading?: boolean;
  error?: ReactNode;
  emptyMessage?: ReactNode;
  /** 当前时刻（epoch ms）；生产传服务端时间，缺省走浏览器 60s 时钟。 */
  now?: number;
  height?: number;
}

const SEVERITY_COLORS: Record<SeverityKey, string> = {
  critical: 'var(--color-danger)',
  warning: 'var(--color-warning)',
  info: 'var(--color-info)',
};

/** 绘制顺序从底到顶：info → warning → critical。 */
const STACK_ORDER: readonly SeverityKey[] = ['info', 'warning', 'critical'];

const FALLBACK_WIDTH = 560;
const PADDING_LEFT = 36;
const PADDING_RIGHT = 10;
const PADDING_TOP = 26;
const PADDING_BOTTOM = 22;

const monthDayTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
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
  // 24h 窗口跨午夜时首个 23:00 属于昨天，与单机折线同口径补月/日。
  return window === '7d'
    ? monthDayTimeFormatter.format(time)
    : formatChartTime(time, 24 * 3_600_000);
}

function formatTime(value: string): string {
  const time = Date.parse(value);
  return Number.isFinite(time) ? fullTimeFormatter.format(time) : '时间未知';
}

function formatBucketWidth(seconds: number): string {
  return seconds % 3600 === 0 ? `${seconds / 3600} 小时` : `${Math.round(seconds / 60)} 分钟`;
}

export default function AlertSeverityStackedBarChart({
  buckets,
  window = '24h',
  bucketSeconds,
  onBucketSelect,
  selectedBucketStart,
  loading = false,
  error,
  emptyMessage = '该时间窗内暂无告警事件',
  now,
  height = 240,
}: AlertSeverityStackedBarChartProps) {
  const [containerRef, measuredWidth] = useChartWidth();
  const [hovered, setHovered] = useState<number | null>(null);
  const nowMs = useNowMs(now);

  const providedBucketSeconds = finiteOrNull(bucketSeconds);
  const activeBucketSeconds =
    providedBucketSeconds !== null && providedBucketSeconds > 0
      ? providedBucketSeconds
      : defaultBucketSeconds(window);
  const stacked = useMemo(() => stackSeverityTimeline(buckets), [buckets]);
  const total = useMemo(() => stacked.reduce((sum, bucket) => sum + bucket.total, 0), [stacked]);

  const width = measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH;
  const plotWidth = Math.max(0, width - PADDING_LEFT - PADDING_RIGHT);
  const plotHeight = Math.max(0, height - PADDING_TOP - PADDING_BOTTOM);
  const bands = bandLayout(stacked.length, plotWidth, 0.28);
  const maxTotal = stacked.reduce((max, bucket) => Math.max(max, bucket.total), 0);
  const scale = countScale(maxTotal);
  // 走查第 3 轮 low：7 天视图的 "MM/DD HH:mm"（11 字符）在 1440 双列卡里
  // 7 个标签会与相邻标签重叠、首尾还被绘图区边缘裁掉，7 天降到 6 个。
  // 走查第 14 轮 low：24h 标签也补了月/日（同样 11 字符），从 7 个降到
  // 5 个，避免跨午夜时首尾日期标签互相压住。
  const tickCount = window === '7d' ? (width < 520 ? 3 : 6) : width < 520 ? 4 : 5;
  const tickIndices = labelTickIndices(stacked.length, tickCount);
  // 未走完的最后一桶（end > now）标「进行中」（走查第 14 轮 low）。
  const lastBucket = stacked[stacked.length - 1];
  const lastBucketEnd = lastBucket
    ? Date.parse(lastBucket.bucket_start) + activeBucketSeconds * 1000
    : Number.NaN;
  const inProgressIndex =
    nowMs > 0 && Number.isFinite(lastBucketEnd) && lastBucketEnd > nowMs ? stacked.length - 1 : -1;

  const ariaLabel = `告警发生分布，近 ${window === '7d' ? '7 天' : '24 小时'}，每桶 ${formatBucketWidth(
    activeBucketSeconds,
  )}，共 ${stacked.length} 个桶、${total} 条告警，按严重度堆叠${
    inProgressIndex >= 0 ? '，最后一桶进行中' : ''
  }`;

  function selectBucket(index: number): void {
    const bucket = stacked[index];
    if (!bucket) {
      return;
    }
    const range = bucketRange(bucket.bucket_start, activeBucketSeconds);
    if (!range) {
      return;
    }
    onBucketSelect?.({
      ...range,
      bucketStart: bucket.bucket_start,
      bucketSeconds: activeBucketSeconds,
    });
  }

  /**
   * 触点 → 最近桶下标（第 11 轮 medium）。
   *
   * 指针事件统一由根 svg 接收（每桶矩形只负责键盘/读屏，不再各自套一层
   * pointer 逻辑），把触点换算回 SVG 的绘图区坐标后交给 nearestBandIndex。
   * 这与 MetricChart 的 handleMove/pointerToSvgX 同口径：390 宽下 24/28 个
   * 桶的命中由触点 x 决定，始终落到视觉上最近的桶。
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
    return nearestBandIndex(svgX - PADDING_LEFT, plotWidth, stacked.length);
  }

  const hoveredBucket = hovered !== null ? stacked[hovered] : undefined;
  const selectable = Boolean(onBucketSelect);

  return (
    <ChartFrame
      loading={loading}
      error={error}
      // 零告警也走 ChartFrame 空态（走查第 9 轮 low）：后端固定返回 24 个
      // 全零桶，旧条件只看 stacked.length，会把 24 个空桶都渲染成
      // tabIndex=0 的热区（键盘用户要 Tab 24 次才能到事件表）。
      empty={total === 0}
      emptyMessage={emptyMessage}
      height={height}
      subtitle={
        selectable
          ? `点击柱体把该时段写入下方事件表筛选（每桶 ${formatBucketWidth(activeBucketSeconds)}）。`
          : `按严重度堆叠；每桶 ${formatBucketWidth(activeBucketSeconds)}。`
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
            if (index < 0) {
              return;
            }
            // 点按后保留 tooltip（触屏没有 mouseleave，便于确认选中的桶）。
            setHovered(index);
            selectBucket(index);
          }}
        >
          <title>{ariaLabel}</title>

          {/* 图例（高 → 低）。 */}
          {SEVERITY_ORDER.map((severity, index) => (
            <g key={severity} transform={`translate(${PADDING_LEFT + index * 72}, 12)`}>
              <rect x={0} y={-6} width={9} height={9} rx={2} fill={SEVERITY_COLORS[severity]} />
              <text x={14} y={2} fontSize={10} fill="var(--color-text-secondary)">
                {SEVERITY_LABELS[severity]}
              </text>
            </g>
          ))}

          {/* 横向网格 + 整数计数刻度。 */}
          {scale.ticks.map((tick) => {
            const y = PADDING_TOP + plotHeight - (tick / scale.max) * plotHeight;
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
                  {tick}
                </text>
              </g>
            );
          })}

          {/* 堆叠柱。 */}
          {stacked.map((bucket, index) => {
            const band = bands[index];
            if (!band) {
              return null;
            }
            const selected = selectedBucketStart === bucket.bucket_start;
            const x = PADDING_LEFT + band.start;
            const bottom = PADDING_TOP + plotHeight;
            let cursor = 0;
            return (
              <g key={`bucket-${index}`}>
                {selected ? (
                  <rect
                    x={x - 1}
                    y={PADDING_TOP}
                    width={Math.max(1, band.size + 2)}
                    height={plotHeight}
                    fill="var(--color-bg-selected)"
                    rx={3}
                  />
                ) : null}
                {STACK_ORDER.map((severity) => {
                  const level = bucket.levels.find((entry) => entry.key === severity);
                  const value = level?.value ?? 0;
                  if (value <= 0) {
                    return null;
                  }
                  const barHeight = (value / scale.max) * plotHeight;
                  const y = bottom - cursor - barHeight;
                  cursor += barHeight;
                  return (
                    <rect
                      key={`${index}-${severity}`}
                      x={x}
                      y={y}
                      width={Math.max(1, band.size)}
                      height={Math.max(1, barHeight)}
                      rx={1}
                      fill={SEVERITY_COLORS[severity]}
                    />
                  );
                })}
                {selected ? (
                  <rect
                    x={x - 1}
                    y={PADDING_TOP}
                    width={Math.max(1, band.size + 2)}
                    height={plotHeight}
                    fill="none"
                    stroke="var(--color-primary)"
                    strokeWidth={1.5}
                    rx={3}
                  />
                ) : null}
              </g>
            );
          })}

          {/* x 轴标签。首尾贴绘图区边缘（start/end 锚点），避免最后一个
              "11/19 20:00" 被右缘裁成 "11/19 20:"。 */}
          <g data-testid="alert-severity-x-axis">
            {tickIndices.map((index) => {
              const bucket = stacked[index];
              const band = bands[index];
              if (!bucket || !band) {
                return null;
              }
              const isFirst = index === 0;
              const isLast = index === stacked.length - 1;
              return (
                <text
                  key={`x-${index}`}
                  x={
                    isFirst
                      ? PADDING_LEFT
                      : isLast
                        ? Math.max(PADDING_LEFT, width - PADDING_RIGHT)
                        : PADDING_LEFT + band.center
                  }
                  y={PADDING_TOP + plotHeight + 15}
                  textAnchor={isFirst ? 'start' : isLast ? 'end' : 'middle'}
                  fontSize={10}
                  fill="var(--color-text-tertiary)"
                >
                  {formatBucketLabel(bucket.bucket_start, window)}
                </text>
              );
            })}
          </g>

          {/* 键盘/读屏热区：每桶保留可聚焦节点（Tab 序与 aria-label 不变）；
              pointer 事件冒泡给根 svg，由触点 x 取最近桶，而不是由这块
              约 11px 宽的矩形决定下钻时段。 */}
          {stacked.map((bucket, index) => {
            const band = bands[index];
            if (!band) {
              return null;
            }
            const inProgress = index === inProgressIndex;
            const ariaBucketLabel = `${formatTime(bucket.bucket_start)} 桶${
              inProgress ? '（进行中）' : ''
            }，严重 ${bucket.levels[0]?.value ?? 0}，警告 ${
              bucket.levels[1]?.value ?? 0
            }，信息 ${bucket.levels[2]?.value ?? 0}，合计 ${bucket.total}`;
            return (
              <rect
                key={`hit-${index}`}
                x={PADDING_LEFT + index * (plotWidth / stacked.length)}
                y={PADDING_TOP}
                width={Math.max(1, plotWidth / stacked.length)}
                height={plotHeight}
                fill="transparent"
                tabIndex={0}
                data-in-progress={inProgress ? 'true' : undefined}
                role={selectable ? 'button' : 'img'}
                aria-label={
                  selectable ? `${ariaBucketLabel}；按 Enter 筛选该时段` : ariaBucketLabel
                }
                onFocus={() => {
                  setHovered(index);
                }}
                onBlur={() => {
                  setHovered(null);
                }}
                onKeyDown={(event) => {
                  activateOnKey(event, () => {
                    selectBucket(index);
                  });
                }}
                style={{ cursor: selectable ? 'pointer' : 'default' }}
              />
            );
          })}

          {/* 绘图区几何标记（第 11 轮 medium）：仅用于 E2E 断言命中范围覆盖
              整张绘图区；pointer-events:none 不拦截上面的每桶热区。 */}
          <rect
            data-testid="alert-severity-hit-surface"
            x={PADDING_LEFT}
            y={PADDING_TOP}
            width={plotWidth}
            height={plotHeight}
            fill="transparent"
            pointerEvents="none"
          />
        </svg>

        {hoveredBucket ? (
          <ChartTooltip
            x={PADDING_LEFT + (hovered !== null ? (bands[hovered]?.center ?? 0) : 0)}
            containerWidth={width}
            testId="alert-distribution-tooltip"
          >
            <div style={{ fontWeight: 600 }}>
              {formatTime(hoveredBucket.bucket_start)}
              {hovered === inProgressIndex ? '（进行中）' : ''}
            </div>
            {SEVERITY_ORDER.map((severity) => {
              const count =
                hoveredBucket.levels.find((entry) => entry.key === severity)?.value ?? 0;
              return (
                <div key={severity}>
                  <span
                    aria-hidden="true"
                    style={{
                      display: 'inline-block',
                      width: 8,
                      height: 8,
                      borderRadius: 2,
                      background: SEVERITY_COLORS[severity],
                      marginRight: 6,
                    }}
                  />
                  {SEVERITY_LABELS[severity]} <strong>{count}</strong>
                </div>
              );
            })}
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              合计 {hoveredBucket.total} 条{selectable ? ' · 点击筛选该时段' : ''}
            </div>
          </ChartTooltip>
        ) : null}
      </div>
    </ChartFrame>
  );
}
