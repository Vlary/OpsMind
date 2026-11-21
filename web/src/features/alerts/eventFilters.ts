// 告警事件下钻筛选的纯展示逻辑（node 环境直测）。
//
// 图表点击产生两类筛选：规则（rule_id）与时段（fired_after/fired_before，
// 由告警分桶的 bucket_start + bucket_seconds 换算）。两者都只是事件表
// GET /api/alert-events 的查询参数，可与状态/等级筛选、before_id 游标叠加。

import type { ChartWindow } from '../monitor/charts/chartModel';

import { ruleDisplayName } from './ruleForm';

export interface RuleDrilldown {
  id: string;
  name: string;
}

export interface TimeDrilldown {
  /** RFC3339，含。 */
  after: string;
  /** RFC3339，不含。 */
  before: string;
  /** 后端原始 bucket_start，用于在图上高亮同一桶。 */
  bucketStart: string;
  /** 筛选行 Tag 上的中文时段。 */
  label: string;
}

const hourMinute = new Intl.DateTimeFormat('zh-CN', {
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

const monthDayHourMinute = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

/**
 * 桶区间 → 筛选 Tag 文案。24h 窗只显示时分（13:00–14:00）；
 * 7d 窗跨天，带上月日。时间非法时退化为「自定义时段」，不显示 Invalid Date。
 */
export function formatTimeDrilldownLabel(
  after: string,
  before: string,
  window: ChartWindow,
): string {
  const start = Date.parse(after);
  const end = Date.parse(before);
  if (!Number.isFinite(start) || !Number.isFinite(end)) {
    return '自定义时段';
  }
  const formatter = window === '7d' ? monthDayHourMinute : hourMinute;
  return `${formatter.format(start)}–${formatter.format(end)}`;
}

/**
 * 规则 Tag 文案：优先规则展示名；系统哨兵/空名时退回短 id，绝不显示裸 UUID。
 * 名称按 Unicode 码点截断，避免过长的用户自定义规则把筛选行撑爆。
 *
 * 走查第 8 轮 low：系统哨兵规则（monitor.collector.unreachable）的库内
 * 名是英文键，Tag 此前直出原文且被截断成 `monitor.collector.unreac…`，
 * 与图表/事件表的「采集不可达（系统）」对不上。这里复用同一份
 * ruleDisplayName 映射，保证下钻 Tag 与页面其他位置逐字一致。
 */
export function formatRuleDrilldownLabel(name: string, id: string, maxLength = 24): string {
  const display = ruleDisplayName(name);
  const source = display.trim() !== '' ? display : id.slice(0, 8);
  const characters = Array.from(source);
  if (characters.length <= maxLength) {
    return source;
  }
  return `${characters.slice(0, maxLength).join('')}…`;
}

/**
 * 图表窗口的筛选 Tag 文案。规则下钻现在与柱子同口径（事件表用 stats 的
 * from/to），Tag 必须写清窗口，避免「柱上 21 次、表里 71 条」的无说明并列。
 */
export function formatWindowScopeLabel(window: ChartWindow): string {
  return window === '7d' ? '近 7 天' : '近 24 小时';
}

/** 图表窗口 → 事件表查询边界（半开区间 [after, before)）。 */
export interface EventWindowScope {
  /** RFC3339，含。 */
  after: string;
  /** RFC3339，不含。 */
  before: string;
  label: string;
}

/**
 * 规则下钻的窗口边界直接取自当前 stats 响应（stats.from/to），而不是
 * 点击瞬间的快照：图表每 60s 刷新、窗口按桶滑动，快照会让表格与柱子
 * 随时间重新错开。边界语义与后端一致（stats 为 [from, to)）。
 *
 * 走查第 13 轮 medium：终点原样下发，不再用 JS Date 减 1ms。Date 只有
 * 毫秒精度，桶末 500µs 的事件（…59.9995Z）落在 [start, end) 桶内，却会被
 * 「减 1ms 的闭区间」排除——旧实现柱上 13 条、表里 12 行。后端的
 * fired_before 已改为半开语义（`fired_at < before`）。
 */
export function eventWindowScope(window: ChartWindow, from: string, to: string): EventWindowScope {
  return {
    after: from,
    before: to,
    label: formatWindowScopeLabel(window),
  };
}

/** 告警分布两图的时间窗与下钻筛选（事件表与图表共用一份状态）。 */
export interface EventsFilterState {
  window: ChartWindow;
  ruleFilter: RuleDrilldown | null;
  timeFilter: TimeDrilldown | null;
}

export type EventsFilterAction =
  | { type: 'window'; window: ChartWindow }
  | { type: 'time'; selection: TimeDrilldown }
  | { type: 'rule'; selection: RuleDrilldown }
  | { type: 'clearTime' }
  | { type: 'clearRule' };

export const INITIAL_EVENTS_FILTER_STATE: EventsFilterState = {
  window: '24h',
  ruleFilter: null,
  timeFilter: null,
};

/**
 * 告警页筛选状态机。
 *
 * 关键约束：切换 24h/7d 时清空两类下钻。两个窗口的服务端桶宽不同
 * （24h→1h、7d→6h），旧下钻的 after/before 仍按 1 小时范围过滤事件表，
 * 而图上高亮的是 6 小时桶——Tag、表格与视觉指示三者不一致。
 *
 * 走查第 12 轮 medium：规则下钻不再「与窗口无关」——它现在绑定图表窗口
 * （事件表用 stats.from/to 与柱子同口径，见 {@link eventWindowScope}），
 * 窗口一换旧边界即失效，因此必须与时段下钻一起清空；否则规则会保留、
 * 事件表退回全时（柱上 21 次 vs 表里 71 条）。切回原窗口也不恢复旧下钻。
 */
export function eventsFilterReducer(
  state: EventsFilterState,
  action: EventsFilterAction,
): EventsFilterState {
  switch (action.type) {
    case 'window':
      if (state.window === action.window) {
        return state;
      }
      return { ...state, window: action.window, timeFilter: null, ruleFilter: null };
    case 'time':
      return { ...state, timeFilter: action.selection };
    case 'rule':
      return { ...state, ruleFilter: action.selection };
    case 'clearTime':
      return state.timeFilter === null ? state : { ...state, timeFilter: null };
    case 'clearRule':
      return state.ruleFilter === null ? state : { ...state, ruleFilter: null };
    default:
      return state;
  }
}
