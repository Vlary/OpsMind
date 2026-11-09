// M4 五期：LTTB 降采样纯逻辑测试（CONTRACT.md 8.7——点数 >500 抽样绘制）。
import { describe, expect, it } from 'vitest';

import { CHART_MAX_POINTS, downsample, needsDownsample } from '../downsample';

function series(count: number, stepMs = 60_000): { t: number; v: number }[] {
  const base = Date.UTC(2026, 9, 27, 0, 0, 0);
  return Array.from({ length: count }, (_, index) => ({
    t: base + index * stepMs,
    v: index === 500 ? 99 : (index % 50) + 1,
  }));
}

describe('needsDownsample', () => {
  it('点数不超过上限时不抽样', () => {
    expect(needsDownsample(series(500))).toBe(false);
  });

  it('点数超过 500 时需要抽样', () => {
    expect(needsDownsample(series(501))).toBe(true);
  });
});

describe('downsample', () => {
  it('不超上限时原样返回（拷贝）', () => {
    const points = series(100);
    const out = downsample(points);
    expect(out).toHaveLength(100);
    expect(out).toEqual(points);
    expect(out).not.toBe(points);
  });

  it('1440 点（24h@60s）抽样到 ≤500 且保序', () => {
    const points = series(1440);
    const out = downsample(points);
    expect(out.length).toBeLessThanOrEqual(CHART_MAX_POINTS);
    expect(out.length).toBeGreaterThanOrEqual(2);
    let lastT = -Infinity;
    for (const point of out) {
      expect(point.t).toBeGreaterThanOrEqual(lastT);
      lastT = point.t;
    }
  });

  it('首尾点恒保留', () => {
    const points = series(1200);
    const out = downsample(points);
    expect(out[0]).toEqual(points[0]);
    expect(out[out.length - 1]).toEqual(points[points.length - 1]);
  });

  it('LTTB 保留显著峰值（index 500 的 v=99）', () => {
    const points = series(1200);
    const out = downsample(points);
    const maxSampled = Math.max(...out.map((point) => point.v));
    expect(maxSampled).toBe(99);
  });

  it('抽样绝不放大点数（守卫）', () => {
    for (const count of [501, 600, 1000, 1440, 5000]) {
      const out = downsample(series(count));
      expect(out.length).toBeLessThanOrEqual(CHART_MAX_POINTS);
    }
  });

  it('空数组与无效 max 安全返回', () => {
    expect(downsample([])).toEqual([]);
    expect(downsample(series(10), 0)).toEqual([]);
    expect(downsample(series(10), 1)).toHaveLength(1);
  });

  it('恒定值序列可正常抽样', () => {
    const points = Array.from({ length: 800 }, (_, index) => ({ t: index * 1000, v: 42 }));
    const out = downsample(points);
    expect(out.length).toBeLessThanOrEqual(CHART_MAX_POINTS);
    for (const point of out) {
      expect(point.v).toBe(42);
    }
  });
});
