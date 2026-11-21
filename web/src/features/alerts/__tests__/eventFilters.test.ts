import { describe, expect, it } from 'vitest';

import {
  eventsFilterReducer,
  eventWindowScope,
  formatRuleDrilldownLabel,
  formatTimeDrilldownLabel,
  formatWindowScopeLabel,
  INITIAL_EVENTS_FILTER_STATE,
  type TimeDrilldown,
} from '../eventFilters';
import { SUSPEND_RULE_NAME } from '../ruleForm';

// 时间格式化按运行环境本地时区，测试用「本地时间构造 → toISOString」
// 消除时区依赖（CI 常为 UTC，开发机为 Asia/Shanghai）。
function localIso(year: number, month: number, day: number, hour: number, minute = 0): string {
  return new Date(year, month - 1, day, hour, minute, 0, 0).toISOString();
}

describe('告警下钻时段 Tag 文案', () => {
  it('24h 窗显示时分区间（服务端桶宽 1h）', () => {
    const after = localIso(2026, 3, 1, 13);
    const before = localIso(2026, 3, 1, 14);
    expect(formatTimeDrilldownLabel(after, before, '24h')).toBe('13:00–14:00');
  });

  it('7d 窗跨天时带月日（服务端桶宽 6h）', () => {
    const after = localIso(2026, 3, 1, 0);
    const before = localIso(2026, 3, 1, 6);
    expect(formatTimeDrilldownLabel(after, before, '7d')).toBe('03/01 00:00–03/01 06:00');
  });

  it('时间非法时退化为「自定义时段」，不输出 Invalid Date', () => {
    expect(formatTimeDrilldownLabel('nope', localIso(2026, 3, 1, 1), '24h')).toBe('自定义时段');
    expect(formatTimeDrilldownLabel(localIso(2026, 3, 1, 1), '', '7d')).toBe('自定义时段');
  });
});

describe('桶终点原样下发（后端半开区间，第 13 轮）', () => {
  it('不经过 JS Date 改写：桶末 500µs 的精度必须保留', () => {
    // Date 只有毫秒精度：旧实现 new Date(parsed - 1).toISOString() 会把
    // …59.9995Z 变成 …59.999Z，恰好排除桶最后 500µs 的事件。
    const microsecondEnd = '2026-11-20T11:59:59.9995Z';
    expect(eventWindowScope('24h', '2026-11-20T11:00:00Z', microsecondEnd).before).toBe(
      microsecondEnd,
    );
  });

  it('非法时间原样返回（交给后端 400，不静默改写）', () => {
    expect(eventWindowScope('24h', '2026-11-20T11:00:00Z', 'not-a-time').before).toBe('not-a-time');
  });
});

describe('告警下钻规则 Tag 文案', () => {
  it('优先使用规则名', () => {
    expect(formatRuleDrilldownLabel('CPU 持续高负载', 'rule-1')).toBe('CPU 持续高负载');
  });

  it('系统哨兵规则用中文展示名，不再显示被截断的英文键（第 8 轮 low）', () => {
    expect(formatRuleDrilldownLabel(SUSPEND_RULE_NAME, 'rule-1')).toBe('采集不可达（系统）');
    expect(formatRuleDrilldownLabel(SUSPEND_RULE_NAME, 'rule-1')).not.toContain('monitor.');
    expect(formatRuleDrilldownLabel(SUSPEND_RULE_NAME, 'rule-1')).not.toContain('…');
  });

  it('空名回退到短 id（不显示裸 UUID）', () => {
    expect(formatRuleDrilldownLabel('   ', '0123456789abcdef')).toBe('01234567');
  });

  it('按 Unicode 码点截断，不劈开 emoji', () => {
    expect(formatRuleDrilldownLabel('😀😀😀😀', 'r', 2)).toBe('😀😀…');
  });

  it('长度恰好等于上限时不加省略号', () => {
    expect(formatRuleDrilldownLabel('abc', 'r', 3)).toBe('abc');
  });
});

describe('告警图表窗口范围（规则下钻与柱子同口径）', () => {
  it('Tag 文案按窗口显示「近 24 小时 / 近 7 天」', () => {
    expect(formatWindowScopeLabel('24h')).toBe('近 24 小时');
    expect(formatWindowScopeLabel('7d')).toBe('近 7 天');
  });

  it('窗口终点原样传给事件接口（同为半开区间，不再减 1ms）', () => {
    const scope = eventWindowScope('24h', '2026-11-19T00:00:00Z', '2026-11-20T00:00:00Z');
    expect(scope).toEqual({
      after: '2026-11-19T00:00:00Z',
      before: '2026-11-20T00:00:00Z',
      label: '近 24 小时',
    });
  });

  it('7 天窗口同样保留 from/to 原值', () => {
    const scope = eventWindowScope('7d', '2026-11-13T00:00:00Z', '2026-11-20T00:00:00Z');
    expect(scope.after).toBe('2026-11-13T00:00:00Z');
    expect(scope.before).toBe('2026-11-20T00:00:00Z');
    expect(scope.label).toBe('近 7 天');
  });

  it('窗口终点非法时原样交给后端暴露 400，不静默改写', () => {
    const scope = eventWindowScope('24h', '2026-11-19T00:00:00Z', 'not-a-time');
    expect(scope.before).toBe('not-a-time');
  });
});

describe('告警分布筛选状态机', () => {
  const selection: TimeDrilldown = {
    after: '2026-11-19T12:00:00.000Z',
    before: '2026-11-19T13:00:00.000Z',
    bucketStart: '2026-11-19T12:00:00.000Z',
    label: '20:00–21:00',
  };

  it('切窗口同时清空时段与规则下钻（规则已绑定窗口，第 12 轮）', () => {
    // 走查第 12 轮：规则下钻现在带 stats.from/to 与柱子同口径，窗口一换
    // 旧边界即失效；保留规则会让事件表退回全时（21 vs 71）。
    let state = eventsFilterReducer(INITIAL_EVENTS_FILTER_STATE, {
      type: 'rule',
      selection: { id: 'rule-1', name: 'CPU 持续高负载' },
    });
    state = eventsFilterReducer(state, { type: 'time', selection });
    state = eventsFilterReducer(state, { type: 'window', window: '7d' });

    expect(state.window).toBe('7d');
    expect(state.timeFilter).toBeNull();
    expect(state.ruleFilter).toBeNull();
  });

  it('切到同一窗口不清掉当前时段（状态幂等）', () => {
    const selected = eventsFilterReducer(INITIAL_EVENTS_FILTER_STATE, {
      type: 'time',
      selection,
    });
    const same = eventsFilterReducer(selected, { type: 'window', window: '24h' });
    expect(same).toBe(selected);
    expect(same.timeFilter).toEqual(selection);
  });

  it('两类筛选可独立清除', () => {
    let state = eventsFilterReducer(INITIAL_EVENTS_FILTER_STATE, { type: 'time', selection });
    state = eventsFilterReducer(state, {
      type: 'rule',
      selection: { id: 'r', name: '规则' },
    });
    const clearedTime = eventsFilterReducer(state, { type: 'clearTime' });
    expect(clearedTime.timeFilter).toBeNull();
    expect(clearedTime.ruleFilter).toEqual({ id: 'r', name: '规则' });
    const clearedAll = eventsFilterReducer(clearedTime, { type: 'clearRule' });
    expect(clearedAll.ruleFilter).toBeNull();
  });
});
