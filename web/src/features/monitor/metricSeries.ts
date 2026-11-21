// 指标折线断档识别（走查第 8 轮 high）。
//
// 运维在 24 小时窗口里遇到「采集中断 3 小时」时，旧版 MetricChart 把缺口
// 两端直接连成一段直线，且只在首/末采样点上算覆盖率——断档被当成连续
// 采样。本模块按相邻采样 Δt 切段：
//
//   · 判定基准优先取原始点的**中位采样间隔**，超过其 2 倍即视为断档；
//     没有足够点在时用调用方给的采集间隔（collector interval_seconds）
//     兜底。
//   · 判定必须在**降采样之前**做：LTTB 把 2880 点压到 500 点后，相邻
//     绘制点本身就可能相差数个原始点，若在抽样序列上判定会把正常抽样
//     误报成断档。绘制层再按时间把缺口映射到（可能已抽样的）点。
//
// 输入来自 GET /api/servers/:id/metrics（30s 最小间隔 × 24h 最多 2880
// 点），按 MAX_SEGMENT_POINTS 设硬上限；缺口列表另有 MAX_GAPS 上限，避免
// 异常输入生成无界数组。

import type { SeriesPoint } from './downsample';

/**
 * 断档扫描的最大原始点数。后端上限 2880（30s × 24h），留出余量；
 * 旧值 2000 会让 30s 满窗的最早 880 点不参与切段与覆盖率计算。
 */
export const MAX_SEGMENT_POINTS = 3000;
/** 单次识别最多记录的缺口数（超出后不再追加，仍会在既有缺口处断线）。 */
export const MAX_GAPS = 64;
/** 相邻点 Δt 超过基线采样间隔的倍数即可能是断档。 */
export const GAP_INTERVAL_FACTOR = 2;
/**
 * 连续多少个「新间隔」视为采集间隔换挡而非断档。运维把采集间隔从 60s
 * 调到 600s 后，24h 窗口尾部会连续出现 600s 间隔；只有 ≥3 个连续、
 * 彼此同档的新间隔才认定为新稳态（2 个仍可能是漏采 1–2 次）。
 */
export const GEAR_CHANGE_MIN_RUN = 3;
/** 同一档位间隔允许的相对偏差（±10%）。 */
export const GEAR_CHANGE_TOLERANCE = 0.1;
/**
 * 稀疏样本阈值：≤4 个点时两侧间隔的中位数会被单个缺口带偏（3 个点、
 * 23h 前 / 23h+60s / 20h 前的间隔是 60s 与 3h，中位 1.5h，阈值 3h，
 * 3h 缺口恰好不满足 delta > threshold）。≤4 点时改用观测到的最小间隔
 * 作基准：单个缺口不会抬高它，均匀稀疏序列也不会被逐段断开。
 */
export const SPARSE_SAMPLE_MAX = 4;

export interface SeriesGap {
  /** 缺口前最后一个采样的时间（epoch ms）。 */
  fromTime: number;
  /** 缺口后第一个采样的时间（epoch ms）。 */
  toTime: number;
  /** 缺口时长 = toTime - fromTime。 */
  durationMs: number;
}

export interface SeriesSegmentation {
  /** 判定的断档阈值（ms）；点数不足且无采集间隔时为 null。 */
  thresholdMs: number | null;
  /** 按时间升序的缺口列表（至多 MAX_GAPS 条）。 */
  gaps: SeriesGap[];
}

function finitePositive(value: number | null | undefined): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : null;
}

/**
 * 相邻采样间隔（ms），跳过非正/非有限的 Δt。数组长度不超过
 * MAX_SEGMENT_POINTS-1，且容量提前钉死：异常长的输入不会越界增长。
 */
function collectIntervals(points: readonly SeriesPoint[], limit: number): number[] {
  const bounded = Math.max(0, Math.min(points.length, Math.floor(limit)));
  if (bounded < 2) {
    return [];
  }
  const capacity = Math.min(MAX_SEGMENT_POINTS, bounded - 1);
  const intervals: number[] = [];
  for (let index = 1; index < bounded && intervals.length < capacity; index++) {
    const previous = points[index - 1];
    const current = points[index];
    if (!previous || !current) {
      continue;
    }
    const delta = current.t - previous.t;
    if (Number.isFinite(delta) && delta > 0) {
      intervals.push(delta);
    }
  }
  return intervals;
}

/** 已排序间隔数组的中位数；空数组返回 null。 */
function medianOfSortedIntervals(intervals: readonly number[]): number | null {
  if (intervals.length === 0) {
    return null;
  }
  const middle = Math.floor(intervals.length / 2);
  const upper = intervals[middle];
  if (upper === undefined) {
    return null;
  }
  if (intervals.length % 2 === 1) {
    return upper;
  }
  const lower = intervals[middle - 1];
  return lower === undefined ? upper : (lower + upper) / 2;
}

/** 两个间隔是否属于同一档位（±10%，至少 ±1s 容差）。 */
function sameIntervalBand(left: number, right: number): boolean {
  const max = Math.max(left, right);
  if (max <= 0) {
    return false;
  }
  return Math.abs(left - right) <= Math.max(1_000, max * GEAR_CHANGE_TOLERANCE);
}

/**
 * 原始点的中位采样间隔（ms）。跳过非正/非有限的 Δt；不足 2 个有效
 * 间隔时返回 null。最多扫描 limit 个点，内部分配的数组同样有界。
 */
export function medianSampleIntervalMs(
  points: readonly SeriesPoint[],
  limit = MAX_SEGMENT_POINTS,
): number | null {
  const intervals = collectIntervals(points, limit);
  intervals.sort((left, right) => left - right);
  return medianOfSortedIntervals(intervals);
}

/**
 * 观测间隔的主众数（ms）：按 ±10% 容差把间隔分成档位，取样本数最多的
 * 档位中位值。24h 窗口里混有历史归档间隔与调整后的新间隔时，中位数会
 * 被多数派拉向旧口径；主众数仍是多数派没错，但配合 segmentSeries 的
 * 「连续新间隔 = 换挡」识别，少数派的稳定新档位不再逐个报断档。
 *
 * 空输入/全部无效返回 null。排序与分档都是一次有界扫描（≤3000 项）。
 */
export function dominantSampleIntervalMs(
  points: readonly SeriesPoint[],
  limit = MAX_SEGMENT_POINTS,
): number | null {
  const intervals = collectIntervals(points, limit);
  intervals.sort((left, right) => left - right);
  return dominantOfSortedIntervals(intervals);
}

/**
 * 原始点的最小采样间隔（ms）。跳过非正/非有限的 Δt；不足 1 个有效
 * 间隔时返回 null。与中位数不同，单个大缺口不会抬高它（稀疏样本判定
 * 用的就是它，见 segmentSeries）。只做一次有界扫描，不分配数组。
 */
export function minSampleIntervalMs(
  points: readonly SeriesPoint[],
  limit = MAX_SEGMENT_POINTS,
): number | null {
  const bounded = Math.max(0, Math.min(points.length, Math.floor(limit)));
  if (bounded < 2) {
    return null;
  }
  let min: number | null = null;
  for (let index = 1; index < bounded; index++) {
    const previous = points[index - 1];
    const current = points[index];
    if (!previous || !current) {
      continue;
    }
    const delta = current.t - previous.t;
    if (!Number.isFinite(delta) || delta <= 0) {
      continue;
    }
    if (min === null || delta < min) {
      min = delta;
    }
  }
  return min;
}

/**
 * 识别断档。
 *
 * `intervalMs` 是采集器配置的采样间隔（可空）。基线优先取观测间隔的
 * **主众数**：24h 窗口里历史数据可能按 5 分钟归档，而采集器配置是 60s，
 * 用 2×60s 会把每个 5 分钟点都误判成断档。中位数只在主众数缺失时兜底。
 *
 * 采集间隔调整（如 60s → 600s）会让窗口尾部连续出现 600s 间隔。整段
 * 连续 ≥GEAR_CHANGE_MIN_RUN 个、彼此同档的「新间隔」是换挡后的新稳态，
 * 不计断档；孤立的大间隔（前后仍是原档位）才是真断档（batch-04 的 3h
 * 缺口）。稀疏样本（≤4 点，见 SPARSE_SAMPLE_MAX）例外：中位数会被单个
 * 缺口带偏（3 点、间隔 60s 与 3h 时中位 1.5h、阈值 3h，3h 缺口恰好不
 * 满足 delta > threshold），此时改用观测到的**最小**间隔作基准——均匀
 * 稀疏（如 3 点各隔 1h）仍不误报，单一缺口却能被 2×最小间隔识别。
 * 配置间隔仍只在完全算不出观测间隔时兜底。
 */
export function segmentSeries(
  points: readonly SeriesPoint[],
  options: { intervalMs?: number | null; limit?: number } = {},
): SeriesSegmentation {
  const limit = Math.max(2, Math.floor(options.limit ?? MAX_SEGMENT_POINTS));
  const bounded = Math.max(0, Math.min(points.length, limit));
  const interval = finitePositive(options.intervalMs);
  if (bounded < 2) {
    return { thresholdMs: null, gaps: [] };
  }
  // 相邻点对（有效 Δt）。最多 bounded-1 项，循环有界。
  const entries: Array<{ fromTime: number; toTime: number; delta: number }> = [];
  for (let index = 1; index < bounded && entries.length < MAX_SEGMENT_POINTS - 1; index++) {
    const previous = points[index - 1];
    const current = points[index];
    if (!previous || !current) {
      continue;
    }
    const delta = current.t - previous.t;
    if (Number.isFinite(delta) && delta > 0) {
      entries.push({ fromTime: previous.t, toTime: current.t, delta });
    }
  }
  if (entries.length === 0) {
    return { thresholdMs: null, gaps: [] };
  }
  const intervals = entries.map((entry) => entry.delta);
  intervals.sort((left, right) => left - right);
  // 稀疏样本（≤4 点）的中位数会被单一缺口带偏：3 个点的间隔是 60s 与
  // 3h 时中位 1.5h、阈值 3h，缺口恰好被吞掉（走查第 9 轮 low）。此时
  // 改用最小间隔作基准（60s → 阈值 120s）；均匀稀疏序列的最小间隔等于
  // 正常间隔，不会把每个点都断开。配置间隔只在算不出观测间隔时兜底。
  const sparseBase = bounded <= SPARSE_SAMPLE_MAX ? (intervals[0] ?? null) : null;
  const dominant = bounded > SPARSE_SAMPLE_MAX ? dominantOfSortedIntervals(intervals) : null;
  const base = sparseBase ?? dominant ?? medianOfSortedIntervals(intervals) ?? interval;
  if (base === null) {
    return { thresholdMs: null, gaps: [] };
  }
  const thresholdMs = base * GAP_INTERVAL_FACTOR;
  // 换挡标记：连续 ≥3 个超过阈值且彼此同档的间隔 = 新稳态，不算缺口。
  // 标志数组与 entries 同长（≤2999），不随输入无限增长。
  const steady = new Array<boolean>(entries.length).fill(false);
  let cursor = 0;
  while (cursor < entries.length) {
    const current = entries[cursor];
    if (!current || current.delta <= thresholdMs) {
      cursor++;
      continue;
    }
    let end = cursor + 1;
    while (end < entries.length) {
      const next = entries[end];
      if (!next || next.delta <= thresholdMs || !sameIntervalBand(next.delta, current.delta)) {
        break;
      }
      end++;
    }
    if (end - cursor >= GEAR_CHANGE_MIN_RUN) {
      for (let index = cursor; index < end; index++) {
        steady[index] = true;
      }
    }
    cursor = end;
  }
  const gaps: SeriesGap[] = [];
  for (let index = 0; index < entries.length && gaps.length < MAX_GAPS; index++) {
    const entry = entries[index];
    if (!entry || steady[index] || entry.delta <= thresholdMs) {
      continue;
    }
    gaps.push({ fromTime: entry.fromTime, toTime: entry.toTime, durationMs: entry.delta });
  }
  return { thresholdMs, gaps };
}

/**
 * 已排序间隔数组的主众数：按 ±10% 容差分档，取样本数最多的档位中位值。
 * 与 {@link dominantSampleIntervalMs} 共用分档规则；本函数不再排序/复制。
 */
function dominantOfSortedIntervals(intervals: readonly number[]): number | null {
  if (intervals.length === 0) {
    return null;
  }
  let bestValue: number | null = null;
  let bestCount = 0;
  let start = 0;
  while (start < intervals.length) {
    const anchor = intervals[start];
    if (anchor === undefined) {
      break;
    }
    let end = start + 1;
    while (end < intervals.length && sameIntervalBand(intervals[end] ?? anchor, anchor)) {
      end++;
    }
    const count = end - start;
    const middle = intervals[start + Math.floor((count - 1) / 2)] ?? anchor;
    if (count > bestCount || (count === bestCount && bestValue !== null && middle < bestValue)) {
      bestCount = count;
      bestValue = middle;
    }
    start = end;
  }
  return bestValue;
}

/**
 * 按缺口把点序列切成连续段，返回点下标数组。
 *
 * 断线语义与 ChartModel.splitLineRuns 一致（缺口处断开、单点段保留），
 * 但这里除了「缺口落在相邻两点之间」，还要处理降采样后的情形：抽样点
 * 可能跨过缺口，只要某个缺口完全落在相邻两个绘制点的时间区间内，就
 * 在这两个点之间断开。扫描一次、缺口指针不回退，段数不超过点数。
 */
export function splitRunsAtGaps(
  points: readonly SeriesPoint[],
  gaps: readonly SeriesGap[],
  limit = MAX_SEGMENT_POINTS,
): number[][] {
  const bounded = Math.max(0, Math.min(points.length, Math.floor(limit)));
  if (bounded === 0) {
    return [];
  }
  const gapCount = Math.max(0, Math.min(gaps.length, MAX_GAPS));
  const runs: number[][] = [];
  let current: number[] = [];
  let gapIndex = 0;
  for (let index = 0; index < bounded; index++) {
    current.push(index);
    const next = index + 1 < bounded ? points[index + 1] : undefined;
    const point = points[index];
    if (!next || !point) {
      continue;
    }
    // 丢弃已经完全落在当前点之前的缺口。
    let gap = gapIndex < gapCount ? gaps[gapIndex] : undefined;
    while (gap && gap.toTime <= point.t) {
      gapIndex++;
      gap = gapIndex < gapCount ? gaps[gapIndex] : undefined;
    }
    if (gap && gap.fromTime >= point.t && gap.toTime <= next.t) {
      runs.push(current);
      current = [];
      gapIndex++;
    }
  }
  if (current.length > 0) {
    runs.push(current);
  }
  return runs;
}

/**
 * 实际样本覆盖时长：各连续段与窗口交集长度之和（断档不计入）。
 * 没有缺口时等价于「首末采样点跨度」，有缺口时不再把空白期算进覆盖。
 */
export function coveredSpanMs(
  points: readonly SeriesPoint[],
  runs: readonly number[][],
  windowStart: number,
  windowEnd: number,
): number {
  if (!Number.isFinite(windowStart) || !Number.isFinite(windowEnd) || windowEnd <= windowStart) {
    return 0;
  }
  let covered = 0;
  for (const run of runs) {
    const first = run[0];
    const last = run[run.length - 1];
    if (first === undefined || last === undefined) {
      continue;
    }
    const startPoint = points[first];
    const endPoint = points[last];
    if (!startPoint || !endPoint) {
      continue;
    }
    const start = Math.max(startPoint.t, windowStart);
    const end = Math.min(endPoint.t, windowEnd);
    if (end > start) {
      covered += end - start;
    }
  }
  return covered;
}
