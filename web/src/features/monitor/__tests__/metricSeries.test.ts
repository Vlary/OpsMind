// 指标折线断档识别（走查第 8 轮 high）：中位采样间隔、缺口切段、覆盖率
// 不再把断档算成已覆盖。node 环境直测。
import { describe, expect, it } from 'vitest';

import {
  coveredSpanMs,
  dominantSampleIntervalMs,
  GAP_INTERVAL_FACTOR,
  GEAR_CHANGE_MIN_RUN,
  MAX_GAPS,
  medianSampleIntervalMs,
  minSampleIntervalMs,
  segmentSeries,
  SPARSE_SAMPLE_MAX,
  splitRunsAtGaps,
} from '../metricSeries';
import type { SeriesPoint } from '../downsample';

const MINUTE = 60_000;
const HOUR = 3_600_000;
const T0 = Date.UTC(2026, 10, 19, 0, 0);

/** 从 start 起每 stepMs 一个点，跳过 (skipFrom, skipTo) 区间。 */
function seriesWithGap(options: {
  start: number;
  count: number;
  stepMs: number;
  skipFrom?: number;
  skipTo?: number;
}): SeriesPoint[] {
  const points: SeriesPoint[] = [];
  for (let index = 0; index < options.count; index++) {
    const t = options.start + index * options.stepMs;
    if (options.skipFrom !== undefined && options.skipTo !== undefined) {
      if (t > options.skipFrom && t < options.skipTo) {
        continue;
      }
    }
    points.push({ t, v: 10 + (index % 7) });
  }
  return points;
}

describe('medianSampleIntervalMs', () => {
  it('规则 5 分钟采样 → 中位间隔 5 分钟', () => {
    const points = seriesWithGap({ start: T0, count: 20, stepMs: 5 * MINUTE });
    expect(medianSampleIntervalMs(points)).toBe(5 * MINUTE);
  });

  it('忽略非正/非有限 Δt 与不足两点的输入', () => {
    expect(medianSampleIntervalMs([])).toBeNull();
    expect(medianSampleIntervalMs([{ t: T0, v: 1 }])).toBeNull();
    expect(
      medianSampleIntervalMs([
        { t: T0 + 2 * MINUTE, v: 1 },
        { t: T0 + 2 * MINUTE, v: 2 },
        { t: Number.NaN, v: 3 },
      ]),
    ).toBeNull();
  });

  it('偶数个间隔取中间两个的均值', () => {
    const points = [
      { t: T0, v: 1 },
      { t: T0 + 1 * MINUTE, v: 2 },
      { t: T0 + 3 * MINUTE, v: 3 },
      { t: T0 + 10 * MINUTE, v: 4 },
    ];
    // Δt = 1, 2, 7 分钟 → 中位 2 分钟。
    expect(medianSampleIntervalMs(points)).toBe(2 * MINUTE);
  });
});

describe('segmentSeries（相邻 Δt 超过 2 倍中位间隔即断档）', () => {
  it('5 分钟采样中空 3 小时：识别出一处 3 小时缺口', () => {
    const gapFrom = T0 + 4 * HOUR;
    const gapTo = gapFrom + 3 * HOUR;
    const points = seriesWithGap({
      start: T0,
      count: 12 * 12,
      stepMs: 5 * MINUTE,
      skipFrom: gapFrom,
      skipTo: gapTo,
    });
    const segmentation = segmentSeries(points);
    expect(segmentation.thresholdMs).toBe(5 * MINUTE * GAP_INTERVAL_FACTOR);
    expect(segmentation.gaps).toHaveLength(1);
    expect(segmentation.gaps[0]).toEqual({
      fromTime: gapFrom,
      toTime: gapTo,
      durationMs: 3 * HOUR,
    });
  });

  it('正常抖动（5 分钟 ±10 秒）不误判断档', () => {
    const points = Array.from({ length: 40 }, (_, index) => ({
      t: T0 + index * 5 * MINUTE + (index % 2 === 0 ? 0 : 10_000),
      v: index,
    }));
    expect(segmentSeries(points).gaps).toEqual([]);
  });

  it('1440 点 60s 连续序列（24h 满窗）没有断档', () => {
    const points = seriesWithGap({ start: T0, count: 1440, stepMs: MINUTE });
    const segmentation = segmentSeries(points);
    expect(segmentation.gaps).toHaveLength(0);
  });

  it('2880 点 30s 连续序列（24h 满窗）全部参与切段与覆盖率', () => {
    // 后端上限 2880（30s × 24h）：旧 MAX_SEGMENT_POINTS=2000 会静默丢掉
    // 最早 880 点，覆盖率与断档判定都只看最近 2000 点。
    const points = seriesWithGap({ start: T0, count: 2880, stepMs: 30_000 });
    const segmentation = segmentSeries(points);
    expect(segmentation.gaps).toHaveLength(0);
    const runs = splitRunsAtGaps(points, segmentation.gaps);
    expect(runs).toHaveLength(1);
    expect(runs[0]).toHaveLength(2880);
    expect(coveredSpanMs(points, runs, T0, T0 + 24 * HOUR)).toBe(2879 * 30_000);
  });

  it('点数不足且无采集间隔时不给阈值（不瞎断线）', () => {
    expect(segmentSeries([]).thresholdMs).toBeNull();
    expect(segmentSeries([{ t: T0, v: 1 }]).gaps).toEqual([]);
  });

  it('样本缺失的时间戳不会中断扫描或产生假缺口', () => {
    const points = seriesWithGap({ start: T0, count: 30, stepMs: 5 * MINUTE }); // 2.5h 连续
    const withBad = [points[0] as SeriesPoint, { t: Number.NaN, v: 0 }, ...points.slice(1)];
    const segmentation = segmentSeries(withBad);
    // 坏点把序列一分为二，但两个子序列都连续，缺口只可能来自坏点处；
    // 这里只断言不会产生超过 1 条且不会崩。
    expect(segmentation.gaps.length).toBeLessThanOrEqual(1);
  });
});

describe('minSampleIntervalMs（稀疏样本的稳健基准）', () => {
  it('最小间隔不被单个大缺口抬高', () => {
    expect(
      minSampleIntervalMs([
        { t: T0, v: 1 },
        { t: T0 + MINUTE, v: 2 },
        { t: T0 + 3 * HOUR + MINUTE, v: 3 },
      ]),
    ).toBe(MINUTE);
  });

  it('空/单点/全坏时间戳返回 null', () => {
    expect(minSampleIntervalMs([])).toBeNull();
    expect(minSampleIntervalMs([{ t: T0, v: 1 }])).toBeNull();
    expect(
      minSampleIntervalMs([
        { t: Number.NaN, v: 1 },
        { t: T0, v: 2 },
      ]),
    ).toBeNull();
  });
});

describe('segmentSeries 稀疏样本（走查第 9 轮 low）', () => {
  // 3 点：23h 前、23h+60s、20h 前——间隔 60s 与 3h，中位 1.5h。
  const sparse = [
    { t: T0, v: 10 },
    { t: T0 + MINUTE, v: 11 },
    { t: T0 + 3 * HOUR + MINUTE, v: 12 },
  ];

  it('无采集间隔时改用最小间隔：3h 缺口不再被中位数 1.5h 掩盖', () => {
    const segmentation = segmentSeries(sparse);
    expect(segmentation.thresholdMs).toBe(MINUTE * GAP_INTERVAL_FACTOR);
    expect(segmentation.gaps).toEqual([
      { fromTime: T0 + MINUTE, toTime: T0 + 3 * HOUR + MINUTE, durationMs: 3 * HOUR },
    ]);
  });

  it('配置 60s 采集间隔时同样识别出 3h 缺口，60s 间隔不误报', () => {
    const segmentation = segmentSeries(sparse, { intervalMs: MINUTE });
    expect(SPARSE_SAMPLE_MAX).toBe(4);
    expect(segmentation.thresholdMs).toBe(MINUTE * GAP_INTERVAL_FACTOR);
    expect(segmentation.gaps).toEqual([
      { fromTime: T0 + MINUTE, toTime: T0 + 3 * HOUR + MINUTE, durationMs: 3 * HOUR },
    ]);
  });

  it('均匀稀疏（4 个 5 分钟点）即使配置 60s 也不逐段误报', () => {
    // 最小间隔 = 5min → 阈值 10min；不能把稀疏本身当成缺口。
    const evenly = Array.from({ length: 4 }, (_, index) => ({
      t: T0 + index * 5 * MINUTE,
      v: index,
    }));
    const segmentation = segmentSeries(evenly, { intervalMs: MINUTE });
    expect(segmentation.thresholdMs).toBe(5 * MINUTE * GAP_INTERVAL_FACTOR);
    expect(segmentation.gaps).toEqual([]);
  });

  it('点数较多时仍以观测中位间隔为准（5 分钟归档不被 60s 配置误报）', () => {
    // 12 个 5 分钟点超过 SPARSE_SAMPLE_MAX：阈值回到 2×5min。
    const dense = Array.from({ length: 12 }, (_, index) => ({
      t: T0 + index * 5 * MINUTE,
      v: index,
    }));
    const segmentation = segmentSeries(dense, { intervalMs: MINUTE });
    expect(segmentation.thresholdMs).toBe(5 * MINUTE * GAP_INTERVAL_FACTOR);
    expect(segmentation.gaps).toEqual([]);
  });
});

describe('采集间隔换挡（走查第 14 轮 medium）', () => {
  /** 先按 prefixStep 采 prefix 个间隔，再按 tailStep 采 tail 个间隔。 */
  function mixedSeries(
    prefix: number,
    prefixStep: number,
    tail: number,
    tailStep: number,
  ): SeriesPoint[] {
    let t = T0;
    const points: SeriesPoint[] = [{ t, v: 0 }];
    for (let index = 0; index < prefix; index++) {
      t += prefixStep;
      points.push({ t, v: index });
    }
    for (let index = 0; index < tail; index++) {
      t += tailStep;
      points.push({ t, v: index });
    }
    return points;
  }

  it('主众数取占多数的间隔：687×60s + 72×600s → 基线 60s', () => {
    const points = mixedSeries(687, MINUTE, 72, 10 * MINUTE);
    expect(medianSampleIntervalMs(points)).toBe(MINUTE);
    expect(dominantSampleIntervalMs(points)).toBe(MINUTE);
  });

  it('24h 窗口尾部 72 个 600s 间隔是换挡，不再报 72 处断档', () => {
    const points = mixedSeries(687, MINUTE, 72, 10 * MINUTE);
    const segmentation = segmentSeries(points);
    expect(segmentation.thresholdMs).toBe(MINUTE * GAP_INTERVAL_FACTOR);
    expect(GEAR_CHANGE_MIN_RUN).toBe(3);
    expect(segmentation.gaps).toEqual([]);
  });

  it('换挡后的真断档仍然识别：3h 缺口只报 1 处', () => {
    const points = mixedSeries(60, MINUTE, 10, 10 * MINUTE);
    const last = points[points.length - 1] as SeriesPoint;
    let t = last.t;
    t += 3 * HOUR;
    points.push({ t, v: 100 });
    for (let index = 0; index < 5; index++) {
      t += 10 * MINUTE;
      points.push({ t, v: 101 + index });
    }
    const segmentation = segmentSeries(points);
    expect(segmentation.gaps).toHaveLength(1);
    expect(segmentation.gaps[0]?.durationMs).toBe(3 * HOUR);
  });

  it('连续 3 个新间隔（中段）视为换挡；只有 2 个仍按缺口处理', () => {
    const three = mixedSeries(20, MINUTE, 3, 10 * MINUTE);
    for (let index = 0; index < 20; index++) {
      const last = three[three.length - 1] as SeriesPoint;
      three.push({ t: last.t + MINUTE, v: 200 + index });
    }
    expect(segmentSeries(three).gaps).toEqual([]);

    const two = mixedSeries(20, MINUTE, 2, 10 * MINUTE);
    for (let index = 0; index < 20; index++) {
      const last = two[two.length - 1] as SeriesPoint;
      two.push({ t: last.t + MINUTE, v: 200 + index });
    }
    expect(segmentSeries(two).gaps).toHaveLength(2);
  });

  it('历史 5 分钟归档 + 新 60s 采集：主众数/中位数均不逐点误报', () => {
    const points = mixedSeries(120, 5 * MINUTE, 60, MINUTE);
    expect(segmentSeries(points).gaps).toEqual([]);
  });
});

describe('splitRunsAtGaps（与 splitLineRuns 相同的断线语义）', () => {
  it('原始点：缺口两侧切成两段，边界点都保留', () => {
    const points = [
      { t: T0, v: 1 },
      { t: T0 + 5 * MINUTE, v: 2 },
      { t: T0 + 3 * HOUR, v: 3 },
      { t: T0 + 3 * HOUR + 5 * MINUTE, v: 4 },
    ];
    const segmentation = segmentSeries(points);
    const runs = splitRunsAtGaps(points, segmentation.gaps);
    expect(runs).toEqual([
      [0, 1],
      [2, 3],
    ]);
  });

  it('降采样后：缺口落在两个抽样点之间时仍断开', () => {
    const points = [
      { t: T0, v: 1 },
      { t: T0 + 5 * MINUTE, v: 2 },
      { t: T0 + 3 * HOUR, v: 3 },
      { t: T0 + 3 * HOUR + 5 * MINUTE, v: 4 },
    ];
    const segmentation = segmentSeries(points);
    // 模拟 LTTB 丢掉缺口两侧紧邻点，只留下更早/更晚的点。
    const sampled = [points[0] as SeriesPoint, points[3] as SeriesPoint];
    const runs = splitRunsAtGaps(sampled, segmentation.gaps);
    expect(runs).toEqual([[0], [1]]);
  });

  it('无缺口时只有一段；空输入返回空数组', () => {
    const points = seriesWithGap({ start: T0, count: 10, stepMs: 5 * MINUTE });
    expect(splitRunsAtGaps(points, [])).toEqual([[0, 1, 2, 3, 4, 5, 6, 7, 8, 9]]);
    expect(splitRunsAtGaps([], [])).toEqual([]);
  });

  it('缺口数量有硬上限（MAX_GAPS）', () => {
    const gaps = Array.from({ length: MAX_GAPS + 20 }, (_, index) => ({
      fromTime: T0 + index * HOUR,
      toTime: T0 + index * HOUR + 30 * MINUTE,
      durationMs: 30 * MINUTE,
    }));
    const points = Array.from({ length: 10 }, (_, index) => ({ t: T0 + index * HOUR, v: index }));
    // 不抛异常且段数不超过点数（防无界增长）。
    const runs = splitRunsAtGaps(points, gaps);
    expect(runs.length).toBeLessThanOrEqual(points.length);
  });
});

describe('coveredSpanMs（覆盖率按实际样本，而不是首末跨度）', () => {
  const windowStart = T0;
  const windowEnd = T0 + 24 * HOUR;

  it('24h 窗口内断采 3 小时：覆盖 21 小时，缺口不计入', () => {
    const gapFrom = T0 + 16 * HOUR;
    const gapTo = gapFrom + 3 * HOUR;
    const points = seriesWithGap({
      start: T0,
      count: 24 * 12 + 1,
      stepMs: 5 * MINUTE,
      skipFrom: gapFrom,
      skipTo: gapTo,
    });
    const segmentation = segmentSeries(points);
    const runs = splitRunsAtGaps(points, segmentation.gaps);
    expect(coveredSpanMs(points, runs, windowStart, windowEnd)).toBe(21 * HOUR);
  });

  it('无缺口时等于首末采样点跨度（保持旧口径）', () => {
    const points = seriesWithGap({ start: T0, count: 289, stepMs: 5 * MINUTE });
    const segmentation = segmentSeries(points);
    const runs = splitRunsAtGaps(points, segmentation.gaps);
    expect(coveredSpanMs(points, runs, windowStart, windowEnd)).toBe(24 * HOUR);
  });

  it('段与窗口相交部分才计数（窗口外的点被裁剪）', () => {
    const points = [
      { t: T0 - 2 * HOUR, v: 1 },
      { t: T0 + 2 * HOUR, v: 2 },
    ];
    const runs = splitRunsAtGaps(points, []);
    expect(coveredSpanMs(points, runs, windowStart, windowEnd)).toBe(2 * HOUR);
  });

  it('非法窗口返回 0', () => {
    const points = [{ t: T0, v: 1 }];
    expect(coveredSpanMs(points, splitRunsAtGaps(points, []), windowEnd, windowStart)).toBe(0);
  });
});
