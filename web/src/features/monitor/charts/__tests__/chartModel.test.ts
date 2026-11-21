// 监控看板图表的纯函数单测：缩放、分带、桶化、极值、空输入、乱序输入、
// 单点、null 断线与边界钳制。node 环境直测，不需要 DOM。
import { describe, expect, it } from 'vitest';

import {
  axisLabelPlacement,
  bandLayout,
  bucketRange,
  buildServerOptions,
  clampScore,
  countScale,
  defaultBucketSeconds,
  estimateTextWidth,
  finiteOrNull,
  formatInspectionTime,
  formatSampleAge,
  formatSampleTime,
  hasDuplicateLabels,
  isFutureSample,
  isStale,
  labelTickIndices,
  latestTimestamp,
  MAX_MODEL_POINTS,
  metricChartWindow,
  metricValue,
  nearestBandIndex,
  percentText,
  pickDefaultJobId,
  pickDefaultServerId,
  rankFraction,
  sampleTimeAheadMs,
  sortClusterTrend,
  sortInspectionTrend,
  sortNoisyRules,
  sortTopServers,
  splitLineRuns,
  stackSeverityTimeline,
  staleAgeMs,
  staleThresholdMs,
  toCount,
  trendCoverageNote,
  truncateRuleNameToWidth,
  truncateMiddle,
  truncateMiddleToWidth,
  truncateText,
  type InspectionTrendPoint,
  type NoisyRuleRow,
  type ServerRankRow,
} from '../chartModel';

function server(
  id: string,
  cpu: number | null,
  mem: number | null = 0,
  disk: number | null = 0,
): ServerRankRow {
  return {
    server_id: id,
    server_name: id,
    cpu_pct: cpu,
    mem_pct: mem,
    disk_pct: disk,
    created_at: '2026-11-19T10:00:00Z',
  };
}

describe('数值守卫与格式化', () => {
  it('NaN/Infinity/缺失统一为 null', () => {
    expect(finiteOrNull(Number.NaN)).toBeNull();
    expect(finiteOrNull(Number.POSITIVE_INFINITY)).toBeNull();
    expect(finiteOrNull(undefined)).toBeNull();
    expect(finiteOrNull(0)).toBe(0);
  });

  it('计数清洗：负数归零、非整数四舍五入', () => {
    expect(toCount(-3)).toBe(0);
    expect(toCount(2.6)).toBe(3);
    expect(toCount(Number.NaN)).toBe(0);
  });

  it('百分比文案缺失给破折号', () => {
    expect(percentText(87.34)).toBe('87.3%');
    expect(percentText(null)).toBe('—');
    expect(percentText(Number.NaN)).toBe('—');
  });

  it('百分比比例越界钳制到 0..1', () => {
    expect(rankFraction(0)).toBe(0);
    expect(rankFraction(50)).toBe(0.5);
    expect(rankFraction(100)).toBe(1);
    expect(rankFraction(130)).toBe(1);
    expect(rankFraction(-5)).toBe(0);
    expect(rankFraction(null)).toBe(0);
  });

  it('评分钳制到 0..100', () => {
    expect(clampScore(88)).toBe(88);
    expect(clampScore(-1)).toBe(0);
    expect(clampScore(101)).toBe(100);
    expect(clampScore(null)).toBe(0);
  });
});

describe('sortTopServers（Top12 排行）', () => {
  it('乱序输入按指标降序，同分按名称稳定', () => {
    const rows = [server('b', 40), server('a', 90), server('c', 40)];
    expect(sortTopServers(rows, 'cpu_pct', 12).map((row) => row.server_id)).toEqual([
      'a',
      'b',
      'c',
    ]);
  });

  it('切换指标后排序结果随之变化', () => {
    const rows = [server('cpu-heavy', 99, 10, 10), server('disk-heavy', 10, 10, 99)];
    expect(sortTopServers(rows, 'cpu_pct', 12)[0]?.server_id).toBe('cpu-heavy');
    expect(sortTopServers(rows, 'disk_pct', 12)[0]?.server_id).toBe('disk-heavy');
  });

  it('缺失值排最后，不参与数值比较', () => {
    const rows = [server('missing', null), server('low', 1), server('high', 80)];
    expect(sortTopServers(rows, 'cpu_pct', 12).map((row) => row.server_id)).toEqual([
      'high',
      'low',
      'missing',
    ]);
  });

  it('limit 截断且不改写入参', () => {
    const rows = [server('b', 20), server('a', 10)];
    const top = sortTopServers(rows, 'cpu_pct', 1);
    expect(top).toHaveLength(1);
    expect(top[0]?.server_id).toBe('b');
    expect(rows.map((row) => row.server_id)).toEqual(['b', 'a']);
  });

  it('空输入 / limit<=0 返回空数组', () => {
    expect(sortTopServers([], 'cpu_pct', 12)).toEqual([]);
    expect(sortTopServers([server('a', 1)], 'cpu_pct', 0)).toEqual([]);
  });

  it('超过 MAX_MODEL_POINTS 台时仍能在全量里选出最热机（走查第 2 轮高危）', () => {
    // 前 2000 台按 UUID 升序、CPU 都很低；最热机排在 2000 之后（uuid 序末尾）。
    const rows: ServerRankRow[] = Array.from({ length: MAX_MODEL_POINTS }, (_, index) =>
      server(`00000000-0000-4000-8000-${String(index).padStart(12, '0')}`, 1),
    );
    rows.push(
      server('ffffffff-ffff-4fff-bfff-ffffffffffff', 99.9),
      server('ffffffff-ffff-4fff-bfff-fffffffffffe', 88.8),
    );
    const top = sortTopServers(rows, 'cpu_pct', 12);
    expect(top).toHaveLength(12);
    expect(top[0]?.server_id).toBe('ffffffff-ffff-4fff-bfff-ffffffffffff');
    expect(top[0]?.cpu_pct).toBe(99.9);
    expect(top[1]?.server_id).toBe('ffffffff-ffff-4fff-bfff-fffffffffffe');
    // 结果保持全局降序。
    const values = top.map((row) => row.cpu_pct ?? 0);
    expect(values).toEqual([...values].sort((left, right) => right - left));
  });

  it('metricValue 对缺失字段返回 null', () => {
    expect(metricValue({}, 'cpu_pct')).toBeNull();
    expect(metricValue({ cpu_pct: 3 }, 'cpu_pct')).toBe(3);
  });
});

describe('pickDefaultServerId（单机详情默认机）', () => {
  it('默认选 CPU 最高的机器，空列表返回 null', () => {
    const rows = [server('idle', 0), server('hot', 88), server('warm', 30)];
    expect(pickDefaultServerId(rows)).toBe('hot');
    expect(pickDefaultServerId([])).toBeNull();
  });
});

describe('buildServerOptions（选择器排序）', () => {
  it('按显示名 localeCompare 升序，UUID 兜底不崩', () => {
    const rows: ServerRankRow[] = [
      server('uuid-3', 10),
      { ...server('uuid-1', 10), server_name: 'prod-db-01' },
      { ...server('uuid-2', 10), server_name: 'cache-03' },
      { ...server('00000000-0000-4000-8000-000000000009', 10), server_name: '' },
    ];
    const options = buildServerOptions(rows);
    expect(options.map((option) => option.label)).toEqual([
      '00000000',
      'cache-03',
      'prod-db-01',
      'uuid-3',
    ]);
    expect(options).toHaveLength(rows.length);
    // 不改入参顺序（后端 UUID 序）。
    expect(rows[0]?.server_id).toBe('uuid-3');
  });
});

describe('sortNoisyRules（Top8 最吵规则）', () => {
  const rule = (id: string, count: number, serverCount: number, name = id): NoisyRuleRow => ({
    rule_id: id,
    rule_name: name,
    severity: 'warning',
    count,
    server_count: serverCount,
    last_fired_at: '2026-11-19T10:00:00Z',
  });

  it('按次数降序、再按影响服务器数降序', () => {
    const rules = [rule('a', 5, 1), rule('b', 9, 2), rule('c', 9, 7)];
    expect(sortNoisyRules(rules, 8).map((row) => row.rule_id)).toEqual(['c', 'b', 'a']);
  });

  it('空输入与 limit 截断', () => {
    expect(sortNoisyRules([], 8)).toEqual([]);
    expect(sortNoisyRules([rule('a', 1, 1), rule('b', 2, 1)], 1).map((r) => r.rule_id)).toEqual([
      'b',
    ]);
  });
});

describe('bandLayout（分类轴/横向柱分带）', () => {
  it('单条带居中且不崩', () => {
    const bands = bandLayout(1, 100, 0.2);
    expect(bands).toHaveLength(1);
    const band = bands[0];
    expect(band?.start).toBeCloseTo(10);
    expect(band?.size).toBeCloseTo(80);
    expect(band?.center).toBeCloseTo(50);
  });

  it('多条带等距且带间有间隙', () => {
    const bands = bandLayout(4, 200, 0.25);
    expect(bands).toHaveLength(4);
    expect(bands.map((band) => band.center)).toEqual([25, 75, 125, 175]);
    expect(bands.every((band) => band.size > 0 && band.size < 50)).toBe(true);
  });

  it('空输入/非法范围返回空数组', () => {
    expect(bandLayout(0, 100)).toEqual([]);
    expect(bandLayout(3, 0)).toEqual([]);
    expect(bandLayout(3, Number.NaN)).toEqual([]);
    expect(bandLayout(-2, 100)).toEqual([]);
  });

  it('异常大的 count 被上限截断（防 OOM）', () => {
    const bands = bandLayout(MAX_MODEL_POINTS + 500, 2000);
    expect(bands).toHaveLength(MAX_MODEL_POINTS);
  });
});

describe('nearestBandIndex（全绘图区 nearest-x 命中，第 11 轮 medium）', () => {
  it('390 宽 28 桶：触点 x 落进哪条带就返回哪一桶', () => {
    // 绘图区 316px、28 桶，每桶约 11.3px；取每桶中心验证不会错位到相邻桶。
    const extent = 316;
    const count = 28;
    for (let index = 0; index < count; index++) {
      const center = (index + 0.5) * (extent / count);
      expect(nearestBandIndex(center, extent, count)).toBe(index);
    }
  });

  it('桶边界按最近中心判定（中点左侧归前一桶）', () => {
    expect(nearestBandIndex(0, 100, 4)).toBe(0);
    expect(nearestBandIndex(24.9, 100, 4)).toBe(0);
    expect(nearestBandIndex(25, 100, 4)).toBe(1);
    expect(nearestBandIndex(74.9, 100, 4)).toBe(2);
    expect(nearestBandIndex(75, 100, 4)).toBe(3);
  });

  it('越界钳到首/末桶，非法输入返回 -1', () => {
    expect(nearestBandIndex(-10, 100, 4)).toBe(0);
    expect(nearestBandIndex(120, 100, 4)).toBe(3);
    expect(nearestBandIndex(0, 0, 4)).toBe(-1);
    expect(nearestBandIndex(Number.NaN, 100, 4)).toBe(-1);
    expect(nearestBandIndex(10, 100, 0)).toBe(-1);
  });

  it('count 超上限按 MAX_MODEL_POINTS 截断', () => {
    expect(nearestBandIndex(10, 100, MAX_MODEL_POINTS + 500)).toBe(
      nearestBandIndex(10, 100, MAX_MODEL_POINTS),
    );
  });
});

describe('countScale（整数计数轴）', () => {
  it('空/全 0 回落到 0..4', () => {
    expect(countScale(0)).toEqual({ max: 4, step: 1, ticks: [0, 1, 2, 3, 4] });
    expect(countScale(null)).toEqual({ max: 4, step: 1, ticks: [0, 1, 2, 3, 4] });
    expect(countScale(Number.NaN).max).toBe(4);
  });

  it('小计数给整数刻度', () => {
    expect(countScale(9)).toEqual({ max: 9, step: 3, ticks: [0, 3, 6, 9] });
    expect(countScale(5)).toEqual({ max: 6, step: 2, ticks: [0, 2, 4, 6] });
    expect(countScale(1).ticks).toEqual([0, 1]);
  });

  it('大计数向上取整且刻度为整数', () => {
    const scale = countScale(24);
    expect(scale.max).toBeGreaterThanOrEqual(24);
    expect(scale.ticks.every((tick) => Number.isInteger(tick))).toBe(true);
    expect(scale.ticks[0]).toBe(0);
    expect(scale.ticks[scale.ticks.length - 1]).toBe(scale.max);
  });

  it('极端大值不产生无限刻度', () => {
    const scale = countScale(1e12);
    expect(scale.ticks.length).toBeLessThanOrEqual(101);
    expect(scale.max).toBeGreaterThanOrEqual(1e12);
  });
});

describe('labelTickIndices（x 轴标签密度）', () => {
  it('数量不超过上限时全标', () => {
    expect(labelTickIndices(4, 6)).toEqual([0, 1, 2, 3]);
  });

  it('超上限时首尾必选且升序', () => {
    const indices = labelTickIndices(24, 6);
    expect(indices[0]).toBe(0);
    expect(indices[indices.length - 1]).toBe(23);
    expect(indices).toEqual([...indices].sort((a, b) => a - b));
    expect(new Set(indices).size).toBe(indices.length);
  });

  it('空输入/非法上限返回空数组', () => {
    expect(labelTickIndices(0, 6)).toEqual([]);
    expect(labelTickIndices(10, 0)).toEqual([]);
    expect(labelTickIndices(2, 1)).toEqual([0]);
  });

  it('超上限时标签均匀铺满，末标签不再紧贴前一个（7 天视图重叠回归）', () => {
    // 28 个 6h 桶、6 个标签：旧实现 0,6,12,18,24,27（末段仅 3 桶，
    // "11/19 08:00" 与 "11/20 02:00" 重叠 16px）；均匀分布后间距 5–6 桶。
    expect(labelTickIndices(28, 6)).toEqual([0, 5, 11, 16, 22, 27]);
    const indices = labelTickIndices(28, 6);
    for (let index = 1; index < indices.length; index++) {
      const gap = (indices[index] ?? 0) - (indices[index - 1] ?? 0);
      expect(gap).toBeGreaterThanOrEqual(5);
      expect(gap).toBeLessThanOrEqual(6);
    }
  });
});

describe('hasDuplicateLabels（巡检日标签重复检测）', () => {
  it('同一天两份报告 → 重复', () => {
    expect(hasDuplicateLabels(['11/19', '11/19', '11/20'])).toBe(true);
  });

  it('跨天日期互不相同 → 不重复', () => {
    expect(hasDuplicateLabels(['11/18', '11/19', '11/20'])).toBe(false);
  });

  it('空数组/单元素不重复；扫描有上限', () => {
    expect(hasDuplicateLabels([])).toBe(false);
    expect(hasDuplicateLabels(['11/19'])).toBe(false);
    expect(hasDuplicateLabels(['a', 'a'], 1)).toBe(false);
  });
});

describe('axisLabelPlacement（巡检首末标签居中/贴边）', () => {
  it('标签放得下时跟随分带中心（5 份报告不再差半个分带）', () => {
    // 1440 卡：首点中心 147.6，标签 "11/19" 约 30px。
    expect(axisLabelPlacement(147.6, 30, 38, 1134, 'first')).toEqual({
      x: 147.6,
      anchor: 'middle',
    });
    expect(axisLabelPlacement(1024.4, 30, 38, 1134, 'last')).toEqual({
      x: 1024.4,
      anchor: 'middle',
    });
  });

  it('标签放不下时首贴左缘 start、末贴右缘 end', () => {
    expect(axisLabelPlacement(56, 66, 38, 548, 'first')).toEqual({ x: 38, anchor: 'start' });
    expect(axisLabelPlacement(529, 66, 38, 548, 'last')).toEqual({ x: 548, anchor: 'end' });
  });

  it('中间标签恒居中；非法宽度/坐标有确定行为', () => {
    expect(axisLabelPlacement(300, 40, 38, 548, 'middle')).toEqual({ x: 300, anchor: 'middle' });
    expect(axisLabelPlacement(Number.NaN, 40, 38, 548, 'middle')).toEqual({
      x: 38,
      anchor: 'middle',
    });
    expect(axisLabelPlacement(56, Number.NaN, 38, 548, 'first')).toEqual({
      x: 56,
      anchor: 'middle',
    });
  });
});

describe('sortClusterTrend（趋势桶顺序与去重）', () => {
  it('乱序输入按 bucket_start 升序', () => {
    const points = [
      { bucket_start: '2026-11-19T03:00:00Z', cpu_p95: 3 },
      { bucket_start: '2026-11-19T01:00:00Z', cpu_p95: 1 },
      { bucket_start: '2026-11-19T02:00:00Z', cpu_p95: 2 },
    ];
    expect(sortClusterTrend(points).map((point) => point.cpu_p95)).toEqual([1, 2, 3]);
  });

  it('重复时刻保留首条、非法时间丢弃、空输入不崩', () => {
    const sorted = sortClusterTrend([
      { bucket_start: 'bad', cpu_p95: 9 },
      { bucket_start: '2026-11-19T01:00:00Z', cpu_p95: 1 },
      { bucket_start: '2026-11-19T01:00:00Z', cpu_p95: 2 },
    ]);
    expect(sorted).toHaveLength(1);
    expect(sorted[0]?.cpu_p95).toBe(1);
    expect(sortClusterTrend([])).toEqual([]);
  });
});

describe('splitLineRuns（null 断线）', () => {
  it('全空返回空段', () => {
    expect(splitLineRuns([null, null])).toEqual([]);
    expect(splitLineRuns([])).toEqual([]);
  });

  it('单点保留为一段（单点态不丢）', () => {
    expect(splitLineRuns([5])).toEqual([[0]]);
    expect(splitLineRuns([null, 5, null])).toEqual([[1]]);
  });

  it('按 null/NaN 断开且索引正确', () => {
    expect(splitLineRuns([1, 2, null, 3, Number.NaN, 4, 5])).toEqual([[0, 1], [3], [5, 6]]);
  });

  it('超长输入只扫描上限内点数', () => {
    const values = Array.from({ length: MAX_MODEL_POINTS + 100 }, () => 1);
    const runs = splitLineRuns(values);
    expect(runs).toHaveLength(1);
    expect(runs[0]).toHaveLength(MAX_MODEL_POINTS);
  });
});

describe('stackSeverityTimeline（告警分桶堆叠）', () => {
  it('按时间升序、统计各等级与总数', () => {
    const buckets = [
      { bucket_start: '2026-11-19T02:00:00Z', info: 1, warning: 2, critical: 3 },
      { bucket_start: '2026-11-19T01:00:00Z', info: 4, warning: 0, critical: 0 },
    ];
    const stacked = stackSeverityTimeline(buckets);
    expect(stacked.map((bucket) => bucket.bucket_start)).toEqual([
      '2026-11-19T01:00:00Z',
      '2026-11-19T02:00:00Z',
    ]);
    expect(stacked[1]?.total).toBe(6);
    expect(stacked[1]?.levels).toEqual([
      { key: 'critical', value: 3 },
      { key: 'warning', value: 2 },
      { key: 'info', value: 1 },
    ]);
  });

  it('同一时刻重复桶合并求和，保留首个原始时间串', () => {
    const stacked = stackSeverityTimeline([
      { bucket_start: '2026-11-19T01:00:00Z', info: 1, warning: 1, critical: 0 },
      { bucket_start: '2026-11-19T01:00:00Z', info: 2, warning: 0, critical: 1 },
    ]);
    expect(stacked).toHaveLength(1);
    expect(stacked[0]?.bucket_start).toBe('2026-11-19T01:00:00Z');
    expect(stacked[0]?.total).toBe(5);
  });

  it('负数归零、非法时间丢弃、空输入返回空数组', () => {
    const stacked = stackSeverityTimeline([
      { bucket_start: 'not-a-date', info: 9, warning: 9, critical: 9 },
      { bucket_start: '2026-11-19T01:00:00Z', info: -3, warning: 2, critical: 0 },
    ]);
    expect(stacked).toHaveLength(1);
    expect(stacked[0]?.levels).toEqual([
      { key: 'critical', value: 0 },
      { key: 'warning', value: 2 },
      { key: 'info', value: 0 },
    ]);
    expect(stackSeverityTimeline([])).toEqual([]);
  });
});

describe('bucketRange（点击桶 → fired_after/before）', () => {
  it('1 小时桶生成左闭右开 RFC3339 区间', () => {
    expect(bucketRange('2026-11-19T13:00:00Z', 3600)).toEqual({
      after: '2026-11-19T13:00:00.000Z',
      before: '2026-11-19T14:00:00.000Z',
    });
  });

  it('6 小时桶（7d 档）', () => {
    expect(bucketRange('2026-11-19T12:00:00Z', 21_600)?.before).toBe('2026-11-19T18:00:00.000Z');
  });

  it('非法时间/桶宽返回 null（不触发筛选）', () => {
    expect(bucketRange('bad', 3600)).toBeNull();
    expect(bucketRange('2026-11-19T13:00:00Z', 0)).toBeNull();
    expect(bucketRange('2026-11-19T13:00:00Z', null)).toBeNull();
    expect(bucketRange('2026-11-19T13:00:00Z', Number.NaN)).toBeNull();
  });

  it('窗口缺省桶宽与后端写死值一致', () => {
    expect(defaultBucketSeconds('24h')).toBe(3600);
    expect(defaultBucketSeconds('7d')).toBe(21_600);
  });
});

describe('sortInspectionTrend（巡检走势）', () => {
  function point(id: string, createdAt: string, score = 80): InspectionTrendPoint {
    return {
      report_id: id,
      created_at: createdAt,
      avg_score: score,
      server_count: 3,
      anomaly_count: 1,
      trigger: 'cron',
    };
  }

  it('乱序输入按 created_at 升序且不改写入参', () => {
    const points = [
      point('c', '2026-11-19T03:00:00Z'),
      point('a', '2026-11-19T01:00:00Z'),
      point('b', '2026-11-19T02:00:00Z'),
    ];
    expect(sortInspectionTrend(points).map((entry) => entry.report_id)).toEqual(['a', 'b', 'c']);
    expect(points.map((entry) => entry.report_id)).toEqual(['c', 'a', 'b']);
  });

  it('非法时间排最后；单点/空输入不崩', () => {
    const sorted = sortInspectionTrend([
      point('bad', 'not-a-date'),
      point('good', '2026-11-19T01:00:00Z'),
    ]);
    expect(sorted.map((entry) => entry.report_id)).toEqual(['good', 'bad']);
    expect(sortInspectionTrend([])).toEqual([]);
    expect(sortInspectionTrend([point('only', '2026-11-19T01:00:00Z')])).toHaveLength(1);
  });
});

describe('pickDefaultJobId（巡检任务默认项）', () => {
  const jobs = [
    { id: 'job-1', name: '基础巡检' },
    { id: 'job-2', name: '数据库巡检' },
  ];

  it('优先最近一份报告所属任务', () => {
    expect(pickDefaultJobId(jobs, 'job-2')).toBe('job-2');
  });

  it('最新任务不在列表时回落到唯一任务或 null', () => {
    expect(pickDefaultJobId(jobs, 'job-x')).toBeNull();
    expect(pickDefaultJobId([jobs[0] as { id: string; name: string }], 'job-x')).toBe('job-1');
    expect(pickDefaultJobId([], null)).toBeNull();
  });
});

describe('时效与文案', () => {
  it('过期判断以 5 分钟为默认阈值，边界不算过期', () => {
    const now = Date.parse('2026-11-19T10:05:00Z');
    expect(isStale('2026-11-19T10:00:00Z', now)).toBe(false);
    expect(isStale('2026-11-19T09:59:00Z', now)).toBe(true);
    expect(isStale('bad', now)).toBe(false);
  });

  it('过期阈值随采集间隔放宽：10 分钟间隔 → 20 分钟，缺省仍 5 分钟', () => {
    expect(staleThresholdMs()).toBe(300_000);
    expect(staleThresholdMs(null)).toBe(300_000);
    expect(staleThresholdMs(60)).toBe(300_000);
    expect(staleThresholdMs(600)).toBe(1_200_000);
    expect(staleThresholdMs(3600)).toBe(7_200_000);
    // 6 分钟前的采样在 10 分钟间隔下不算过期（走查场景）。
    const now = Date.parse('2026-11-19T10:06:00Z');
    expect(isStale('2026-11-19T10:00:00Z', now, staleThresholdMs(600))).toBe(false);
  });

  it('采样时刻与年龄文案可读且非法时间不崩', () => {
    const formatted = formatSampleTime('2026-11-19T10:00:00Z');
    expect(formatted).toMatch(/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}/);
    expect(formatSampleTime('bad')).toBe('时间未知');
    expect(formatSampleAge(34 * 60_000)).toBe('34 分钟');
    expect(formatSampleAge(2 * 3_600_000)).toBe('2 小时');
    expect(formatSampleAge(5_000)).toBe('5 秒');
  });

  it('巡检报告时间只到分钟，非法时间给「时间未知」（第 11 轮 low）', () => {
    // 概要卡第二行用；格式与采样时刻一致到分钟，窄屏不换行。
    expect(formatInspectionTime('2026-11-15T06:48:00Z')).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    expect(formatInspectionTime('bad')).toBe('时间未知');
    expect(formatInspectionTime(null)).toBe('时间未知');
  });

  it('过期时长对未来时间归零', () => {
    const now = Date.parse('2026-11-19T10:00:00Z');
    expect(staleAgeMs('2026-11-19T10:01:00Z', now)).toBe(0);
    expect(staleAgeMs(null, now)).toBeNull();
  });

  it('未来采样：超前量可读，超过 1 秒容差即判时钟异常（第 17 轮）', () => {
    const now = Date.parse('2026-11-19T10:00:00Z');
    expect(sampleTimeAheadMs('2026-11-19T10:20:00Z', now)).toBe(20 * 60_000);
    expect(sampleTimeAheadMs('2026-11-19T09:59:00Z', now)).toBeNull();
    expect(sampleTimeAheadMs('bad', now)).toBeNull();
    // 1 秒内的请求往返/取整抖动不算异常。
    expect(isFutureSample('2026-11-19T10:00:00Z', now)).toBe(false);
    expect(isFutureSample('2026-11-19T10:00:00.500Z', now)).toBe(false);
    expect(isFutureSample('2026-11-19T10:00:02Z', now)).toBe(true);
    // 走查第 17 轮 medium：600s 采集间隔下 +9 分钟必须判异常，不能再拿
    // 整个采集间隔当容差（否则 Top12 当前值与趋势 SQL 静默不一致）。
    expect(isFutureSample('2026-11-19T10:09:00Z', now)).toBe(true);
    expect(isFutureSample('2026-11-19T10:20:00Z', now, null)).toBe(true);
    // 显式传入的容差仍生效（调用方需要更宽松口径时）。
    expect(isFutureSample('2026-11-19T10:05:00Z', now, 600_000)).toBe(false);
    expect(isFutureSample('2026-11-19T10:20:00Z', now, 600_000)).toBe(true);
    expect(isFutureSample('2026-11-19T10:20:00Z', Number.NaN)).toBe(false);
  });

  it('最晚采样时间忽略非法项', () => {
    expect(
      latestTimestamp([
        { created_at: '2026-11-19T09:00:00Z' },
        { created_at: 'bad' },
        { created_at: '2026-11-19T10:00:00Z' },
      ]),
    ).toBe(Date.parse('2026-11-19T10:00:00Z'));
    expect(latestTimestamp([])).toBeNull();
  });

  it('最晚采样时间扫描全量行，不被前 2000 行截断', () => {
    const rows = Array.from({ length: MAX_MODEL_POINTS }, () => ({
      created_at: '2026-11-19T09:00:00Z',
    }));
    rows.push({ created_at: '2026-11-19T11:00:00Z' });
    expect(latestTimestamp(rows)).toBe(Date.parse('2026-11-19T11:00:00Z'));
  });

  it('名称按码点截断，单点空串不崩', () => {
    expect(truncateText('短名', 4)).toBe('短名');
    expect(truncateText('数据库主节点-北京-01', 6)).toBe('数据库主节点…');
    expect(truncateText('abc', 0)).toBe('');
    expect(truncateText('😀😀😀', 2)).toBe('😀😀…');
  });
});

describe('metricChartWindow（采集暂停冻结时间窗，第 13 轮）', () => {
  const now = Date.parse('2026-11-20T14:35:00Z');
  const lastSample = Date.parse('2026-11-20T12:34:27Z');

  it('采集运行中：窗口终点跟随真实 now', () => {
    const win = metricChartWindow(60, now, lastSample, false);
    expect(win.end).toBe(now);
    expect(win.end - win.start).toBe(3_600_000);
  });

  it('采集运行中但最新点略晚于挂载时刻：终点取较晚者，不裁掉最新点', () => {
    // 首次 metrics 请求在挂载之后完成，响应末点常比 nowMs 晚几百毫秒；
    // 严格以 nowMs 为终点会把它滤掉（1440 → 1439）。
    const win = metricChartWindow(1440, now, now + 800, false);
    expect(win.end).toBe(now + 800);
    expect(win.end - win.start).toBe(1440 * 60_000);
  });

  it('采集暂停：窗口终点冻结在最后一个采样点，跨度不变', () => {
    const win = metricChartWindow(60, now, lastSample, true);
    expect(win.end).toBe(lastSample);
    expect(win.start).toBe(lastSample - 3_600_000);
    // 真实 now 仍用于「最新采样 N 分钟前」，不参与窗口终点。
    expect(win.end).toBeLessThan(now);
  });

  it('暂停但尚无采样：回落 now（图显示空态，不出现倒置窗口）', () => {
    expect(metricChartWindow(60, now, null, true)).toEqual({
      start: now - 3_600_000,
      end: now,
    });
    expect(metricChartWindow(60, now, Number.NaN, true).end).toBe(now);
  });

  it('非法/非正 minutes 按 1 分钟，非法 now 不产生 NaN 窗口', () => {
    expect(metricChartWindow(0, now, null, false).end).toBe(now);
    expect(metricChartWindow(0, now, null, false).start).toBe(now - 60_000);
    const invalid = metricChartWindow(60, Number.NaN, null, false);
    expect(Number.isFinite(invalid.start)).toBe(true);
    expect(invalid.end).toBe(0);
  });
});

describe('truncateMiddle（窄屏服务器名）', () => {
  it('长度在上限内原样保留：batch-04 / batch-05 不再撞名', () => {
    expect(truncateMiddle('batch-04', 9)).toBe('batch-04');
    expect(truncateMiddle('batch-05', 9)).toBe('batch-05');
  });

  it('超长名称保留尾部编号（区分信息在尾部）', () => {
    expect(truncateMiddle('prod-web-batch-04', 9)).toBe('prod…h-04');
    expect(truncateMiddle('prod-web-batch-05', 9)).toBe('prod…h-05');
  });

  it('总长不超过上限（含省略号），小上限退化前缀截断', () => {
    expect(truncateMiddle('abcdefghij', 5)).toBe('ab…ij');
    expect(truncateMiddle('abcde', 3)).toBe('ab…');
    expect(truncateMiddle('abc', 0)).toBe('');
    expect(truncateMiddle('😀😀😀😀', 3)).toBe('😀😀…');
  });
});

describe('truncateMiddleToWidth（按像素宽度截断轴标签）', () => {
  it('CJK 按双宽计算：1440 左列 92px 保留 8 个汉字与区域后缀', () => {
    const label = truncateMiddleToWidth('生产数据库主节点-华东一区', 92);
    expect(estimateTextWidth(label)).toBeLessThanOrEqual(92);
    expect(label.startsWith('生产')).toBe(true);
    expect(label.endsWith('华东一区')).toBe(true);
    // 15 个半角单位：生产数 + '-' + 省略号 + 华东一区。
    expect(Array.from(label).length).toBeLessThanOrEqual(9);
  });

  it('390 左列 76px（padLeft 88 留 12px）保留约 6 个汉字，左缘不越界', () => {
    const label = truncateMiddleToWidth('生产数据库主节点-华东一区', 76);
    expect(estimateTextWidth(label)).toBeLessThanOrEqual(76);
    // 尾部整段优先：生…华东一区（11 个半角单位 = 66px）。
    expect(label).toBe('生…华东一区');
  });

  it('ASCII 名称沿用中段截断：64px 下 prod-cache-01 / prod-idle-01 仍可区分', () => {
    // 保守按 6px/字符：64px → 10 个单位。
    expect(truncateMiddleToWidth('prod-cache-01', 64)).toBe('prod…he-01');
    expect(truncateMiddleToWidth('prod-idle-01', 64)).toBe('prod…le-01');
    expect(estimateTextWidth('prod…he-01')).toBeLessThanOrEqual(64);
  });

  it('11px 字号的 ASCII 按 6px/字符估算，不把 66px 的 web-prod-01 当 60.5px', () => {
    // 走查第 12 轮 low：旧 5.5px/字符让 64px 预算放行 66.05px 的标签，
    // 左缘落到 1.95px 与过期圆点重叠。这里 64px 只能放 10 个字符。
    expect(truncateMiddleToWidth('web-prod-01', 64)).toBe('web-…od-01');
    expect(estimateTextWidth('web-prod-01')).toBeCloseTo(66, 5);
    expect(truncateMiddleToWidth('web-prod-01', 76)).toBe('web-prod-01');
  });

  it('78px 窄列的系统规则名保留「系统）」后缀且不越界', () => {
    // 最吵规则 390 左列可用约 78px（padLeft 88 - 8 间距 - 2 边距）。
    const label = truncateMiddleToWidth('采集不可达（系统）', 78);
    expect(estimateTextWidth(label)).toBeLessThanOrEqual(78);
    expect(label.endsWith('系统）')).toBe(true);
  });

  it('宽度足够原样返回；0/极窄宽度不崩', () => {
    expect(truncateMiddleToWidth('短名', 94)).toBe('短名');
    expect(truncateMiddleToWidth('abc', 0)).toBe('');
    expect(truncateMiddleToWidth('abcdef', 5)).toBe('');
    // 10px 只放得下省略号；22px = 3 个单位（6px/单位）。
    expect(truncateMiddleToWidth('abcdef', 10)).toBe('…');
    expect(truncateMiddleToWidth('abcdef', 22)).toBe('a…f');
  });
});

// 走查第 17 轮 low：规则名差异在前缀（网络入/网络出），必须保留前缀；
// 系统哨兵的「（系统）」后缀也不能丢（第 6 轮可辨认性要求）。
describe('truncateRuleNameToWidth（前缀保留截断）', () => {
  it('76px 下网络入/出流量异常截断后仍可区分，且不越界', () => {
    // 1024/390 的最吵规则左列可用 76px（padLeft 88 - 12）；中段截断会把
    // 两行都变成「网络…量异常」。
    const inbound = truncateRuleNameToWidth('网络入流量异常', 76);
    const outbound = truncateRuleNameToWidth('网络出流量异常', 76);
    expect(inbound).not.toBe(outbound);
    expect(inbound.startsWith('网络入')).toBe(true);
    expect(outbound.startsWith('网络出')).toBe(true);
    expect(inbound).toContain('…');
    expect(outbound).toContain('…');
    expect(estimateTextWidth(inbound)).toBeLessThanOrEqual(76);
    expect(estimateTextWidth(outbound)).toBeLessThanOrEqual(76);
  });

  it('76px 下系统哨兵仍保留「系统）」后缀', () => {
    // 第 6 轮 E2E 断言同一列的系统规则标签必须含「系统）」。
    const label = truncateRuleNameToWidth('采集不可达（系统）', 76);
    expect(label.startsWith('采集')).toBe(true);
    expect(label.endsWith('系统）')).toBe(true);
    expect(estimateTextWidth(label)).toBeLessThanOrEqual(76);
  });

  it('宽度足够原样返回；0/极窄宽度不崩', () => {
    expect(truncateRuleNameToWidth('网络入流量异常', 94)).toBe('网络入流量异常');
    expect(truncateRuleNameToWidth('abc', 0)).toBe('');
    expect(truncateRuleNameToWidth('abcdef', 10)).toBe('…');
    // 22px = 3 个单位（6px/单位）：保留 2 个单位 + 省略号。
    expect(truncateRuleNameToWidth('abcdef', 22)).toBe('ab…');
  });
});

describe('trendCoverageNote（部分空桶覆盖提示）', () => {
  it('实际首末采样跨度优先于桶跨度（7 桶/1.53 天，不是 1.8 天）', () => {
    const nonEmpty = Array.from({ length: 28 }, (_, index) => index >= 21);
    // 7 个 6 小时桶的桶跨度 = 1.75 天（会被四舍五入成 1.8 天）；实际样本
    // 首末相距 1 天 12:40:20（1.53 天）。
    const note = trendCoverageNote(nonEmpty, 6 * 3600, (36 * 60 + 40) * 60_000 + 20_000);
    expect(note).toContain('有样本 7/28 桶');
    expect(note).toContain('数据跨度 1.5 天');
    expect(note).not.toContain('1.8 天');
    expect(note).not.toContain('覆盖桶跨度');
    expect(note).toContain('不能当作无压力');
  });

  it('缺少实际首末采样时退回桶跨度，并写明首末桶未满', () => {
    const nonEmpty = Array.from({ length: 28 }, (_, index) => index >= 22);
    const note = trendCoverageNote(nonEmpty, 6 * 3600);
    expect(note).toContain('有样本 6/28 桶');
    expect(note).toContain('覆盖桶跨度约 1.5 天（首末桶未满）');
    expect(note).not.toContain('数据跨度');
    expect(note).toContain('不能当作无压力');
  });

  it('全满/全空/空输入都不提示（全空走空态）', () => {
    expect(trendCoverageNote(new Array(28).fill(true), 3600)).toBeNull();
    expect(trendCoverageNote(new Array(28).fill(false), 3600)).toBeNull();
    expect(trendCoverageNote([], 3600)).toBeNull();
  });

  it('非连续样本按首末下标给桶跨度，不虚报每桶都有数据', () => {
    const nonEmpty = Array.from({ length: 10 }, (_, index) => index % 3 === 0);
    const note = trendCoverageNote(nonEmpty, 3600);
    // 4 个样本桶（0/3/6/9），桶跨度 10 小时。
    expect(note).toContain('有样本 4/10 桶');
    expect(note).toContain('覆盖桶跨度约 10 小时（首末桶未满）');
  });

  it('bucketSeconds 非法且无实际跨度时不崩且省略跨度', () => {
    const nonEmpty = [true, false, false];
    const note = trendCoverageNote(nonEmpty, Number.NaN);
    expect(note).toContain('有样本 1/3 桶');
    expect(note).not.toContain('数据跨度');
    expect(note).not.toContain('覆盖桶跨度');
  });

  it('只有一个样本时实际跨度为 0，仍如实写「不足 1 分钟」', () => {
    const nonEmpty = Array.from({ length: 28 }, (_, index) => index === 27);
    const note = trendCoverageNote(nonEmpty, 6 * 3600, 0);
    expect(note).toContain('有样本 1/28 桶');
    expect(note).toContain('数据跨度');
    expect(note).toContain('不足 1 分钟');
  });
});
