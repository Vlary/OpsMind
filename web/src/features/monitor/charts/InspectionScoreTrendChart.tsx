// 巡检均分走势（按任务 / 全部任务）。
//
// 数据：GET /api/inspection/trend 的 points[]（按报告份数，最多 60 份）。
// 交互：任务下拉切换（含「全部任务」）；hover/聚焦单点显示均分 /
// 覆盖台数 / 异常项 / 触发方式。默认选中「最近一份报告所属任务」
// （defaultJobId）；没有定时任务或最近一份是即时巡检时选中「全部任务」，
// 保证即时巡检报告也能出现在走势图上。

import { Select } from 'antd';
import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import { TRIGGER_LABELS, scoreColorVar } from '../../inspections/inspectionView';
import { buildPolyline, mapY } from '../chartMath';
import ChartFrame from './ChartFrame';
import ChartTooltip from './ChartTooltip';
import {
  axisLabelPlacement,
  bandLayout,
  clampScore,
  estimateTextWidth,
  finiteOrNull,
  hasDuplicateLabels,
  labelTickIndices,
  pickDefaultJobId,
  sortInspectionTrend,
  truncateText,
  type AxisLabelEdge,
  type InspectionJobOption,
  type InspectionTrendPoint,
} from './chartModel';
import { useChartWidth } from './useChartWidth';

export interface InspectionScoreTrendChartProps {
  /** inspection/trend 的 points。 */
  points: readonly InspectionTrendPoint[];
  /** GET /api/inspection/jobs 的任务列表（下拉）。 */
  jobs?: readonly InspectionJobOption[];
  /** 受控任务 id；不传则组件内部维护。 */
  jobId?: string | null;
  /** 默认任务（通常取最近一份报告所属任务）。 */
  defaultJobId?: string | null;
  /** 切换任务；null 表示「全部任务」（job_id 缺省，含即时巡检）。 */
  onJobChange?: (jobId: string | null) => void;
  /** 服务端 truncated=true：还有更早报告未显示。 */
  truncated?: boolean;
  loading?: boolean;
  error?: ReactNode;
  emptyMessage?: ReactNode;
  height?: number;
}

const FALLBACK_WIDTH = 560;
const PADDING_LEFT = 38;
const PADDING_RIGHT = 12;
const PADDING_TOP = 26;
const PADDING_BOTTOM = 22;
const SCORE_TICKS = [0, 25, 50, 75, 100];

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

const monthDayTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

function formatDay(value: string): string {
  const time = Date.parse(value);
  return Number.isFinite(time) ? monthDayFormatter.format(time) : '—';
}

/**
 * x 轴报告标签：跨度 >24h 时带时:分。走查第 3 轮 low：14 份 2 小时间隔的
 * 报告只显示 MM/DD，11/18 与 11/19 各出现两次，相邻报告先后无法区分。
 */
function formatDayTime(value: string): string {
  const time = Date.parse(value);
  return Number.isFinite(time) ? monthDayTimeFormatter.format(time) : '—';
}

function formatTime(value: string): string {
  const time = Date.parse(value);
  return Number.isFinite(time) ? fullTimeFormatter.format(time) : '时间未知';
}

function triggerLabel(value: string): string {
  return TRIGGER_LABELS[value] ?? value;
}

const SCORE_BANDS: ReadonlyArray<{ label: string; color: string }> = [
  { label: '≥85 良好', color: 'var(--color-success)' },
  { label: '60–84 关注', color: 'var(--color-warning)' },
  { label: '<60 异常', color: 'var(--color-danger)' },
];

export default function InspectionScoreTrendChart({
  points,
  jobs = [],
  jobId,
  defaultJobId,
  onJobChange,
  truncated = false,
  loading = false,
  error,
  emptyMessage = '暂无巡检报告',
  height = 250,
}: InspectionScoreTrendChartProps) {
  const [containerRef, measuredWidth] = useChartWidth();
  // undefined = 尚未手动选择；null = 明确选择「全部任务」。
  const [innerJobId, setInnerJobId] = useState<string | null | undefined>(undefined);
  const [hovered, setHovered] = useState<number | null>(null);
  // 触屏没有 hover：点按某份报告钉住 tooltip（同一点再点取消，点绘图区
  // 外取消）。用 report_id 做身份，任务切换/刷新重排后仍指向同一份报告。
  const [pinnedReport, setPinnedReport] = useState<string | null>(null);

  const fallbackJobId = pickDefaultJobId(jobs, defaultJobId);
  // 受控时完全信任页面传入的 id（数据查询也由页面按同一 id 发起，避免
  // 「下拉显示 A、曲线是 B」）；非受控时若已选任务已从列表消失则回落到
  // 默认任务。
  const selectedJobId =
    jobId !== undefined
      ? jobId
      : innerJobId === null ||
          (innerJobId !== undefined && jobs.some((job) => job.id === innerJobId))
        ? innerJobId
        : fallbackJobId;
  const sorted = useMemo(() => sortInspectionTrend(points), [points]);

  const width = measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH;
  const plotWidth = Math.max(0, width - PADDING_LEFT - PADDING_RIGHT);
  const plotHeight = Math.max(0, height - PADDING_TOP - PADDING_BOTTOM);
  const bands = bandLayout(sorted.length, plotWidth, 0);

  const coordinates = sorted.map((point, index) => {
    const band = bands[index];
    return {
      x: PADDING_LEFT + (band ? band.center : 0),
      y: PADDING_TOP + mapY(clampScore(point.avg_score), 100, plotHeight),
    };
  });

  // 报告跨度 >24h 或格式化后的日标签重复时轴标签补 HH:mm。走查第 9 轮
  // low：同一天两份报告（11/19 08:51 与 14:51，跨度 6h/20h）只写 MM/DD
  // 时两个点无法区分；不能只按 >24h 阈值判断。
  const firstTime = sorted.length > 0 ? Date.parse(sorted[0]?.created_at ?? '') : Number.NaN;
  const lastTime =
    sorted.length > 0 ? Date.parse(sorted[sorted.length - 1]?.created_at ?? '') : Number.NaN;
  const spanMs = Number.isFinite(firstTime) && Number.isFinite(lastTime) ? lastTime - firstTime : 0;
  const dayLabels = useMemo(() => sorted.map((point) => formatDay(point.created_at)), [sorted]);
  const usesTimeLabels = spanMs > 24 * 3_600_000 || hasDuplicateLabels(dayLabels);
  const tickIndices = labelTickIndices(
    sorted.length,
    usesTimeLabels ? (width < 520 ? 3 : 6) : width < 520 ? 4 : 7,
  );
  const selectedJobName =
    jobs.find((job) => job.id === selectedJobId)?.name ?? selectedJobId ?? '全部任务';
  const pinnedIndex =
    pinnedReport === null ? -1 : sorted.findIndex((point) => point.report_id === pinnedReport);
  // hover 优先（鼠标/键盘），离开后回落到点按钉住的报告。
  const activeIndex = hovered ?? (pinnedIndex >= 0 ? pinnedIndex : null);
  const activePoint = activeIndex !== null ? sorted[activeIndex] : undefined;

  const ariaLabel = `巡检均分走势，任务 ${selectedJobName}，最近 ${sorted.length} 份报告，纵轴 0 到 100 分`;

  function changeJob(next: string | null): void {
    if (jobId === undefined) {
      setInnerJobId(next);
    }
    onJobChange?.(next);
  }

  const ALL_JOBS_VALUE = '__all__';
  const jobSelect =
    jobs.length > 0 ? (
      <Select
        size="small"
        style={{ minWidth: 180, maxWidth: 260 }}
        aria-label="选择巡检任务"
        placeholder="选择巡检任务"
        // 「全部任务」不可缺：即时巡检（job_id 为空）不属于任何任务，
        // 没有该选项时用户无法在图上看到它们（走查第 6 轮 high）。
        value={selectedJobId ?? ALL_JOBS_VALUE}
        options={[
          { value: ALL_JOBS_VALUE, label: '全部任务' },
          ...jobs.map((job) => ({ value: job.id, label: job.name })),
        ]}
        onChange={(value) => {
          if (typeof value === 'string') {
            changeJob(value === ALL_JOBS_VALUE ? null : value);
          }
        }}
      />
    ) : null;

  return (
    <ChartFrame
      loading={loading}
      error={error}
      empty={sorted.length === 0}
      emptyMessage={selectedJobId === null ? emptyMessage : '该任务暂无报告'}
      height={height}
      actions={jobSelect}
      subtitle="每点一份巡检报告；点色为评分档位（≥85 绿 / 60–84 黄 / <60 红）。"
    >
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          width={width}
          height={height}
          style={{ display: 'block', width: '100%' }}
          role="img"
          aria-label={ariaLabel}
          onClick={() => {
            // 点绘图区外/图例空白：取消钉住。
            setHovered(null);
            setPinnedReport(null);
          }}
        >
          <title>{ariaLabel}</title>

          {SCORE_BANDS.map((band, index) => (
            <g key={band.label} transform={`translate(${PADDING_LEFT + index * 92}, 12)`}>
              <circle cx={4} cy={-2} r={3.5} fill={band.color} />
              <text x={12} y={2} fontSize={10} fill="var(--color-text-secondary)">
                {band.label}
              </text>
            </g>
          ))}

          {SCORE_TICKS.map((tick) => {
            const y = PADDING_TOP + mapY(tick, 100, plotHeight);
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

          {coordinates.length >= 2 ? (
            <polyline
              points={buildPolyline(coordinates)}
              fill="none"
              stroke="var(--color-primary)"
              strokeWidth={1.8}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
          ) : null}

          {coordinates.map((coordinate, index) => {
            const point = sorted[index];
            if (!point) {
              return null;
            }
            return (
              <circle
                key={point.report_id}
                cx={coordinate.x}
                cy={coordinate.y}
                r={activeIndex === index ? 4.5 : 3.5}
                fill={scoreColorVar(point.avg_score)}
                stroke="var(--color-bg-container)"
                strokeWidth={1}
              />
            );
          })}

          {/* x 轴标签。走查第 4 轮 low：末标签原为 middle 锚点，
              "11/20 02:13" 右缘越出 SVG 被裁成 "11/20 02:1"。走查第 9 轮
              low：此前 ≤4 份按分带中心、≥5 份首/末强制贴边，5 份时首点
              cx=147.6 而标签 x=38，差半个分带；现在按标签实际宽度决定
              居中还是贴边（axisLabelPlacement）。 */}
          <g data-testid="inspection-trend-x-axis">
            {tickIndices.map((index) => {
              const point = sorted[index];
              const band = bands[index];
              if (!point || !band) {
                return null;
              }
              const label = usesTimeLabels
                ? formatDayTime(point.created_at)
                : formatDay(point.created_at);
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
                  key={`x-${point.report_id}`}
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

          {/* 每个报告点一个可聚焦热区（等宽分带，窄屏也不重叠）。 */}
          {sorted.map((point, index) => {
            const band = bands[index];
            if (!band) {
              return null;
            }
            const score = finiteOrNull(point.avg_score);
            return (
              <rect
                key={`hit-${point.report_id}`}
                x={PADDING_LEFT + band.start}
                y={PADDING_TOP}
                width={Math.max(1, band.size)}
                height={plotHeight}
                fill="transparent"
                tabIndex={0}
                role="img"
                aria-label={`${formatTime(point.created_at)} 报告，均分 ${
                  score === null ? '—' : score
                } 分，覆盖 ${point.server_count} 台，异常项 ${point.anomaly_count}，触发方式 ${triggerLabel(
                  point.trigger,
                )}`}
                onMouseEnter={() => {
                  setHovered(index);
                }}
                onMouseLeave={() => {
                  setHovered(null);
                }}
                onClick={(event) => {
                  // 触屏点按：钉住该报告 tooltip（同一点再点取消）。
                  event.stopPropagation();
                  setHovered(null);
                  setPinnedReport((previous) =>
                    previous === point.report_id ? null : point.report_id,
                  );
                }}
                onFocus={() => {
                  setHovered(index);
                }}
                onBlur={() => {
                  setHovered(null);
                }}
              />
            );
          })}
        </svg>

        {activePoint && activeIndex !== null ? (
          <ChartTooltip
            x={PADDING_LEFT + (bands[activeIndex]?.center ?? 0)}
            containerWidth={width}
            testId="inspection-trend-tooltip"
          >
            <div style={{ fontWeight: 600 }}>{formatTime(activePoint.created_at)}</div>
            <div>
              均分{' '}
              <strong style={{ color: scoreColorVar(activePoint.avg_score) }}>
                {finiteOrNull(activePoint.avg_score) ?? '—'}
              </strong>{' '}
              分
            </div>
            <div>
              覆盖 {activePoint.server_count} 台 · 异常项 {activePoint.anomaly_count}
            </div>
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              触发方式 {triggerLabel(activePoint.trigger)} · 报告{' '}
              {truncateText(activePoint.report_id, 8)}
            </div>
          </ChartTooltip>
        ) : null}
      </div>
      {truncated ? (
        <div style={{ fontSize: 11, color: 'var(--color-text-tertiary)' }}>
          仅显示最近 {sorted.length} 份报告（更早的报告未纳入走势）
        </div>
      ) : null}
    </ChartFrame>
  );
}
