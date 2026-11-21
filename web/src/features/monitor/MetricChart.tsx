// M4 五期：原生 SVG 指标折线图（CONTRACT.md 8.0——不引图表库；8.7 监控页
// ——网格、y 轴自动量纲、hover 竖线 tooltip、点数 >500 抽样绘制）。
//
// 尺寸自适应走 ResizeObserver（卸载即 disconnect，满足 8.7“页面卸载全量
// 清理”）；hover 状态为组件本地 state，不进全局 store。
//
// 走查第 3 轮：① y 轴左留白按刻度文字宽度动态计算（20 GB/s 不再被裁）；
// ② svg 可聚焦并带 aria-label/title，方向键逐点读数（读屏可听）；③ hover
// 只记录采样时间，绘制时按当前 points 重新解析——切换 1h/6h/24h 后旧采样
// 不在新窗口里就自然消失，不再出现「幽灵点」。

import { Skeleton } from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';

import { CHART_MAX_POINTS, downsample, needsDownsample, type SeriesPoint } from './downsample';
import {
  axisPaddingLeft,
  buildPolyline,
  formatAxisValue,
  formatChartTime,
  formatCoverageDuration,
  formatCoverageDurationPrecise,
  mapX,
  mapY,
  nearestIndexByX,
  percentYScale,
  tickValues,
  windowTickTimes,
  yScale,
} from './chartMath';
import { labelTickIndices } from './charts/chartModel';
import {
  coveredSpanMs,
  medianSampleIntervalMs,
  segmentSeries,
  splitRunsAtGaps,
} from './metricSeries';

const CHART_HEIGHT = 160;
const PADDING_RIGHT = 8;
const PADDING_TOP = 8;
const PADDING_BOTTOM = 20;
/** 缺口警示带下方文字放得下的最小像素宽度（更窄只画带）。 */
const GAP_LABEL_MIN_WIDTH = 58;
/** SVG 内最多标注的缺口数；其余靠警示带与下方断档说明。 */
const MAX_GAP_LABELS = 3;
/** 图表下方断档说明最多列出的缺口明细数。 */
const MAX_GAP_NOTES = 2;

/** 读屏逐点读数用的视觉隐藏区（不占布局、不进 Tab 序）。 */
const VISUALLY_HIDDEN = {
  position: 'absolute',
  width: 1,
  height: 1,
  padding: 0,
  margin: -1,
  overflow: 'hidden',
  clip: 'rect(0 0 0 0)',
  whiteSpace: 'nowrap',
  border: 0,
} as const;

export interface MetricChartProps {
  /** 折线数据（时间升序）。 */
  points: SeriesPoint[];
  /** 指标名（进入 aria-label/title，例如「CPU 使用率」）。 */
  label?: string;
  /** 数值格式化（缺省两位小数百分比；bps 用 formatBps）。 */
  formatValue?: (value: number) => string;
  /**
   * y 轴刻度格式化（可选）。第二个参数是本轴上限，用于「按轴统一记法」
   * ——负载 0..2e6 不再混排 500000 与 1.00e6（走查第 9 轮 low）。
   * 不传时沿用 formatValue + formatAxisValue 的旧路径。
   */
  formatAxisTick?: (value: number, axisMax: number) => string;
  /** 折线颜色（语义令牌 `var(--color-chart-N)`，缺省 `--color-chart-1`）。 */
  color?: string;
  /**
   * 百分比指标（CPU/内存/磁盘）：y 轴以 100% 为硬上限，避免 99% 满载被
   * 自动量纲扩到 200% 压成半高。负载/网络等无上界指标保持自动量纲。
   */
  percentAxis?: boolean;
  /**
   * 请求进行中：渲染同高度的加载占位，不进入「暂无采集数据」空态。
   * 弱网下切换服务器/时间窗时，新 query key 尚无数据（isLoading=true），
   * 若直接渲染空态会被误读成该窗口没有历史数据。
   */
  loading?: boolean;
  /**
   * 选中时间窗起点/终点（epoch ms）。监控页按 minutes 计算
   * now-minutes..now 传入；此时横轴按窗口边界铺满，而不是取首/末采样点。
   *
   * 走查第 7 轮 medium：新接入/采集稀疏的机器选「24 小时」时，仅有的
   * 2 小时数据会把整宽拉满（轴 04:44–06:43），用户误当 24 小时读。
   * 缺省（不传）时仍按首/末采样点铺满，兼容独立使用/旧调用。
   */
  windowStart?: number;
  windowEnd?: number;
  /**
   * 服务端 truncated=true：窗口内更早的点因点数上限未返回。覆盖提示必须
   * 写成「仅返回最近 X 小时（更早的点未返回）」，不能把未返回的历史标成
   * 「无采样」（第 10 轮 high）。
   */
  truncated?: boolean;
  /**
   * 采集器配置的采样间隔（ms）。断档判定优先用观测到的主众数采样间隔；
   * 只有点数太少算不出观测间隔时才用本值兜底（见 metricSeries.ts）。
   * 覆盖提示的容差也用它：允许首尾各少一个采集间隔。
   */
  sampleIntervalMs?: number | null;
  /**
   * 该服务器是否存在任何采样（overview.latest 行）。窗口内 0 点但存在
   * latest 时，空态要写「窗口内无采样（最近一次采样 N 前）」，与「从未
   * 采集」区分（走查第 14 轮 low）。
   */
  hasAnySample?: boolean;
  /**
   * 最近一次采样的年龄文案（如「72 小时前」「时间超前服务端时钟 20 分钟」），
   * 由页面按服务端时间计算。仅用于窗口内无采样时的空态。
   */
  latestSampleSummary?: string | null;
}

interface ChartSummary {
  first: SeriesPoint;
  last: SeriesPoint;
  min: number;
  max: number;
  spanMs: number;
}

/** 缺口在绘图区内的像素位置（与窗口相交部分）。 */
interface GapView {
  fromTime: number;
  x1: number;
  x2: number;
  widthPx: number;
  label: string;
  showLabel: boolean;
}

/** 首帧 ResizeObserver 尚未回调时的布局宽度（与 svg 缺省 width 一致）。 */
const FALLBACK_WIDTH = 320;

export default function MetricChart({
  points,
  label,
  formatValue = (value) => `${value.toFixed(1)}%`,
  formatAxisTick,
  color = 'var(--color-chart-1)',
  percentAxis = false,
  loading = false,
  windowStart,
  windowEnd,
  truncated = false,
  sampleIntervalMs = null,
  hasAnySample = false,
  latestSampleSummary = null,
}: MetricChartProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(0);
  // hover 时间与点按钉住的时间分开存：鼠标离开只清 hover，钉住的 tooltip
  // 保留（触屏没有 mouseleave，靠再次点按取消）。两者都只存采样时间，
  // 坐标在渲染时按当前 points/geometry 重新解析，窗口切换后旧时间点自动
  // 失效，不再出现「幽灵点」。
  const [hoverTime, setHoverTime] = useState<number | null>(null);
  const [pinnedTime, setPinnedTime] = useState<number | null>(null);
  const activeTime = hoverTime ?? pinnedTime;
  // 键盘读数的 aria-live 文案；鼠标 hover 不更新，避免读屏被无谓打扰。
  const [announcement, setAnnouncement] = useState('');

  // 宽度自适应：ResizeObserver 在容器尺寸变化时更新（含首次挂载）；
  // 卸载/重挂载清理 observer，避免泄漏。
  useEffect(() => {
    const element = containerRef.current;
    if (!element) {
      return;
    }
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        setWidth(entry.contentRect.width);
      }
    });
    observer.observe(element);
    return () => {
      observer.disconnect();
    };
  }, []);

  // 选中窗口（非法/缺省时为 null）。横轴范围优先取窗口边界：稀疏数据
  // 不能再把 2 小时拉满 24 小时整宽。
  const selectedWindow = useMemo(() => {
    if (windowStart === undefined || windowEnd === undefined) {
      return null;
    }
    if (!Number.isFinite(windowStart) || !Number.isFinite(windowEnd) || windowEnd <= windowStart) {
      return null;
    }
    return { start: windowStart, end: windowEnd };
  }, [windowStart, windowEnd]);

  // 窗口外的点直接丢弃，不再钳到绘图区边缘（走查第 13 轮 medium）：
  // 采集暂停后窗口若继续右移，旧点全被钉在左缘，看起来像窗口末端还有
  // 数据。全部点都在窗口外时走「窗口内无采样」空态。
  const visiblePoints = useMemo(() => {
    if (!selectedWindow) {
      return points;
    }
    const { start, end } = selectedWindow;
    return points.filter((point) => point.t >= start && point.t <= end);
  }, [points, selectedWindow]);

  const sampled = useMemo(
    () =>
      needsDownsample(visiblePoints) ? downsample(visiblePoints, CHART_MAX_POINTS) : visiblePoints,
    [visiblePoints],
  );
  const sampledCount = sampled.length;

  // 断档识别必须在降采样之前完成（见 metricSeries.ts 头注释）：中位采样
  // 间隔来自原始点，缺口再按时间映射到抽样后的绘制点。
  const segmentation = useMemo(
    () =>
      segmentSeries(
        visiblePoints,
        sampleIntervalMs === null ? {} : { intervalMs: sampleIntervalMs },
      ),
    [visiblePoints, sampleIntervalMs],
  );
  const rawRuns = useMemo(
    () => splitRunsAtGaps(visiblePoints, segmentation.gaps),
    [visiblePoints, segmentation],
  );
  const sampledRuns = useMemo(
    () => splitRunsAtGaps(sampled, segmentation.gaps),
    [sampled, segmentation],
  );

  const scale = useMemo(() => {
    let max = 0;
    for (const point of sampled) {
      if (point.v > max) {
        max = point.v;
      }
    }
    return percentAxis ? percentYScale(max) : yScale(max);
  }, [sampled, percentAxis]);

  // y 轴刻度文案先全部算出，再按最宽的一条给左留白：旧版固定 44px 时
  // "20.0GB/s" 的左缘为负（20 GB/s 被裁成 "0.0GB/s"）。
  const axisLabels = tickValues(scale).map((value) =>
    formatAxisTick ? formatAxisTick(value, scale.max) : formatAxisValue(value, formatValue),
  );
  const paddingLeft = axisPaddingLeft(axisLabels);

  // 首帧未测量到宽度时按 320 布局（与 svg 缺省宽度一致）：否则 plotWidth
  // 为 0，服务端/首屏会先闪一帧「点数不足」，也无法给 x 轴定位。
  const svgWidth = width > 0 ? width : FALLBACK_WIDTH;
  const plotWidth = Math.max(0, svgWidth - paddingLeft - PADDING_RIGHT);
  const plotHeight = CHART_HEIGHT - PADDING_TOP - PADDING_BOTTOM;

  const summary = useMemo<ChartSummary | null>(() => {
    if (sampledCount === 0) {
      return null;
    }
    const first = sampled[0] as SeriesPoint;
    const last = sampled[sampledCount - 1] as SeriesPoint;
    let min = first.v;
    let max = first.v;
    for (const point of sampled) {
      if (point.v < min) {
        min = point.v;
      }
      if (point.v > max) {
        max = point.v;
      }
    }
    return { first, last, min, max, spanMs: last.t - first.t };
  }, [sampled, sampledCount]);

  // 数据实际覆盖时长（与窗口相交部分）。走查第 8 轮 high：此前只取
  // 首/末采样点跨度，3 小时断档也被算成「已覆盖」，24h 窗口覆盖率恒
  // 100%。现在按断档切段后逐段累加，空白期不再计入。
  //
  // 走查第 14 轮 low：固定「覆盖 ≥90%」在 600s 采样间隔下会把正常的
  // 1h 窗口判成只覆盖 50 分钟。改为允许首尾各少一个采样间隔的容差：
  // spanMs - max(2×观测中位间隔, 2×配置间隔)；只有缺口大于该容差才是
  // 真正覆盖不足。
  const coverage = useMemo(() => {
    if (!selectedWindow || visiblePoints.length < 2) {
      return null;
    }
    const coveredMs = coveredSpanMs(
      visiblePoints,
      rawRuns,
      selectedWindow.start,
      selectedWindow.end,
    );
    const spanMs = selectedWindow.end - selectedWindow.start;
    const observedMs = medianSampleIntervalMs(visiblePoints);
    const configuredMs =
      typeof sampleIntervalMs === 'number' &&
      Number.isFinite(sampleIntervalMs) &&
      sampleIntervalMs > 0
        ? sampleIntervalMs
        : null;
    const toleranceMs = Math.max(
      observedMs === null ? 0 : observedMs * 2,
      configuredMs === null ? 0 : configuredMs * 2,
    );
    if (coveredMs <= 0 || coveredMs >= spanMs - toleranceMs) {
      return null;
    }
    return { coveredMs, spanMs };
  }, [selectedWindow, visiblePoints, rawRuns, sampleIntervalMs]);

  // 时间文案的跨度口径：有窗口时用窗口跨度（24h 窗口里的 2h 数据也给
  // 月/日），否则沿用数据跨度。
  const displaySpanMs = selectedWindow
    ? selectedWindow.end - selectedWindow.start
    : (summary?.spanMs ?? 0);

  // 断档说明：缺口时长与起止时刻，直接写在图下方（即使缺口太窄放不下
  // 图内文字，也不至于只能靠折线断开去猜）。
  const gapNoteText = useMemo(() => {
    const gapCount = segmentation.gaps.length;
    if (gapCount === 0) {
      return null;
    }
    const details = segmentation.gaps
      .slice(0, MAX_GAP_NOTES)
      .map(
        (gap) =>
          `${formatChartTime(gap.fromTime, displaySpanMs)}–${formatChartTime(
            gap.toTime,
            displaySpanMs,
          )}（${formatCoverageDuration(gap.durationMs)}）`,
      )
      .join('；');
    if (gapCount === 1) {
      return `无采样断档：${details}`;
    }
    const totalMs = segmentation.gaps.reduce((sum, gap) => sum + gap.durationMs, 0);
    return `无采样断档 ${gapCount} 处：${details}${
      gapCount > MAX_GAP_NOTES ? ` 等 ${gapCount} 处` : ''
    }，共缺失 ${formatCoverageDuration(totalMs)}`;
  }, [segmentation, displaySpanMs]);

  const geometry = useMemo(() => {
    // 第 10 轮 low：只有 1 个采样点时也要按窗口坐标画出该点，不能把
    // geometry 置 null 后只显示「点数不足」——卡片「当前 42.5%」与 aria
    // 「共 1 个采样点」会互相矛盾。
    if (sampledCount < 1 || plotWidth <= 0) {
      return null;
    }
    // 走查第 7 轮 medium：有选中窗口时轴的起止是窗口边界，而不是首/末
    // 采样点。点超出窗口（客户端/服务端时钟略偏）时钳到绘图区内。
    let tMin: number;
    let tMax: number;
    if (selectedWindow) {
      tMin = selectedWindow.start;
      tMax = selectedWindow.end;
    } else if (sampledCount === 1) {
      // 无窗口的单点独立使用：给 ±1ms 的退化轴，点落在绘图区中央。
      const only = (sampled[0] as SeriesPoint).t;
      tMin = only - 1;
      tMax = only + 1;
    } else {
      tMin = (sampled[0] as SeriesPoint).t;
      tMax = (sampled[sampledCount - 1] as SeriesPoint).t;
    }
    const coords = sampled.map((point) => {
      const offset = Math.min(plotWidth, Math.max(0, mapX(point.t, tMin, tMax, plotWidth)));
      return {
        x: paddingLeft + offset,
        y: PADDING_TOP + mapY(point.v, scale.max, plotHeight),
      };
    });
    return { tMin, tMax, coords };
  }, [sampled, sampledCount, plotWidth, plotHeight, scale, paddingLeft, selectedWindow]);

  // 缺口在绘图区内的位置：只保留与轴范围相交的部分。宽度够的缺口在
  // 带内写「缺失 X」，其余仅画警示带（文字会压到相邻折线）。
  const gapViews = useMemo<GapView[]>(() => {
    if (!geometry || segmentation.gaps.length === 0) {
      return [];
    }
    const views: Omit<GapView, 'showLabel'>[] = [];
    for (const gap of segmentation.gaps) {
      const start = Math.max(gap.fromTime, geometry.tMin);
      const end = Math.min(gap.toTime, geometry.tMax);
      if (end <= start) {
        continue;
      }
      const x1 =
        paddingLeft +
        Math.min(plotWidth, Math.max(0, mapX(start, geometry.tMin, geometry.tMax, plotWidth)));
      const x2 =
        paddingLeft +
        Math.min(plotWidth, Math.max(0, mapX(end, geometry.tMin, geometry.tMax, plotWidth)));
      views.push({
        fromTime: gap.fromTime,
        x1,
        x2,
        widthPx: Math.max(0, x2 - x1),
        label: `缺失 ${formatCoverageDuration(end - start)}`,
      });
    }
    const labelled = new Set(
      views
        .filter((view) => view.widthPx >= GAP_LABEL_MIN_WIDTH)
        .sort((left, right) => right.widthPx - left.widthPx)
        .slice(0, MAX_GAP_LABELS)
        .map((view) => view.fromTime),
    );
    return views.map((view) => ({ ...view, showLabel: labelled.has(view.fromTime) }));
  }, [geometry, segmentation, paddingLeft, plotWidth]);

  // 当前激活点按「时间」在最新采样里重新解析：切窗口后旧时间点不存在，
  // tooltip/圆点随之消失（不会停在新窗口之外或显示旧读数）。
  const activeIndex =
    activeTime === null ? -1 : sampled.findIndex((point) => point.t === activeTime);
  const activePoint = activeIndex >= 0 ? sampled[activeIndex] : undefined;
  const activeCoord = activePoint && geometry ? geometry.coords[activeIndex] : undefined;

  function inspect(index: number, announce: boolean): void {
    const point = sampled[index];
    if (!point) {
      return;
    }
    setHoverTime(point.t);
    if (announce) {
      setAnnouncement(`${formatChartTime(point.t, displaySpanMs)}，${formatValue(point.v)}`);
    }
  }

  /** 触点 → 图内 x（与 handleMove 共用换算）。null 表示落在绘图区外。 */
  function pointerToSvgX(event: React.MouseEvent<SVGSVGElement>): number | null {
    if (!geometry || plotWidth <= 0) {
      return null;
    }
    const rect = event.currentTarget.getBoundingClientRect();
    const svgX = ((event.clientX - rect.left) / rect.width) * svgWidth;
    if (svgX < paddingLeft || svgX > svgWidth - PADDING_RIGHT) {
      return null;
    }
    return svgX;
  }

  function handleMove(event: React.MouseEvent<SVGSVGElement>): void {
    const svgX = pointerToSvgX(event);
    if (svgX === null) {
      setHoverTime(null);
      return;
    }
    const nearestIndex = nearestIndexByX(geometry?.coords ?? [], svgX);
    if (nearestIndex < 0) {
      return;
    }
    inspect(nearestIndex, false);
  }

  /**
   * 触屏点按：按 x 选最近点并钉住 tooltip（同一点再点按取消；点绘图区
   * 外取消）。TopServerBarChart 的 pinnedId 同款思路，只是用时间戳做
   * 身份，切换窗口后旧点不在新数据里就不会误钉。
   */
  function handleTap(event: React.MouseEvent<SVGSVGElement>): void {
    const svgX = pointerToSvgX(event);
    if (svgX === null) {
      setHoverTime(null);
      setPinnedTime(null);
      return;
    }
    const nearestIndex = nearestIndexByX(geometry?.coords ?? [], svgX);
    const point = sampled[nearestIndex];
    if (!point) {
      return;
    }
    setHoverTime(null);
    setPinnedTime((previous) => (previous === point.t ? null : point.t));
  }

  function handleKeyDown(event: React.KeyboardEvent<SVGSVGElement>): void {
    if (sampledCount === 0) {
      return;
    }
    const current = activeIndex >= 0 ? activeIndex : sampledCount - 1;
    let next: number;
    switch (event.key) {
      case 'ArrowLeft':
        next = Math.max(0, current - 1);
        break;
      case 'ArrowRight':
        next = Math.min(sampledCount - 1, current + 1);
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = sampledCount - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    inspect(next, true);
  }

  // 读屏文案按窗口内的原始点数报数：24h 窗口接口最多返回 2880 点
  // （30s 采集），图上只画降采样后的 500 点——只报 500 会让读屏用户误以为
  // 数据量缺失。窗口外的点不参与绘制也不计数（第 13 轮）。
  const sampleCountText =
    sampledCount === visiblePoints.length
      ? `共 ${visiblePoints.length} 个采样点`
      : `共 ${visiblePoints.length} 个采样点（绘制 ${sampledCount}）`;
  // 有选中窗口时，aria 的「窗口」报窗口起止（而非首/末采样点），并
  // 明确数据只覆盖了其中多长——读屏用户也不会把 2 小时当 24 小时。
  const rangeText = selectedWindow
    ? `窗口 ${formatChartTime(selectedWindow.start, displaySpanMs)} 至 ${formatChartTime(
        selectedWindow.end,
        displaySpanMs,
      )}`
    : `窗口 ${formatChartTime(summary?.first.t ?? Number.NaN, displaySpanMs)} 至 ${formatChartTime(
        summary?.last.t ?? Number.NaN,
        displaySpanMs,
      )}`;
  // 服务端截断时，缺口不是「无采样」而是「更早的点未返回」：只说数据
  // 覆盖了多少会把接口上限误报成采集中断。非截断的稀疏窗口仍按实际
  // 覆盖时长提示：缺口不足 1 小时时改用分钟精度并直接写「缺 N 分钟」——
  // 走查第 16 轮 low：23h49m 覆盖曾被四舍五入成「24 小时」，与窗口同值
  // 后看不出缺了多少。大缺口保持原有整小时口径（21h/3h 等比窗口小得
  // 多，不会与「24 小时窗口」同值，分钟精度只会让 20h55m 这类采样边界
  // 误差显得像真实缺口）。
  const coverageGapMs = coverage ? coverage.spanMs - coverage.coveredMs : 0;
  const coverageNote =
    truncated && summary && summary.spanMs > 0
      ? `仅返回最近 ${formatCoverageDuration(summary.spanMs)}（更早的点未返回）`
      : coverage
        ? coverageGapMs < 3_600_000
          ? `数据仅覆盖 ${formatCoverageDurationPrecise(
              coverage.coveredMs,
            )}（${formatCoverageDurationPrecise(
              coverage.spanMs,
            )}窗口内缺 ${formatCoverageDurationPrecise(coverageGapMs)}采样）`
          : `数据仅覆盖 ${formatCoverageDuration(coverage.coveredMs)}（${formatCoverageDuration(
              coverage.spanMs,
            )}窗口内其余时段无采样）`
        : null;
  // 单采样点：图上按窗口坐标画一个点，同时在图下给出时间与值，避免
  // 「当前 42.5%」与「点数不足」互相矛盾（第 10 轮 low）。
  const singlePointNote =
    sampledCount === 1
      ? `仅 1 个采样点（${formatChartTime((sampled[0] as SeriesPoint).t, displaySpanMs)}），值 ${formatValue(
          (sampled[0] as SeriesPoint).v,
        )}`
      : null;
  // 有窗口但所有点都在窗口外：区分「停采但历史仍存在」与「从未采集」
  // （走查第 14 轮 low）。有 latest 行时写出最近一次采样的年龄，用户能
  // 判断是停采 3 天还是采集器从未上报；没有任何 latest 才是「暂无采集
  // 数据」。窗口外但客户端仍有 points 的旧兜底文案保留。
  const emptyText = (() => {
    if (!selectedWindow) {
      return '暂无采集数据';
    }
    if (hasAnySample) {
      return latestSampleSummary
        ? `窗口内无采样（最近一次采样 ${latestSampleSummary}）`
        : '窗口内无采样（最近一次采样时间未知）';
    }
    if (points.length > 0) {
      return '窗口内无采样（采集可能已暂停）';
    }
    return '暂无采集数据';
  })();
  const ariaLabel = summary
    ? `${label ?? '指标'}折线图，${rangeText}${coverageNote ? `，${coverageNote}` : ''}${
        gapNoteText ? `，${gapNoteText}` : ''
      }，${sampleCountText}，最新 ${formatValue(summary.last.v)}，最小 ${formatValue(
        summary.min,
      )}，最大 ${formatValue(summary.max)}；聚焦后可用左右方向键逐点读数`
    : `${label ?? '指标'}折线图，${emptyText}`;

  // 加载态优先于空态：弱网切换服务器/时间窗时新 query 尚无数据，
  // 渲染「暂无采集数据」会被误读成该窗口没有历史数据。
  if (loading && sampledCount === 0) {
    return (
      <div
        ref={containerRef}
        data-testid="metric-chart-loading"
        aria-busy="true"
        style={{ height: CHART_HEIGHT, overflow: 'hidden' }}
      >
        <Skeleton active title={{ width: 140 }} paragraph={{ rows: 3 }} />
      </div>
    );
  }

  if (sampledCount === 0) {
    return (
      <div
        ref={containerRef}
        style={{
          height: CHART_HEIGHT,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          color: 'var(--color-text-tertiary)',
          fontSize: 12,
        }}
      >
        {emptyText}
      </div>
    );
  }

  return (
    <>
      {/* 提示行放在定位容器之外（走查第 15 轮 low）：旧实现把提示行与
          SVG 一起放进 position:relative 容器，tooltip 的 top:PADDING_TOP
          相对整个容器计算，悬停时直接盖住「无采样断档 2 处…」等说明，
          用户反而看不到解释图表口径的上下文。现在浮层只覆盖绘图区。 */}
      {/* 单点提示：时间与值都写出来，图上同时有该点的圆圈。 */}
      {singlePointNote ? (
        <div
          data-testid="metric-chart-single-point-note"
          style={{ fontSize: 11, color: 'var(--color-text-secondary)', marginBottom: 2 }}
        >
          {singlePointNote}
        </div>
      ) : null}
      {/* 覆盖范围提示：24h 窗口里只有 2h 采样时明确写出，别让用户把
          仅有的数据误读成整段历史。 */}
      {coverageNote ? (
        <div
          data-testid="metric-chart-coverage-note"
          style={{ fontSize: 11, color: 'var(--color-text-secondary)', marginBottom: 2 }}
        >
          {coverageNote}
        </div>
      ) : null}
      {/* 断档说明：折线在缺口处断开，同时给出起止时刻与缺失时长；不依赖
          缺口在屏幕上的宽度，窄卡也能读到。 */}
      {gapNoteText ? (
        <div
          data-testid="metric-chart-gap-note"
          style={{ fontSize: 11, color: 'var(--color-warning)', marginBottom: 2 }}
        >
          {gapNoteText}
        </div>
      ) : null}
      <div ref={containerRef} style={{ position: 'relative' }}>
        <svg
          width={svgWidth}
          height={CHART_HEIGHT}
          style={{ display: 'block', width: '100%' }}
          role="img"
          aria-label={ariaLabel}
          tabIndex={0}
          data-testid="metric-chart-svg"
          onFocus={() => {
            if (activeIndex < 0) {
              inspect(sampledCount - 1, true);
            }
          }}
          onBlur={() => {
            setHoverTime(null);
            setAnnouncement('');
          }}
          onKeyDown={handleKeyDown}
          onMouseMove={handleMove}
          onClick={handleTap}
          onMouseLeave={() => {
            setHoverTime(null);
          }}
        >
          <title>{ariaLabel}</title>
          {/* 横向网格 + y 轴刻度（左留白按标签宽度动态计算）。 */}
          <g data-testid="metric-chart-y-axis">
            {tickValues(scale).map((value, index) => {
              const y = PADDING_TOP + mapY(value, scale.max, plotHeight);
              return (
                <g key={index}>
                  <line
                    x1={paddingLeft}
                    x2={svgWidth - PADDING_RIGHT}
                    y1={y}
                    y2={y}
                    stroke="var(--color-border-subtle)"
                    strokeWidth={1}
                  />
                  <text
                    x={paddingLeft - 6}
                    y={y + 3}
                    textAnchor="end"
                    fontSize={10}
                    fill="var(--color-text-tertiary)"
                    data-testid="metric-chart-y-tick"
                  >
                    {axisLabels[index] ?? formatAxisValue(value, formatValue)}
                  </text>
                </g>
              );
            })}
          </g>
          {/* x 轴时间刻度（走查第 2 轮）：3–5 个标签按采样跨度选密度，24h
            等长窗口带月/日，避免只看 HH:mm 分不清昨天/今天。
            走查第 7 轮 medium：有选中窗口时按窗口边界等距打刻度，稀疏
            数据也表达完整 24h，而不是把 2h 数据的起止当成轴范围。 */}
          {geometry ? (
            <g data-testid="metric-chart-time-axis">
              {selectedWindow
                ? windowTickTimes(
                    selectedWindow.start,
                    selectedWindow.end,
                    svgWidth < 420 ? 3 : 5,
                  ).map((time, index, all) => {
                    const isFirst = index === 0;
                    const isLast = index === all.length - 1;
                    const x = isFirst
                      ? paddingLeft
                      : isLast
                        ? svgWidth - PADDING_RIGHT
                        : paddingLeft + mapX(time, geometry.tMin, geometry.tMax, plotWidth);
                    return (
                      <text
                        key={`w-${index}`}
                        x={x}
                        y={PADDING_TOP + plotHeight + 14}
                        textAnchor={isFirst ? 'start' : isLast ? 'end' : 'middle'}
                        fontSize={10}
                        fill="var(--color-text-tertiary)"
                      >
                        {formatChartTime(time, geometry.tMax - geometry.tMin)}
                      </text>
                    );
                  })
                : labelTickIndices(
                    sampledCount,
                    geometry.tMax - geometry.tMin >= 12 * 3_600_000 ? (svgWidth < 360 ? 3 : 4) : 5,
                  ).map((index) => {
                    const coord = geometry.coords[index];
                    const point = sampled[index];
                    if (!coord || !point) {
                      return null;
                    }
                    const isFirst = index === 0;
                    const isLast = index === sampledCount - 1;
                    // 无窗口的单点：首尾同一刻度，放在点的正下方（不再贴左缘）。
                    const only = sampledCount === 1;
                    return (
                      <text
                        key={index}
                        x={
                          only
                            ? coord.x
                            : isFirst
                              ? paddingLeft
                              : isLast
                                ? svgWidth - PADDING_RIGHT
                                : coord.x
                        }
                        y={PADDING_TOP + plotHeight + 14}
                        textAnchor={only ? 'middle' : isFirst ? 'start' : isLast ? 'end' : 'middle'}
                        fontSize={10}
                        fill="var(--color-text-tertiary)"
                      >
                        {formatChartTime(point.t, geometry.tMax - geometry.tMin)}
                      </text>
                    );
                  })}
            </g>
          ) : null}
          {geometry ? (
            <>
              {/* 断档警示带：先画在折线下方，标出无采样区间；宽度够时在带内
                写「缺失 X」，否则由上方断档说明兜底。 */}
              {gapViews.map((gap, index) => (
                <g
                  key={`gap-${gap.fromTime}-${index}`}
                  data-testid="metric-chart-gap-band"
                  aria-hidden="true"
                >
                  <rect
                    x={gap.x1}
                    y={PADDING_TOP}
                    width={Math.max(1, gap.x2 - gap.x1)}
                    height={plotHeight}
                    fill="var(--color-warning)"
                    opacity={0.08}
                  />
                  {gap.showLabel ? (
                    <text
                      x={(gap.x1 + gap.x2) / 2}
                      y={PADDING_TOP + 10}
                      textAnchor="middle"
                      fontSize={10}
                      fill="var(--color-warning)"
                      data-testid="metric-chart-gap-label"
                    >
                      {gap.label}
                    </text>
                  ) : null}
                </g>
              ))}
              {/* 发现断档后每段各画一条 polyline：缺口处不再用直线跨接。
                单点段画圆点，与 splitLineRuns 的断线语义一致。 */}
              {sampledRuns.map((run, runIndex) => {
                const coordinates: Array<{ x: number; y: number }> = [];
                for (const pointIndex of run) {
                  const coordinate = geometry.coords[pointIndex];
                  if (coordinate) {
                    coordinates.push(coordinate);
                  }
                }
                const single = coordinates.length === 1 ? coordinates[0] : undefined;
                if (single) {
                  return (
                    <circle
                      key={`series-dot-${runIndex}`}
                      data-testid="metric-chart-series-dot"
                      cx={single.x}
                      cy={single.y}
                      r={2.5}
                      fill={color}
                    />
                  );
                }
                if (coordinates.length < 2) {
                  return null;
                }
                return (
                  <polyline
                    key={`series-${runIndex}`}
                    data-testid="metric-chart-series-segment"
                    points={buildPolyline(coordinates)}
                    fill="none"
                    stroke={color}
                    strokeWidth={1.6}
                  />
                );
              })}
              {activePoint && activeCoord ? (
                <g>
                  <line
                    x1={activeCoord.x}
                    x2={activeCoord.x}
                    y1={PADDING_TOP}
                    y2={PADDING_TOP + plotHeight}
                    stroke="var(--color-border-strong)"
                    strokeDasharray="3 3"
                  />
                  <circle
                    cx={activeCoord.x}
                    cy={PADDING_TOP + mapY(activePoint.v, scale.max, plotHeight)}
                    r={3}
                    fill={color}
                  />
                </g>
              ) : null}
            </>
          ) : (
            <text
              x={paddingLeft + plotWidth / 2}
              y={PADDING_TOP + plotHeight / 2}
              textAnchor="middle"
              fontSize={12}
              fill="var(--color-text-tertiary)"
            >
              图表宽度不足，无法绘制
            </text>
          )}
        </svg>
        <div data-testid="metric-chart-announcement" aria-live="polite" style={VISUALLY_HIDDEN}>
          {announcement}
        </div>
        {activePoint && activeCoord ? (
          <div
            data-testid="metric-chart-tooltip"
            style={{
              position: 'absolute',
              left: Math.min(Math.max(activeCoord.x + 10, 4), Math.max(svgWidth - 150, 4)),
              top: PADDING_TOP,
              background: 'var(--color-bg-elevated)',
              // 七期走查：浅色下浮层底与卡片底同为 --color-bg-container，只有
              // 背景色时浮层边界完全消失（同底色叠同底色）。两主题同名变量
              // 分层：浅色靠 --color-shadow、深色靠 --color-border 描边
              // （8.3.7 的「不得用阴影代替描边」反之亦然——两者都给，任何
              // 主题都不会塌）。
              border: '1px solid var(--color-border)',
              boxShadow: 'var(--color-shadow)',
              borderRadius: 6,
              padding: '4px 8px',
              fontSize: 12,
              color: 'var(--color-text)',
              pointerEvents: 'none',
              whiteSpace: 'nowrap',
            }}
          >
            <div>
              {geometry
                ? formatChartTime(activePoint.t, geometry.tMax - geometry.tMin)
                : formatChartTime(activePoint.t, 0)}
            </div>
            <div style={{ fontWeight: 600 }}>{formatValue(activePoint.v)}</div>
          </div>
        ) : null}
      </div>
    </>
  );
}
