// 最紧张服务器 Top 12（跨机排行）。
//
// 数据：GET /api/monitor/overview 的 servers[]（MetricLatestRow，每台一行）。
// 交互：CPU / 内存 / 磁盘 三选一切换排序指标；hover 或键盘聚焦某行显示
// 精确值、采样时刻与过期标记。三指标同为百分比，共用 0..100 轴。

import { Segmented } from 'antd';
import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import ChartFrame from './ChartFrame';
import ChartTooltip from './ChartTooltip';
import {
  formatSampleAge,
  formatSampleTime,
  isFutureSample,
  isStale,
  latestTimestamp,
  percentText,
  rankFraction,
  sortTopServers,
  staleAgeMs,
  truncateMiddleToWidth,
  type RankMetric,
  type ServerRankRow,
} from './chartModel';
import { useChartWidth } from './useChartWidth';

export interface TopServerBarChartProps {
  /** overview.servers（每台一行最新采样）。 */
  servers: readonly ServerRankRow[];
  /** 受控排序指标；不传则组件内部维护。 */
  metric?: RankMetric;
  defaultMetric?: RankMetric;
  onMetricChange?: (metric: RankMetric) => void;
  /** 排行条数，默认 12（上限 12）。 */
  limit?: number;
  loading?: boolean;
  error?: ReactNode;
  emptyMessage?: ReactNode;
  /** 判定「过期」的采样年龄，默认 5 分钟。 */
  staleAfterMs?: number;
  /** 测试/快照可注入固定时钟；生产传服务端时间（overview.server_time 偏移）。 */
  now?: number;
  height?: number;
}

const METRIC_OPTIONS: ReadonlyArray<{ value: RankMetric; label: string }> = [
  { value: 'cpu_pct', label: 'CPU' },
  { value: 'mem_pct', label: '内存' },
  { value: 'disk_pct', label: '磁盘' },
];

const METRIC_LABELS: Record<RankMetric, string> = {
  cpu_pct: 'CPU 使用率',
  mem_pct: '内存使用率',
  disk_pct: '磁盘使用率',
};

const FALLBACK_WIDTH = 420;
const PADDING_TOP = 10;
const PADDING_BOTTOM = 22;
const PERCENT_TICKS = [0, 25, 50, 75, 100];

export default function TopServerBarChart({
  servers,
  metric,
  defaultMetric = 'cpu_pct',
  onMetricChange,
  limit = 12,
  loading = false,
  error,
  emptyMessage = '暂无已采集的服务器',
  staleAfterMs = 300_000,
  now,
  height = 340,
}: TopServerBarChartProps) {
  const [containerRef, measuredWidth] = useChartWidth();
  const [innerMetric, setInnerMetric] = useState<RankMetric>(defaultMetric);
  const [hovered, setHovered] = useState<number | null>(null);
  // 触屏没有 hover：点按某行把 tooltip 钉住（再点一次取消）。用 server_id
  // 而不是行号，指标切换/刷新重排后仍指向同一台机器。
  const [pinnedId, setPinnedId] = useState<string | null>(null);
  // 时钟走定时器回调（渲染期不调 Date.now，保持组件纯净）：首帧按「不过期」
  // 处理，定时器立刻校正，之后每 60s 刷新一次过期标记。
  const [clock, setClock] = useState(0);
  useEffect(() => {
    if (now !== undefined) {
      return;
    }
    const tick = () => {
      setClock(Date.now());
    };
    const initial = setTimeout(tick, 0);
    const interval = setInterval(tick, 60_000);
    return () => {
      clearTimeout(initial);
      clearInterval(interval);
    };
  }, [now]);

  const activeMetric = metric ?? innerMetric;
  const cappedLimit = Math.max(1, Math.min(12, Math.floor(limit)));
  const nowMs = now ?? clock;
  // 时钟尚未初始化（首帧 0 / SSR）时不排除任何行，避免把全部服务器误判
  // 成「未来采样」；定时器立刻校正，生产传入服务端时间。
  // 超前台数使用 1 秒容差（isFutureSample 默认值），不再随采集间隔放大；
  // 与趋势 SQL 的 created_at <= now 钳制同口径（走查第 17 轮 medium）。
  const rankable = useMemo(() => {
    if (nowMs <= 0) {
      return { rows: servers, ahead: 0 };
    }
    const rows: ServerRankRow[] = [];
    let ahead = 0;
    for (const row of servers) {
      if (isFutureSample(row.created_at, nowMs)) {
        ahead++;
      } else {
        rows.push(row);
      }
    }
    return { rows, ahead };
  }, [servers, nowMs]);
  const ranked = useMemo(
    () => sortTopServers(rankable.rows, activeMetric, cappedLimit),
    [rankable.rows, activeMetric, cappedLimit],
  );

  const width = measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH;
  // 走查第 12 轮 low：390 宽下 padLeft=76 只剩 64px 标签预算，11 字符的
  // `web-prod-01` 实测宽 66.05px，左缘落到 1.95px，与行首过期圆点
  // （cx=6/r=3，覆盖 3–9px）重叠，截图里读成 `eb-prod-01`。窄屏左列加宽
  // 到 88px（绘图区仍有 256px），并给过期行再留 12px 圆点/间隙预算。
  const padLeft = width < 520 ? 88 : 104;
  const padRight = width < 520 ? 46 : 58;
  const plotWidth = Math.max(0, width - padLeft - padRight);
  const plotHeight = Math.max(0, height - PADDING_TOP - PADDING_BOTTOM);
  const rowHeight = ranked.length > 0 ? Math.min(24, Math.max(16, plotHeight / ranked.length)) : 24;
  const groupTop = PADDING_TOP + Math.max(0, (plotHeight - rowHeight * ranked.length) / 2);

  const latest = latestTimestamp(rankable.rows);
  const aheadNote =
    rankable.ahead > 0 ? `${rankable.ahead} 台采样时间超前服务端时钟，未参与排名` : null;
  const ariaLabel = `最紧张服务器 Top ${ranked.length}，按${METRIC_LABELS[activeMetric]}降序，0 到 100% 共用百分比轴，共 ${rankable.rows.length} 台已采集服务器${
    latest === null ? '' : `，最晚采样 ${formatSampleTime(new Date(latest).toISOString())}`
  }${aheadNote ? `；${aheadNote}` : ''}`;
  const resolvedEmptyMessage =
    rankable.ahead > 0 && ranked.length === 0
      ? `暂无可用采样：${rankable.ahead} 台采样时间超前服务端时钟，请检查时钟`
      : emptyMessage;

  function changeMetric(next: RankMetric): void {
    if (metric === undefined) {
      setInnerMetric(next);
    }
    onMetricChange?.(next);
  }

  const metricSwitch = (
    <div role="group" aria-label="选择排行排序指标">
      <Segmented
        size="small"
        value={activeMetric}
        options={METRIC_OPTIONS.map((option) => ({ label: option.label, value: option.value }))}
        onChange={(value) => {
          changeMetric(value as RankMetric);
        }}
      />
    </div>
  );

  const pinnedIndex =
    pinnedId === null ? -1 : ranked.findIndex((row) => row.server_id === pinnedId);
  // hover 优先（鼠标/键盘），离开后回落到点按钉住的行。
  const activeIndex = hovered ?? (pinnedIndex >= 0 ? pinnedIndex : null);
  const activeRow = activeIndex !== null ? ranked[activeIndex] : undefined;
  const activeValue = activeRow ? activeRow[activeMetric] : null;
  const activeAge = activeRow ? staleAgeMs(activeRow.created_at, nowMs) : null;

  return (
    <ChartFrame
      loading={loading}
      error={error}
      empty={ranked.length === 0}
      emptyMessage={resolvedEmptyMessage}
      height={height}
      actions={metricSwitch}
      subtitle={`三指标同为百分比，共用 0–100% 轴；采样超 ${Math.max(
        1,
        Math.round(staleAfterMs / 60_000),
      )} 分钟标为过期${aheadNote ? `；${aheadNote}` : ''}。`}
    >
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          width={width}
          height={height}
          style={{ display: 'block', width: '100%' }}
          role="img"
          aria-label={ariaLabel}
        >
          <title>{ariaLabel}</title>

          {/* 0..100 竖向网格 + 刻度（三指标共用一把轴）。 */}
          {PERCENT_TICKS.map((tick) => {
            const x = padLeft + (tick / 100) * plotWidth;
            return (
              <g key={tick}>
                <line
                  x1={x}
                  x2={x}
                  y1={PADDING_TOP}
                  y2={PADDING_TOP + plotHeight}
                  stroke="var(--color-border)"
                  strokeWidth={1}
                />
                <text
                  x={x}
                  y={PADDING_TOP + plotHeight + 15}
                  textAnchor="middle"
                  fontSize={10}
                  fill="var(--color-text-tertiary)"
                >
                  {tick}%
                </text>
              </g>
            );
          })}

          {ranked.map((row, index) => {
            const rowTop = groupTop + index * rowHeight;
            const centerY = rowTop + rowHeight / 2;
            const value = row[activeMetric];
            const barWidth = rankFraction(value) * plotWidth;
            const stale = isStale(row.created_at, nowMs, staleAfterMs);
            const rowAge = staleAgeMs(row.created_at, nowMs);
            // 走查第 6 轮 medium：按码点截断会让中文名（11 字 ≈121px）
            // 超出左列（1440 可用 92px / 390 非过期 76px、过期 64px），
            // 文字左缘被裁成负坐标。改为按像素宽度中段截断（CJK 双宽），
            // 左缘留 4px。走查第 12 轮 low：过期行的行首圆点占 3–9px，
            // 再让出 12px，且 ASCII 宽度按 6px/字符保守估算，保证标签
            // 左缘 ≥ 12px。
            const labelBudget = padLeft - 12 - (stale ? 12 : 0);
            const label = truncateMiddleToWidth(row.server_name || row.server_id, labelBudget, 11);
            const valueText = percentText(value);
            // 走查第 8 轮 medium：过期样式此前只落在柱体上，0.0% 的柱宽为
            // 0，过期与刚采到的 0.0% 机器看起来完全一样。行首加固定大小
            // 的警告圆点，并给标签/数值文字上 warning 色——不依赖柱宽。
            const textFill = stale ? 'var(--color-warning)' : 'var(--color-text-secondary)';
            return (
              <g key={row.server_id}>
                {stale ? (
                  <g data-testid="top-server-stale-marker" aria-hidden="true">
                    <circle cx={6} cy={centerY} r={3} fill="var(--color-warning)" />
                    <title>
                      {`采样已过期${rowAge === null ? '' : ` ${formatSampleAge(rowAge)}`}`}
                    </title>
                  </g>
                ) : null}
                <text
                  x={padLeft - 8}
                  y={centerY + 4}
                  textAnchor="end"
                  fontSize={11}
                  fill={textFill}
                >
                  {label}
                </text>
                <rect
                  x={padLeft}
                  y={centerY - Math.max(4, rowHeight * 0.28)}
                  width={Math.max(0, barWidth)}
                  height={Math.max(7, rowHeight * 0.56)}
                  rx={3}
                  fill="var(--color-primary)"
                  opacity={stale ? 0.55 : 1}
                  stroke={stale ? 'var(--color-warning)' : 'none'}
                  strokeDasharray={stale ? '4 3' : undefined}
                />
                <text
                  x={Math.min(padLeft + barWidth + 5, width - 42)}
                  y={centerY + 4}
                  fontSize={10}
                  fill={textFill}
                >
                  {valueText}
                </text>
                {/* hover/focus 热区：覆盖整行，键盘可聚焦。 */}
                <rect
                  x={0}
                  y={rowTop}
                  width={width}
                  height={rowHeight}
                  fill="transparent"
                  tabIndex={0}
                  role="img"
                  aria-label={`第 ${index + 1} 名 ${row.server_name || row.server_id}，${
                    METRIC_LABELS[activeMetric]
                  } ${valueText}，采样于 ${formatSampleTime(row.created_at)}${
                    stale && rowAge !== null ? `，已过期 ${formatSampleAge(rowAge)}` : ''
                  }`}
                  onClick={() => {
                    // 触屏点按：钉住该行 tooltip 读取完整机器名/采样时间；
                    // 再次点按同一行取消（先清 hover，避免 focus 残留）。
                    setHovered(null);
                    setPinnedId((previous) => (previous === row.server_id ? null : row.server_id));
                  }}
                  onMouseEnter={() => {
                    setHovered(index);
                  }}
                  onMouseLeave={() => {
                    setHovered(null);
                  }}
                  onFocus={() => {
                    setHovered(index);
                  }}
                  onBlur={() => {
                    setHovered(null);
                  }}
                  style={{ cursor: 'default' }}
                />
              </g>
            );
          })}
        </svg>

        {activeRow ? (
          <ChartTooltip
            x={padLeft + rankFraction(activeValue) * plotWidth}
            containerWidth={width}
            top={Math.max(4, groupTop + (activeIndex ?? 0) * rowHeight)}
            testId="top-server-tooltip"
          >
            <div style={{ fontWeight: 600 }}>{activeRow.server_name || activeRow.server_id}</div>
            <div>
              <span
                aria-hidden="true"
                style={{
                  display: 'inline-block',
                  width: 8,
                  height: 8,
                  borderRadius: 2,
                  background: 'var(--color-primary)',
                  marginRight: 6,
                }}
              />
              {METRIC_LABELS[activeMetric]} <strong>{percentText(activeValue)}</strong>
            </div>
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              采样 {formatSampleTime(activeRow.created_at)}
            </div>
            {activeAge !== null && isStale(activeRow.created_at, nowMs, staleAfterMs) ? (
              <div style={{ color: 'var(--color-warning)' }}>
                已过期 {formatSampleAge(activeAge)}（阈值 {Math.round(staleAfterMs / 60_000)} 分钟）
              </div>
            ) : null}
          </ChartTooltip>
        ) : null}
      </div>
    </ChartFrame>
  );
}
