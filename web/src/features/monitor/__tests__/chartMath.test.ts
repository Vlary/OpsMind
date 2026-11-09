// M4 五期：图表坐标数学纯逻辑测试（y 轴自动量纲 / 映射 / bps 格式化）。
import { describe, expect, it } from 'vitest';

import { buildPolyline, formatBps, mapX, mapY, tickValues, yScale } from '../chartMath';

describe('yScale', () => {
  it('数据全 0 / 空时给 0..100 缺省量纲', () => {
    expect(yScale(0)).toEqual({ max: 100, step: 25, dataMax: 0 });
  });

  it('百分比量程向上取 nice number 并留余量', () => {
    const scale = yScale(88);
    expect(scale.max).toBeGreaterThanOrEqual(88);
    expect(scale.max).toBe(100);
    expect(scale.step).toBe(scale.max / 4);
  });

  it('负值/NaN 视为缺省量纲（防御）', () => {
    expect(yScale(Number.NaN).max).toBe(100);
  });

  it('bps 大值走 1/2/5×10^n 对齐', () => {
    // 9.1M*1.1 ≈ 10.01M → niceCeil → 20M。
    const scale = yScale(9_100_000);
    expect(scale.max).toBeGreaterThanOrEqual(9_100_000);
    expect([10_000_000, 20_000_000]).toContain(scale.max);
  });
});

describe('tickValues', () => {
  it('0..max 共 5 档', () => {
    const values = tickValues(yScale(80));
    expect(values).toHaveLength(5);
    expect(values[0]).toBe(0);
    expect(values[4]).toBe(100);
  });
});

describe('mapX/mapY', () => {
  it('x 线性映射且退化区间钉左缘', () => {
    expect(mapX(5, 0, 10, 100)).toBeCloseTo(50);
    expect(mapX(5, 5, 5, 100)).toBe(0);
  });

  it('y 反转（SVG y 向下）且 yMax=0 钉底', () => {
    expect(mapY(0, 100, 160)).toBe(160);
    expect(mapY(100, 100, 160)).toBe(0);
    expect(mapY(50, 100, 160)).toBe(80);
    expect(mapY(10, 0, 160)).toBe(160);
  });
});

describe('formatBps', () => {
  it('人类可读分级', () => {
    expect(formatBps(45)).toBe('45 B/s');
    expect(formatBps(890)).toBe('890 B/s');
    expect(formatBps(1_499)).toBe('1.5 KB/s');
    expect(formatBps(1500)).toBe('1.5 KB/s');
    expect(formatBps(2_500_000)).toBe('2.50 MB/s');
    expect(formatBps(1_230_000_000)).toBe('1.23 GB/s');
  });
});

describe('buildPolyline', () => {
  it('坐标串格式化且空数据为空串', () => {
    expect(buildPolyline([])).toBe('');
    expect(
      buildPolyline([
        { x: 0, y: 160.01 },
        { x: 44, y: 8 },
      ]),
    ).toBe('0.0,160.0 44.0,8.0');
  });
});
