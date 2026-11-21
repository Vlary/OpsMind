// M4 五期：图表坐标数学纯逻辑测试（y 轴自动量纲 / 映射 / bps 格式化）。
import { describe, expect, it } from 'vitest';

import {
  axisPaddingLeft,
  buildPolyline,
  estimateAxisLabelWidth,
  formatAxisValue,
  formatBps,
  formatChartTime,
  formatCoverageDuration,
  formatCoverageDurationPrecise,
  formatNumericAxisTick,
  formatSignificant,
  LONG_TIME_SPAN_MS,
  mapX,
  mapY,
  nearestIndexByX,
  percentYScale,
  tickValues,
  windowTickTimes,
  yScale,
} from '../chartMath';

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

  it('百分比序列以 100 为硬上限：95% 满载不再扩到 200%', () => {
    // 走查回归：此前 95 → padded 104.5 → niceCeil 200，P95 被压到图高一半。
    expect(percentYScale(95)).toEqual({ max: 100, step: 25, dataMax: 95 });
    // 真实场景 99.25（psql 当前小时桶）也必须停在 100。
    expect(percentYScale(99.25)).toEqual({ max: 100, step: 25, dataMax: 99.25 });
    expect(percentYScale(100).max).toBe(100);
    expect(yScale(95, 100).max).toBe(100);
  });

  it('百分比数据真的越过 100 才允许扩张', () => {
    expect(percentYScale(130).max).toBeGreaterThan(100);
    // 自动量纲路径不受 cap 影响（parser 复用的 yScale(0) 仍指向 100）。
    expect(yScale(130).max).toBeGreaterThanOrEqual(130);
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

describe('nearestIndexByX（hover 与触屏点按共用的最近点判定）', () => {
  const coords = [{ x: 0 }, { x: 10 }, { x: 20 }, { x: 30 }];

  it('返回 x 距离最近的点', () => {
    expect(nearestIndexByX(coords, 0)).toBe(0);
    expect(nearestIndexByX(coords, 9)).toBe(1);
    expect(nearestIndexByX(coords, 21)).toBe(2);
    expect(nearestIndexByX(coords, 999)).toBe(3);
  });

  it('相邻等距时取先出现的点（与旧线性扫描一致）', () => {
    expect(nearestIndexByX(coords, 15)).toBe(1);
  });

  it('空数组返回 -1（调用方据此不打开 tooltip）', () => {
    expect(nearestIndexByX([], 12)).toBe(-1);
  });
});

describe('formatBps', () => {
  it('人类可读分级（B/KB/MB/GB）', () => {
    expect(formatBps(45)).toBe('45 B/s');
    expect(formatBps(890)).toBe('890 B/s');
    expect(formatBps(1_499)).toBe('1.5 KB/s');
    expect(formatBps(1500)).toBe('1.5 KB/s');
    expect(formatBps(2_500_000)).toBe('2.50 MB/s');
    expect(formatBps(1_230_000_000)).toBe('1.23 GB/s');
  });

  it('补齐 TB/PB/EB 三档，9.22e18 B/s 不再写成十位数 GB/s', () => {
    // 走查第 7 轮 low：extreme-01 的 9.22e18 B/s 曾被格式化成
    // 「9223372036.85 GB/s」，轴刻度更是丢 B 的「5000000000G/s」。
    expect(formatBps(1_500_000_000_000)).toBe('1.50 TB/s');
    expect(formatBps(1_500_000_000_000_000)).toBe('1.50 PB/s');
    expect(formatBps(2 ** 63 - 1)).toBe('9.22 EB/s');
  });

  it('超过 EB 的异常采样用 3 位有效数字科学计数，且保留 B/s', () => {
    expect(formatBps(3.4028235e38)).toBe('3.40e38 B/s');
    expect(formatBps(Number.POSITIVE_INFINITY)).toBe('—');
    expect(formatBps(Number.NaN)).toBe('—');
  });

  it('四舍五入到 1000 时提升单位，不出现 "1000.00 GB/s"（第 11 轮 low）', () => {
    // 同屏「网络入 1.00 TB/s」而「网络出 1000.00 GB/s」会被读成相差
    // 1000 倍；阈值 999.995 起提升（toFixed(2) 后 >= 1000）。
    expect(formatBps(999_999_999_999)).toBe('1.00 TB/s');
    expect(formatBps(999_995_000_000)).toBe('1.00 TB/s');
    expect(formatBps(999_994_000_000)).toBe('999.99 GB/s');
    expect(formatBps(999_999_999)).toBe('1.00 GB/s');
    expect(formatBps(999_999)).toBe('1.00 MB/s');
    // KB 只有 1 位小数：999.95 KB → 1000.0 → 提升到 1.00 MB。
    expect(formatBps(999_950)).toBe('1.00 MB/s');
    expect(formatBps(-999_999_999_999)).toBe('-1.00 TB/s');
    // 最大档 EB 找不到更大单位时仍回退科学计数。
    expect(formatBps(1e21)).toBe('1.00e21 B/s');
  });
});

describe('formatSignificant（3 位有效数字）', () => {
  it('常规负载保留短小数，极端浮点不再展开 17 位', () => {
    expect(formatSignificant(3.4)).toBe('3.4');
    expect(formatSignificant(0.42)).toBe('0.42');
    expect(formatSignificant(123.456)).toBe('123');
    expect(formatSignificant(1_234_567)).toBe('1.23e6');
    expect(formatSignificant(3.4028235e38)).toBe('3.40e38');
    expect(formatSignificant(Number.NaN)).toBe('—');
  });
});

describe('formatNumericAxisTick（按轴 max 统一记法）', () => {
  it('max≥1e6 时所有非零刻度统一科学计数，0 仍为 0', () => {
    // 走查第 9 轮 low：旧轴 0 | 500000 | 1.00e6 | 1.50e6 | 2.00e6。
    const ticks = [0, 500_000, 1_000_000, 1_500_000, 2_000_000].map((value) =>
      formatNumericAxisTick(value, 2_000_000),
    );
    expect(ticks).toEqual(['0', '5.00e5', '1.00e6', '1.50e6', '2.00e6']);
    // 同一轴的刻度不得混排普通数字与科学计数。
    expect(ticks.filter((label) => label !== '0' && !label.includes('e'))).toEqual([]);
  });

  it('小量程保持普通有效数字', () => {
    expect(formatNumericAxisTick(0, 4)).toBe('0');
    expect(formatNumericAxisTick(0.5, 4)).toBe('0.5');
    expect(formatNumericAxisTick(2, 4)).toBe('2');
    expect(formatNumericAxisTick(750_000, 800_000)).toBe('750000');
  });

  it('非法值返回破折号', () => {
    expect(formatNumericAxisTick(Number.NaN, 2_000_000)).toBe('—');
    expect(formatNumericAxisTick(1, Number.NaN)).toBe('1');
  });
});

describe('formatAxisValue（y 轴短标签）', () => {
  it('轴与卡头共用带 B 的完整单位，不再压成 12.5M/s', () => {
    // 走查第 7 轮 low：同一张卡轴是 12.5M/s、0 刻度 0 B/s、卡头
    // 20.00 MB/s。现在轴标签直接复用 formatBps（保留 B 与 /s），左留白
    // 由 axisPaddingLeft 动态给到 96px 上限。
    expect(formatAxisValue(5_000_000_000, formatBps)).toBe('5.00 GB/s');
    expect(formatAxisValue(20_000_000_000, formatBps)).toBe('20.00 GB/s');
    expect(formatAxisValue(1_300_000_000, formatBps)).toBe('1.30 GB/s');
    expect(formatAxisValue(2_500_000, formatBps)).toBe('2.50 MB/s');
    expect(formatAxisValue(999_000, formatBps)).toBe('999.0 KB/s');
    expect(formatAxisValue(12_500_000, formatBps)).toBe('12.50 MB/s');
    for (const value of [5_000_000_000, 20_000_000_000, 1_300_000_000, 2_500_000, 999_000]) {
      expect(formatAxisValue(value, formatBps).length).toBeLessThanOrEqual(12);
    }
  });

  it('极端值轴标签走 EB/科学计数，不出现 G/s 长整数', () => {
    expect(formatAxisValue(2 ** 63 - 1, formatBps)).toBe('9.22 EB/s');
    expect(formatAxisValue(3.4028235e38, formatBps)).toBe('3.40e38 B/s');
    expect(formatAxisValue(2 ** 63 - 1, formatBps)).not.toContain('G/s');
  });

  it('短标签原样保留，百分比/未知单位不丢单位', () => {
    expect(formatAxisValue(45, formatBps)).toBe('45 B/s');
    expect(formatAxisValue(1_500, formatBps)).toBe('1.5 KB/s');
    expect(formatAxisValue(42, (value) => `${value.toFixed(1)}%`)).toBe('42.0%');
    expect(formatAxisValue(100, (value) => `${value.toFixed(1)}%`)).toBe('100.0%');
  });
});

describe('axisPaddingLeft（y 轴左留白）', () => {
  it('20 GB/s 轴的每个刻度标签左缘都不为负', () => {
    const scale = yScale(9_876_543_210);
    const labels = tickValues(scale).map((value) => formatAxisValue(value, formatBps));
    expect(labels).toEqual(['0 B/s', '5.00 GB/s', '10.00 GB/s', '15.00 GB/s', '20.00 GB/s']);
    const padding = axisPaddingLeft(labels);
    // 渲染式：x = padding - 6、textAnchor=end，左缘 = x - 估算宽。
    for (const label of labels) {
      expect(padding - 6 - estimateAxisLabelWidth(label)).toBeGreaterThanOrEqual(0);
    }
  });

  it('EB 档极端轴仍能容纳（96px 上限内）', () => {
    const labels = ['0 B/s', '5.00 EB/s', '10.00 EB/s', '15.00 EB/s', '20.00 EB/s'];
    const padding = axisPaddingLeft(labels);
    expect(padding).toBeLessThanOrEqual(96);
    for (const label of labels) {
      expect(padding - 6 - estimateAxisLabelWidth(label)).toBeGreaterThanOrEqual(0);
    }
  });

  it('短标签保持 44px 下限，长标签按宽度增长并有上限', () => {
    expect(axisPaddingLeft(['0%', '25%', '50%'])).toBe(44);
    expect(axisPaddingLeft(['0 B/s', '1.5 KB/s'])).toBe(estimateAxisLabelWidth('1.5 KB/s') + 8);
    expect(axisPaddingLeft(['x'.repeat(100)])).toBe(96);
  });
});

describe('formatChartTime（x 轴/tooltip 时间）', () => {
  it('长跨度（≥12h）带月/日，短跨度只给时:分', () => {
    const time = Date.parse('2026-11-19T09:13:00Z');
    expect(formatChartTime(time, 6 * 3_600_000)).toMatch(/^\d{2}:\d{2}$/);
    expect(formatChartTime(time, 24 * 3_600_000)).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    expect(formatChartTime(time, LONG_TIME_SPAN_MS)).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    expect(formatChartTime(Number.NaN, 24 * 3_600_000)).toBe('—');
  });
});

describe('formatCoverageDuration（覆盖范围文案）', () => {
  it('分钟/小时/天分级，2 小时不会写成 0.1 天', () => {
    expect(formatCoverageDuration(30_000)).toBe('不足 1 分钟');
    expect(formatCoverageDuration(45 * 60_000)).toBe('45 分钟');
    expect(formatCoverageDuration(2 * 3_600_000)).toBe('2 小时');
    expect(formatCoverageDuration(2.5 * 3_600_000)).toBe('2.5 小时');
    expect(formatCoverageDuration(24 * 3_600_000)).toBe('24 小时');
    expect(formatCoverageDuration(Number.NaN)).toBe('不足 1 分钟');
  });
});

// 走查第 16 轮 low：覆盖提示专用分钟精度，23h49m 不得再四舍五入成 24 小时。
describe('formatCoverageDurationPrecise（覆盖提示分钟精度）', () => {
  it('非整小时保留分钟：23h49m 不再与「24 小时窗口」同值', () => {
    expect(formatCoverageDurationPrecise(30_000)).toBe('不足 1 分钟');
    expect(formatCoverageDurationPrecise(45 * 60_000)).toBe('45 分钟');
    expect(formatCoverageDurationPrecise(2 * 3_600_000)).toBe('2 小时');
    expect(formatCoverageDurationPrecise(2.5 * 3_600_000)).toBe('2 小时 30 分钟');
    expect(formatCoverageDurationPrecise(24 * 3_600_000)).toBe('24 小时');
    expect(formatCoverageDurationPrecise((23 * 60 + 49) * 60_000)).toBe('23 小时 49 分钟');
    // 秒级偏差四舍五入到分钟：20h59m50s 仍读「21 小时」（窗口终点=当前
    // 时刻，末个采样天然差几十秒）；23h58m50s 读「23 小时 59 分钟」。
    expect(formatCoverageDurationPrecise((21 * 60 - 1) * 60_000 + 50_000)).toBe('21 小时');
    expect(formatCoverageDurationPrecise((24 * 60 - 2) * 60_000 + 50_000)).toBe('23 小时 59 分钟');
    // 跨天仍读「天」，不受小时段分钟精度影响。
    expect(formatCoverageDurationPrecise(36 * 3_600_000)).toBe('1.5 天');
    expect(formatCoverageDurationPrecise(Number.NaN)).toBe('不足 1 分钟');
  });
});

describe('windowTickTimes（选中窗口的等距刻度）', () => {
  it('首尾等于窗口边界，中间等距', () => {
    const start = Date.parse('2026-11-19T06:43:00Z');
    const end = Date.parse('2026-11-20T06:43:00Z');
    const ticks = windowTickTimes(start, end, 5);
    expect(ticks).toHaveLength(5);
    expect(ticks[0]).toBe(start);
    expect(ticks[4]).toBe(end);
    expect(ticks[2]).toBe(start + (end - start) / 2);
  });

  it('非法窗口返回空数组，刻度数钳在 2..8', () => {
    expect(windowTickTimes(10, 10)).toEqual([]);
    expect(windowTickTimes(Number.NaN, 10)).toEqual([]);
    expect(windowTickTimes(0, 100, 99)).toHaveLength(8);
    expect(windowTickTimes(0, 100, 0)).toHaveLength(2);
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
