// 监控看板图表的纯计算层（无 React、无 DOM，node 环境直测）。
//
// 五张图的数据形状直接对应后端聚合接口（本轮图表方案 / CONTRACT.md 监控章节）：
//   · Top12        ← GET /api/monitor/overview 的 servers[]（MetricLatestRow）
//   · 集群趋势      ← GET /api/monitor/trend 的 points[]（nullable 桶）
//   · 告警分布      ← GET /api/alert-events/stats 的 severity_timeline[]
//   · 最吵规则      ← GET /api/alert-events/stats 的 top_rules[]
//   · 巡检均分      ← GET /api/inspection/trend 的 points[]
//
// 所有函数对空输入、单点、乱序、null/NaN 与超长输入都有确定行为；聚合/
// 绘制用数组扫描一律有 2000 点上限（契约「单次响应点数 ≤2000」）。排行
// 例外：Top-K 只保留 K 条（K ≤ 12），允许全量扫描而不会把整份响应复制进
// 内存——服务器总数可能远超 2000，截断输入会漏掉真正的热点机。

import { formatCoverageDuration } from '../chartMath';

export type ChartWindow = '24h' | '7d';

export type RankMetric = 'cpu_pct' | 'mem_pct' | 'disk_pct';

export type SeverityKey = 'critical' | 'warning' | 'info';

/** 纯计算层允许一次处理的最大数据点数（防异常响应把内存打满）。 */
export const MAX_MODEL_POINTS = 2000;

/** 严重度展示顺序（高 → 低）；堆叠绘制时反向从底向上。 */
export const SEVERITY_ORDER: readonly SeverityKey[] = ['critical', 'warning', 'info'];

export const SEVERITY_LABELS: Record<SeverityKey, string> = {
  critical: '严重',
  warning: '警告',
  info: '信息',
};

// ---------------------------------------------------------------------------
// 数据形状（与后端 JSON 字段同名；字段可选是为了兼容尚未 gen:api 的调用方）
// ---------------------------------------------------------------------------

export interface ServerRankRow {
  server_id: string;
  server_name: string;
  cpu_pct?: number | null;
  mem_pct?: number | null;
  disk_pct?: number | null;
  created_at: string;
}

export interface ClusterTrendPoint {
  bucket_start: string;
  cpu_p95?: number | null;
  cpu_avg?: number | null;
  cpu_max?: number | null;
  mem_p95?: number | null;
  mem_avg?: number | null;
  mem_max?: number | null;
  servers?: number | null;
  samples?: number | null;
}

export interface SeverityBucket {
  bucket_start: string;
  info: number;
  warning: number;
  critical: number;
}

export interface NoisyRuleRow {
  rule_id: string;
  rule_name: string;
  severity: string;
  count: number;
  server_count: number;
  last_fired_at?: string | null;
}

export interface InspectionTrendPoint {
  report_id: string;
  created_at: string;
  avg_score: number;
  server_count: number;
  anomaly_count: number;
  trigger: string;
}

export interface InspectionJobOption {
  id: string;
  name: string;
}

// ---------------------------------------------------------------------------
// 通用数值守卫
// ---------------------------------------------------------------------------

/** 有限数字或 null（NaN/Infinity/字符串一律视为缺失）。 */
export function finiteOrNull(value: number | null | undefined): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

/** 计数清洗：负数/NaN → 0，非整数四舍五入。 */
export function toCount(value: number | null | undefined): number {
  const finite = finiteOrNull(value);
  if (finite === null || finite <= 0) {
    return 0;
  }
  return Math.round(finite);
}

/** 百分比文案；缺失值给破折号，绝不显示 NaN。 */
export function percentText(value: number | null | undefined, digits = 1): string {
  const finite = finiteOrNull(value);
  return finite === null ? '—' : `${finite.toFixed(digits)}%`;
}

/** 0..100 百分比 → 0..1 的横向填充比例（越界钳制）。 */
export function rankFraction(value: number | null | undefined): number {
  const finite = finiteOrNull(value);
  if (finite === null) {
    return 0;
  }
  return Math.min(1, Math.max(0, finite / 100));
}

/** 评分钳制到 0..100（几何计算用；展示仍用原值）。 */
export function clampScore(value: number | null | undefined): number {
  const finite = finiteOrNull(value);
  if (finite === null) {
    return 0;
  }
  return Math.min(100, Math.max(0, finite));
}

// ---------------------------------------------------------------------------
// 排行（Top12 / Top8）
// ---------------------------------------------------------------------------

export function metricValue(
  row: Pick<ServerRankRow, RankMetric>,
  metric: RankMetric,
): number | null {
  return finiteOrNull(row[metric]);
}

function compareServerName(left: ServerRankRow, right: ServerRankRow): number {
  const leftName = left.server_name || left.server_id;
  const rightName = right.server_name || right.server_id;
  return (
    leftName.localeCompare(rightName, 'zh-CN') || left.server_id.localeCompare(right.server_id)
  );
}

/** 排行比较：指标降序 → 缺失值最后 → 名称/ID 稳定排序。 */
function compareRankRows(left: ServerRankRow, right: ServerRankRow, metric: RankMetric): number {
  const leftValue = metricValue(left, metric);
  const rightValue = metricValue(right, metric);
  if (leftValue === null && rightValue === null) {
    return compareServerName(left, right);
  }
  if (leftValue === null) {
    return 1;
  }
  if (rightValue === null) {
    return -1;
  }
  if (rightValue !== leftValue) {
    return rightValue - leftValue;
  }
  return compareServerName(left, right);
}

/**
 * 按指定指标降序取 Top N；缺失值排最后，同分按名称/ID 稳定排序。
 * 输入乱序（后端已排或未排）都得到同一结果；limit<=0 返回空数组。
 *
 * 走查第 2 轮高危：此前先 `rows.slice(0, MAX_MODEL_POINTS)` 再排序，超过
 * 2000 台时 UUID 序 2000 之后的最热机直接被丢掉。现在改为有界 Top-K
 * 选择——全量扫描一遍但只保留 max 条（max ≤ 12；MAX_MODEL_POINTS 仍
 * 钳制 limit 的非法大值），内存与排序开销不随服务器总数增长。
 */
export function sortTopServers(
  rows: readonly ServerRankRow[],
  metric: RankMetric,
  limit = 12,
): ServerRankRow[] {
  const max = Math.max(0, Math.min(MAX_MODEL_POINTS, Math.floor(limit)));
  if (max === 0 || rows.length === 0) {
    return [];
  }
  // 已选中的 Top-K，始终按 compareRankRows 升序（第 0 名在前）。
  const top: ServerRankRow[] = [];
  for (const row of rows) {
    // 二分插入位置：相等时插在已有项之后（稳定）。
    let low = 0;
    let high = top.length;
    while (low < high) {
      const mid = (low + high) >> 1;
      const existing = top[mid];
      if (existing === undefined || compareRankRows(row, existing, metric) < 0) {
        high = mid;
      } else {
        low = mid + 1;
      }
    }
    if (low >= max) {
      continue;
    }
    top.splice(low, 0, row);
    if (top.length > max) {
      top.pop();
    }
  }
  return top;
}

/**
 * 「最吵规则」排序：触发次数降序 → 影响服务器数降序 → 规则名升序。
 * 不修改入参，超出 limit 截断。
 */
export function sortNoisyRules(rules: readonly NoisyRuleRow[], limit = 8): NoisyRuleRow[] {
  const max = Math.max(0, Math.min(MAX_MODEL_POINTS, Math.floor(limit)));
  if (max === 0 || rules.length === 0) {
    return [];
  }
  const source = rules.slice(0, MAX_MODEL_POINTS);
  source.sort((left, right) => {
    const leftCount = toCount(left.count);
    const rightCount = toCount(right.count);
    if (rightCount !== leftCount) {
      return rightCount - leftCount;
    }
    const leftServers = toCount(left.server_count);
    const rightServers = toCount(right.server_count);
    if (rightServers !== leftServers) {
      return rightServers - leftServers;
    }
    return (left.rule_name || left.rule_id).localeCompare(
      right.rule_name || right.rule_id,
      'zh-CN',
    );
  });
  return source.slice(0, max);
}

// ---------------------------------------------------------------------------
// 比例尺 / 分带 / 刻度
// ---------------------------------------------------------------------------

export interface Band {
  index: number;
  /** 带起点（像素）。 */
  start: number;
  /** 带宽度（像素，已扣掉间隙）。 */
  size: number;
  /** 带中心（像素）。 */
  center: number;
}

/**
 * 把 [0, extent] 等分成 count 条带，gapRatio 为间隙占比。
 * 单条带时占满 (1-gap) 的宽度并居中；非法 extent/count 返回空数组。
 */
export function bandLayout(count: number, extent: number, gapRatio = 0.24): Band[] {
  const safeCount = Math.max(0, Math.min(MAX_MODEL_POINTS, Math.floor(count)));
  if (safeCount === 0 || !Number.isFinite(extent) || extent <= 0) {
    return [];
  }
  const gap = Math.min(0.9, Math.max(0, Number.isFinite(gapRatio) ? gapRatio : 0));
  const step = extent / safeCount;
  const size = step * (1 - gap);
  const offset = (step - size) / 2;
  return Array.from({ length: safeCount }, (_, index) => ({
    index,
    start: index * step + offset,
    size,
    center: index * step + step / 2,
  }));
}

/**
 * 绘图区内 x（0..extent）→ 最近的等宽分带下标（第 11 轮 medium）。
 *
 * 告警分布/集群趋势的桶在 390px 宽下有 24–28 个，单个透明热区只有约
 * 11px。旧实现让每个桶矩形各自接收 pointer 事件，手指落点会被最近的
 * DOM 矩形「抢走」，与视觉柱中心无对应关系；现在两图共用一个覆盖整张
 * 绘图区的命中面，按触点 x 直接换算最近桶（与 MetricChart 的
 * nearestIndexByX 同口径）。x 越界钳到首/末桶，非法输入返回 -1。
 */
export function nearestBandIndex(x: number, extent: number, count: number): number {
  const safeCount = Math.max(0, Math.min(MAX_MODEL_POINTS, Math.floor(count)));
  if (safeCount === 0 || !Number.isFinite(extent) || extent <= 0 || !Number.isFinite(x)) {
    return -1;
  }
  const step = extent / safeCount;
  const index = Math.floor(x / step);
  return Math.min(safeCount - 1, Math.max(0, index));
}

export interface CountScale {
  /** 轴上限（step 的整数倍，≥ 数据最大值）。 */
  max: number;
  /** 整数步长。 */
  step: number;
  /** 0..max 的整数刻度（含两端）。 */
  ticks: number[];
}

/**
 * 计数轴：整数步长 1/2/3/5/10×10^n，刻度全部为整数。
 * 空/全 0/非法输入回落到 0..4；刻度数量有上限（防异常大值画爆 SVG）。
 */
export function countScale(dataMax: number | null | undefined, segments = 4): CountScale {
  const segs = Math.max(1, Math.min(10, Math.floor(segments)));
  const value = finiteOrNull(dataMax);
  if (value === null || value <= 0) {
    return {
      max: segs,
      step: 1,
      ticks: Array.from({ length: segs + 1 }, (_, index) => index),
    };
  }
  const rough = value / segs;
  let magnitude = 10 ** Math.floor(Math.log10(rough));
  let step = 0;
  // 最多放大 8 个数量级；找不到就退回 ceil（不会死循环）。
  for (let boost = 0; boost < 8 && step === 0; boost++) {
    for (const base of [1, 2, 3, 5, 10]) {
      const candidate = base * magnitude;
      if (candidate >= rough) {
        step = Math.max(1, Math.round(candidate));
        break;
      }
    }
    magnitude *= 10;
  }
  if (step <= 0) {
    step = Math.max(1, Math.ceil(rough));
  }
  const tickCount = Math.max(1, Math.min(100, Math.ceil(value / step)));
  const max = step * tickCount;
  return {
    max,
    step,
    ticks: Array.from({ length: tickCount + 1 }, (_, index) => index * step),
  };
}

/**
 * x 轴文字标签下标：count 个桶最多放 maxLabels 个标签，首尾必选。
 * 返回升序去重下标；count/maxLabels 非法时返回空数组。
 *
 * 走查第 3 轮：旧实现按 ceil 步长取整后再补尾，最后一个标签与前一个
 * 只隔 3 个桶（24h/7d 的 11 字符标签会重叠 16–18px）。改为把
 * maxLabels 个下标均匀铺满 [0, count-1]（四舍五入），首尾间距一致。
 */
export function labelTickIndices(count: number, maxLabels: number): number[] {
  const safeCount = Math.max(0, Math.min(MAX_MODEL_POINTS, Math.floor(count)));
  const max = Math.max(0, Math.floor(maxLabels));
  if (safeCount === 0 || max === 0) {
    return [];
  }
  if (safeCount <= max) {
    return Array.from({ length: safeCount }, (_, index) => index);
  }
  if (max === 1) {
    return [0];
  }
  const step = (safeCount - 1) / (max - 1);
  const indices: number[] = [];
  for (let index = 0; index < max; index++) {
    const position = Math.round(index * step);
    if (indices[indices.length - 1] !== position) {
      indices.push(position);
    }
  }
  return indices;
}

/**
 * 标签文本是否有重复（最多扫描 MAX_MODEL_POINTS 条）。
 *
 * 巡检 x 轴走查第 9 轮 low：跨度 <24h 时只看日期的标签会把同一天的两份
 * 报告（11/19 08:51 与 11/19 14:51）都写成「11/19」，先后无法区分；
 * 检测到格式化后的日标签重复就切到「MM/DD HH:mm」。
 */
export function hasDuplicateLabels(labels: readonly string[], limit = MAX_MODEL_POINTS): boolean {
  const bounded = Math.max(0, Math.min(labels.length, Math.floor(limit)));
  const seen = new Set<string>();
  for (let index = 0; index < bounded; index++) {
    const label = labels[index];
    if (label === undefined) {
      continue;
    }
    if (seen.has(label)) {
      return true;
    }
    seen.add(label);
  }
  return false;
}

export type AxisLabelEdge = 'first' | 'last' | 'middle';

export interface AxisLabelPlacement {
  /** SVG text 的 x（middle 锚点时是中心，start/end 时是贴边坐标）。 */
  x: number;
  anchor: 'start' | 'middle' | 'end';
}

/**
 * x 轴首/末标签定位：能完整放进绘图区就跟随分带中心（middle），放不下
 * 才钳到边缘并改 start/end 锚点。
 *
 * 走查第 3 轮/第 7 轮的规则是「≤4 份按中心、≥5 份首末贴边」——5 份时
 * 首点 cx=147.6 而标签 x=38，相差约半个分带。改成按实际标签宽度判断：
 * 5 份短日期标签能放下就居中；14 份长标签放不下才贴边，两种密度都对齐。
 */
export function axisLabelPlacement(
  center: number,
  labelWidth: number,
  plotLeft: number,
  plotRight: number,
  edge: AxisLabelEdge,
): AxisLabelPlacement {
  const cx = Number.isFinite(center) ? center : plotLeft;
  const width = Math.max(0, Number.isFinite(labelWidth) ? labelWidth : 0);
  const left = Math.min(plotLeft, plotRight);
  const right = Math.max(plotLeft, plotRight);
  if (edge === 'middle') {
    return { x: cx, anchor: 'middle' };
  }
  if (edge === 'first') {
    return cx - width / 2 >= left ? { x: cx, anchor: 'middle' } : { x: left, anchor: 'start' };
  }
  return cx + width / 2 <= right ? { x: cx, anchor: 'middle' } : { x: right, anchor: 'end' };
}

/**
 * 集群趋势桶：丢弃非法 bucket_start、按时间升序复制（后端已排序，这里
 * 仍防御乱序/重复时间；同一时刻只保留第一条）。
 */
export function sortClusterTrend(points: readonly ClusterTrendPoint[]): ClusterTrendPoint[] {
  const limit = Math.min(points.length, MAX_MODEL_POINTS);
  const seen = new Set<number>();
  const timed: Array<{ point: ClusterTrendPoint; time: number }> = [];
  for (let index = 0; index < limit; index++) {
    const point = points[index];
    if (!point) {
      continue;
    }
    const time = Date.parse(point.bucket_start);
    if (!Number.isFinite(time) || seen.has(time)) {
      continue;
    }
    seen.add(time);
    timed.push({ point, time });
  }
  timed.sort((left, right) => left.time - right.time);
  return timed.map((entry) => entry.point);
}

/**
 * 部分空桶的覆盖提示（走查第 15 轮 low）。
 *
 * 服务端恒补满 24/28 个桶；只有 1 天数据时切近 7 天，图上只有右侧几桶
 * 有折线、其余 22 个空桶没有任何说明——值班员无法区分「服务刚上线 /
 * 保留期不足」与「采集停了 6 天」。非空桶少于总桶数时给出样本桶数与
 * 数据跨度；全空由 ChartFrame 的空态负责，这里返回 null 不重复提示。
 * sampleSpanMs 是接口附带的实际首末采样时刻之差；缺失时退化为桶跨度，
 * 但文案必须写「覆盖桶跨度约 …（首末桶未满）」，不能把桶跨度说成数据
 * 跨度（走查第 17 轮 low）。输入上限 MAX_MODEL_POINTS（防异常响应）。
 */
export function trendCoverageNote(
  nonEmpty: readonly boolean[],
  bucketSeconds: number,
  sampleSpanMs: number | null = null,
): string | null {
  const limit = Math.min(nonEmpty.length, MAX_MODEL_POINTS);
  if (limit === 0) {
    return null;
  }
  let samples = 0;
  let first = -1;
  let last = -1;
  for (let index = 0; index < limit; index++) {
    if (nonEmpty[index]) {
      samples += 1;
      if (first < 0) {
        first = index;
      }
      last = index;
    }
  }
  // 全空走空态；全满没有缺失可解释。
  if (samples === 0 || samples >= limit) {
    return null;
  }
  // 优先用接口附带的实际首末采样时刻算跨度（走查第 17 轮 low）：桶是
  // 6 小时整桶，首末桶都只采到一部分时，桶跨度（如 1.8 天）会明显大于
  // 实际数据跨度（1.5 天），让值班员误判保留/采集时长。缺少实际时刻时
  // 用桶跨度兜底，但文案必须写明「覆盖桶跨度」而不是「数据跨度」。
  const actualSpan = finiteOrNull(sampleSpanMs);
  let spanText = '';
  if (actualSpan !== null && actualSpan >= 0) {
    spanText = `，数据跨度 ${formatCoverageDuration(actualSpan)}`;
  } else {
    const bucket = Number.isFinite(bucketSeconds) && bucketSeconds > 0 ? bucketSeconds : 0;
    const bucketSpanMs = (last - first + 1) * bucket * 1000;
    if (bucketSpanMs > 0) {
      spanText = `，覆盖桶跨度约 ${formatCoverageDuration(bucketSpanMs)}（首末桶未满）`;
    }
  }
  return `有样本 ${samples}/${limit} 桶${spanText}；空桶可能是服务刚上线、保留期不足或采集中断，不能当作无压力。`;
}

// ---------------------------------------------------------------------------
// 折线分段（null 断线）
// ---------------------------------------------------------------------------

/**
 * 把 nullable 序列切成「连续有效段」的下标数组。null/undefined/NaN 断开；
 * 单点段也保留（由组件画成圆点，单点态不崩）。最多扫描 2000 点。
 */
export function splitLineRuns(values: readonly (number | null | undefined)[]): number[][] {
  const limit = Math.min(values.length, MAX_MODEL_POINTS);
  const runs: number[][] = [];
  let current: number[] = [];
  for (let index = 0; index < limit; index++) {
    if (finiteOrNull(values[index] ?? null) === null) {
      if (current.length > 0) {
        runs.push(current);
        current = [];
      }
      continue;
    }
    current.push(index);
  }
  if (current.length > 0) {
    runs.push(current);
  }
  return runs;
}

// ---------------------------------------------------------------------------
// 告警分桶（堆叠柱）
// ---------------------------------------------------------------------------

export interface SeverityLevelCount {
  key: SeverityKey;
  value: number;
}

export interface StackedBucket {
  /** 后端原始 bucket_start（保留原文，供下钻筛选精确匹配）。 */
  bucket_start: string;
  total: number;
  /** 固定顺序：critical / warning / info。 */
  levels: SeverityLevelCount[];
}

/**
 * 规范化告警时间线：按 bucket_start 升序、同一时刻重复桶合并求和、
 * 非法时间丢弃、负数计数归零。返回每桶各等级值与总数。
 */
export function stackSeverityTimeline(buckets: readonly SeverityBucket[]): StackedBucket[] {
  const merged = new Map<
    number,
    { start: string; info: number; warning: number; critical: number }
  >();
  const limit = Math.min(buckets.length, MAX_MODEL_POINTS);
  for (let index = 0; index < limit; index++) {
    const bucket = buckets[index];
    if (!bucket) {
      continue;
    }
    const time = Date.parse(bucket.bucket_start);
    if (!Number.isFinite(time)) {
      continue;
    }
    const existing = merged.get(time);
    const info = toCount(bucket.info);
    const warning = toCount(bucket.warning);
    const critical = toCount(bucket.critical);
    if (existing) {
      existing.info += info;
      existing.warning += warning;
      existing.critical += critical;
    } else {
      merged.set(time, { start: bucket.bucket_start, info, warning, critical });
    }
  }
  return [...merged.entries()]
    .sort((left, right) => left[0] - right[0])
    .map(([, counts]) => ({
      bucket_start: counts.start,
      total: counts.critical + counts.warning + counts.info,
      levels: [
        { key: 'critical', value: counts.critical },
        { key: 'warning', value: counts.warning },
        { key: 'info', value: counts.info },
      ],
    }));
}

export interface BucketRange {
  /** 下发 GET /api/alert-events 的 fired_after（含）。 */
  after: string;
  /** 下发 GET /api/alert-events 的 fired_before（不含）。 */
  before: string;
}

/**
 * 桶 → 事件表筛选区间 [bucket_start, bucket_start+bucketSeconds)。
 * 时间非法/桶宽非正返回 null（调用方据此不触发下钻）。
 */
export function bucketRange(
  bucketStart: string,
  bucketSeconds: number | null | undefined,
): BucketRange | null {
  const start = Date.parse(bucketStart);
  const seconds = finiteOrNull(bucketSeconds);
  if (!Number.isFinite(start) || seconds === null || seconds <= 0) {
    return null;
  }
  return {
    after: new Date(start).toISOString(),
    before: new Date(start + seconds * 1000).toISOString(),
  };
}

/** 窗口缺省桶宽（后端写死：24h→3600s、7d→21600s）。 */
export function defaultBucketSeconds(window: ChartWindow): number {
  return window === '7d' ? 21_600 : 3_600;
}

// ---------------------------------------------------------------------------
// 巡检走势
// ---------------------------------------------------------------------------

/** 按 created_at 升序复制；非法时间排最后（保持原相对顺序）。 */
export function sortInspectionTrend(
  points: readonly InspectionTrendPoint[],
): InspectionTrendPoint[] {
  const source = points.slice(0, MAX_MODEL_POINTS);
  return source
    .map((point, index) => ({ point, index, time: Date.parse(point.created_at) }))
    .sort((left, right) => {
      const leftValid = Number.isFinite(left.time);
      const rightValid = Number.isFinite(right.time);
      if (leftValid && rightValid && left.time !== right.time) {
        return left.time - right.time;
      }
      if (leftValid !== rightValid) {
        return leftValid ? -1 : 1;
      }
      return left.index - right.index;
    })
    .map((entry) => entry.point);
}

/**
 * 巡检任务默认选中项：优先「最近一份报告所属任务」（latestReportJobId），
 * 其次是唯一任务；多任务且无最新报告时返回 null。
 *
 * null 在调用方表示「全部任务」——GET /api/inspection/trend 的 job_id
 * 可缺省，返回全部报告（含即时巡检）。走查第 6 轮 high：此前 null 被
 * 当成「未选任务」而不发请求，只有即时报告或唯一任务无报告时图表恒空。
 */
export function pickDefaultJobId(
  jobs: readonly InspectionJobOption[],
  latestReportJobId?: string | null,
): string | null {
  if (latestReportJobId && jobs.some((job) => job.id === latestReportJobId)) {
    return latestReportJobId;
  }
  if (jobs.length === 1) {
    return jobs[0]?.id ?? null;
  }
  return null;
}

/**
 * 单机详情的默认服务器：默认选 CPU 最高的已采集机器（其次按名称稳定），
 * 避免后端按 UUID 返回时默认落在 0% 的空闲机上。空列表返回 null。
 */
export function pickDefaultServerId(rows: readonly ServerRankRow[]): string | null {
  return sortTopServers(rows, 'cpu_pct', 1)[0]?.server_id ?? null;
}

export interface ServerOption {
  value: string;
  label: string;
}

/**
 * 服务器下拉选项：显示名优先、UUID 前 8 位兜底，按显示名 localeCompare
 * 升序。后端 overview 按 UUID 排序，直接透传会让运维在几百台里找不到
 * 「prod-db-01」；排序只复制选项数组，不改动入参。
 */
export function buildServerOptions(rows: readonly ServerRankRow[]): ServerOption[] {
  return rows
    .map((row) => ({
      value: row.server_id,
      label: row.server_name || row.server_id.slice(0, 8),
    }))
    .sort((left, right) => left.label.localeCompare(right.label, 'zh-CN'));
}

// ---------------------------------------------------------------------------
// 文案 / 时效
// ---------------------------------------------------------------------------

/** 超长名称截断（按 Unicode 码点，避免把 emoji 劈成两半）。 */
export function truncateText(value: string, maxLength = 12): string {
  const safeMax = Math.max(0, Math.floor(maxLength));
  if (safeMax === 0) {
    return '';
  }
  const characters = Array.from(value);
  return characters.length > safeMax ? `${characters.slice(0, safeMax).join('')}…` : value;
}

/**
 * 中段截断（按 Unicode 码点）：保留名称开头与结尾。
 *
 * 服务器名常以「批次/角色-编号」结尾，区分信息在尾部（batch-04 与
 * batch-05 共享前 7 个字符）。窄屏只保留前缀会把同类机器截成同一个
 * 「batch-0…」，运维无法判断该处理哪台。这里保留尾部，总长度不超过
 * maxLength（含省略号）；maxLength 过小时退化为前缀截断。
 *
 * 注意：等宽码点模型对中文按 1 个码点计，中文名会明显超宽（11 个汉字
 * ≈121px，而窄屏左列只有 68px）。图表轴标签请用
 * {@link truncateMiddleToWidth}。
 */
export function truncateMiddle(value: string, maxLength = 12): string {
  const safeMax = Math.max(0, Math.floor(maxLength));
  const characters = Array.from(value);
  if (safeMax === 0) {
    return '';
  }
  if (characters.length <= safeMax) {
    return value;
  }
  if (safeMax <= 3) {
    return `${characters.slice(0, safeMax - 1).join('')}…`;
  }
  // 省略号占 1 个码点，余下前后对半；尾部向上取整，多留区分度。
  const tailLength = Math.ceil((safeMax - 1) / 2);
  const headLength = safeMax - 1 - tailLength;
  return `${characters.slice(0, headLength).join('')}…${characters
    .slice(characters.length - tailLength)
    .join('')}`;
}

/** 全角/宽字符码点范围（CJK、假名、谚文、全角标点、emoji 等）。 */
function isWideCodePoint(codePoint: number): boolean {
  return (
    (codePoint >= 0x1100 && codePoint <= 0x115f) ||
    (codePoint >= 0x2e80 && codePoint <= 0xa4cf) ||
    (codePoint >= 0xac00 && codePoint <= 0xd7a3) ||
    (codePoint >= 0xf900 && codePoint <= 0xfaff) ||
    (codePoint >= 0xfe30 && codePoint <= 0xfe4f) ||
    (codePoint >= 0xff00 && codePoint <= 0xff60) ||
    (codePoint >= 0xffe0 && codePoint <= 0xffe6) ||
    (codePoint >= 0x1f300 && codePoint <= 0x1faff) ||
    (codePoint >= 0x20000 && codePoint <= 0x3fffd)
  );
}

/** 单个码点的显示宽度（半角 1 单位 / 全角 2 单位）。 */
function charUnits(char: string): number {
  return isWideCodePoint(char.codePointAt(0) ?? 0) ? 2 : 1;
}

function charsUnits(chars: readonly string[]): number {
  let units = 0;
  for (const char of chars) {
    units += charUnits(char);
  }
  return units;
}

/**
 * 半字号单位的保守像素系数。浏览器实测 11px 无衬线字体的 ASCII 字符
 * 约 6.0px 宽（`web-prod-01` 的 text bbox 为 66.05px），旧 `fontSize/2`
 * 的 5.5px 低估约 9%，导致 64px 预算的标签实际宽 66px、左缘压到 2px。
 * 走查第 12 轮起按 6/11 字号估算：宁可多留 1 个字符的余量，也不越界。
 */
const WIDTH_UNIT_FONT_RATIO = 6 / 11;

/**
 * 文本显示宽度（单位：半个字号）。CJK/全角字符按 2 个单位、ASCII 按 1 个；
 * 像素换算见 {@link estimateTextWidth}（1 单位 ≈ fontSize×6/11）。
 */
export function textDisplayUnits(value: string): number {
  return charsUnits(Array.from(value));
}

/** 文本显示宽度估算（像素）；用于按可用宽度截断轴标签。 */
export function estimateTextWidth(value: string, fontSize = 11): number {
  const size = Number.isFinite(fontSize) && fontSize > 0 ? fontSize : 11;
  return textDisplayUnits(value) * size * WIDTH_UNIT_FONT_RATIO;
}

// 省略号在多数无衬线字体里约半个到一个全角宽；按 1 单位（11px 字号下
// 保守按 6px）估算，让中段截断后的标签仍落在预算内。
const ELLIPSIS_UNITS = 1;
const SEGMENT_SEPARATORS = new Set(['-', '_', '/', ':', ' ']);

function takeStart(chars: readonly string[], budget: number, maxCount: number): string[] {
  const picked: string[] = [];
  let used = 0;
  const limit = Math.max(0, Math.min(maxCount, chars.length));
  for (let index = 0; index < limit; index++) {
    const char = chars[index];
    if (char === undefined) {
      continue;
    }
    const units = charUnits(char);
    if (used + units > budget) {
      break;
    }
    picked.push(char);
    used += units;
  }
  return picked;
}

function takeEnd(chars: readonly string[], budget: number): string[] {
  const picked: string[] = [];
  let used = 0;
  for (let index = chars.length - 1; index >= 0; index--) {
    const char = chars[index];
    if (char === undefined) {
      continue;
    }
    const units = charUnits(char);
    if (used + units > budget) {
      break;
    }
    picked.unshift(char);
    used += units;
  }
  return picked;
}

/** 最后一个分隔符之后片段的显示宽度（无分隔符则返回整串宽度）。 */
function lastSegmentUnits(chars: readonly string[]): number {
  let start = 0;
  for (let index = chars.length - 1; index >= 0; index--) {
    const char = chars[index];
    if (char !== undefined && SEGMENT_SEPARATORS.has(char)) {
      start = index + 1;
      break;
    }
  }
  let units = 0;
  for (let index = start; index < chars.length; index++) {
    const char = chars[index];
    if (char !== undefined) {
      units += charUnits(char);
    }
  }
  return units;
}

/**
 * 按可用像素宽度做中段截断（CJK 按 2 个等宽单位计）。
 *
 * 走查第 6 轮 medium：Top 12 / 最吵规则的轴标签此前按码点截断，11 个
 * 汉字宽 121px 而窄屏左列只有 68px，文字左缘被裁成负坐标，机器名/规则
 * 名不可辨认。这里以「半字号」为宽度单位（fontSize=11 时按保守的 6px/
 * 单位，见 {@link WIDTH_UNIT_FONT_RATIO}），保证结果估算宽度 ≤ maxWidthPx。
 * 尾部是 `-`/`_`/`/`/`:`/空格分隔的最后一段时优先整段保留（服务器名的
 * 区域/编号后缀区分度最高），但至少给开头留 1 个单位，否则退回对半分配。
 */
export function truncateMiddleToWidth(value: string, maxWidthPx: number, fontSize = 11): string {
  const size = Number.isFinite(fontSize) && fontSize > 0 ? fontSize : 11;
  const safeWidth = Number.isFinite(maxWidthPx) ? Math.max(0, maxWidthPx) : 0;
  const maxUnits = Math.floor(safeWidth / (size * WIDTH_UNIT_FONT_RATIO));
  const chars = Array.from(value);
  if (chars.length === 0 || charsUnits(chars) <= maxUnits) {
    return value;
  }
  if (maxUnits <= ELLIPSIS_UNITS) {
    return maxUnits === ELLIPSIS_UNITS ? '…' : '';
  }
  const remaining = maxUnits - ELLIPSIS_UNITS;
  let tailBudget = Math.ceil(remaining / 2);
  const segmentUnits = lastSegmentUnits(chars);
  if (segmentUnits > tailBudget && segmentUnits <= remaining - 1) {
    tailBudget = segmentUnits;
  }
  let tail = takeEnd(chars, tailBudget);
  const headBudget = remaining - charsUnits(tail);
  const head = takeStart(chars, headBudget, chars.length - tail.length);
  if (head.length === 0) {
    // 剩余预算不足一个汉字时只保留尾部（尾部是区分信息），仍不越界。
    tail = takeEnd(chars, remaining);
    if (tail.length > 0) {
      return `…${tail.join('')}`;
    }
  }
  if (head.length === 0 || tail.length === 0) {
    // 极窄宽度：退化为前缀截断，避免输出仅为省略号。
    const prefix = takeStart(chars, maxUnits - ELLIPSIS_UNITS, chars.length);
    return `${prefix.join('')}…`;
  }
  return `${head.join('')}…${tail.join('')}`;
}

/**
 * 最吵规则轴标签截断：**保留前缀**，并在预算允许时保留可辨识的尾部
 * （CJK 按 2 个等宽单位计）。
 *
 * 走查第 17 轮 low：「网络入流量异常」与「网络出流量异常」的差异字
 * （入/出）在名称中段，中段截断后两行都变成「网络…量异常」，同一张图
 * 上分不清入/出。规则名的区分信息集中在前缀（网络入/网络出、CPU/内存/
 * 磁盘），因此优先保留前缀；同时给尾部留最多 2 个汉字，让系统哨兵的
 * 「（系统）」后缀（走查第 6 轮的可辨认性要求）不被丢掉。宽度换算与
 * {@link truncateMiddleToWidth} 同口径。
 */
const RULE_SYSTEM_SUFFIX = '（系统）';
/** 尾部预算按不含左括号的「系统）」计（6 单位），给前缀多留 1 个汉字。 */
const RULE_SYSTEM_SUFFIX_TAIL = '系统）';

export function truncateRuleNameToWidth(value: string, maxWidthPx: number, fontSize = 11): string {
  const size = Number.isFinite(fontSize) && fontSize > 0 ? fontSize : 11;
  const safeWidth = Number.isFinite(maxWidthPx) ? Math.max(0, maxWidthPx) : 0;
  const maxUnits = Math.floor(safeWidth / (size * WIDTH_UNIT_FONT_RATIO));
  const chars = Array.from(value);
  if (chars.length === 0 || charsUnits(chars) <= maxUnits) {
    return value;
  }
  if (maxUnits <= ELLIPSIS_UNITS) {
    return maxUnits === ELLIPSIS_UNITS ? '…' : '';
  }
  const remaining = maxUnits - ELLIPSIS_UNITS;
  // 尾部预算：系统规则保留完整「（系统）」（6 单位），其它规则最多 2 个
  // 汉字（4 单位），保证前缀至少还能放下「网络入」这类 3 字区分词。
  const preferredTail = value.endsWith(RULE_SYSTEM_SUFFIX)
    ? textDisplayUnits(RULE_SYSTEM_SUFFIX_TAIL)
    : 4;
  const tailBudget = Math.min(preferredTail, Math.max(0, remaining - 2));
  const tail = tailBudget >= 2 ? takeEnd(chars, tailBudget) : [];
  const headBudget = remaining - charsUnits(tail);
  const head = takeStart(chars, headBudget, chars.length - tail.length);
  if (head.length === 0) {
    return tail.length > 0 ? `…${tail.join('')}` : '…';
  }
  if (tail.length === 0) {
    return `${head.join('')}…`;
  }
  return `${head.join('')}…${tail.join('')}`;
}

/** 采集时间距 now 的毫秒数；时间非法返回 null。未来时间按 0 处理。 */
export function staleAgeMs(
  createdAt: string | null | undefined,
  nowMs: number | null | undefined,
): number | null {
  if (!createdAt) {
    return null;
  }
  const created = Date.parse(createdAt);
  const now = finiteOrNull(nowMs);
  if (!Number.isFinite(created) || now === null) {
    return null;
  }
  return Math.max(0, now - created);
}

/**
 * 采样时间超前 now 的毫秒数（created - now）；不超前/时间非法返回 null。
 *
 * 走查第 14 轮 medium：值班机与服务器时钟相差几分钟时，未来时间戳会被
 * staleAgeMs 钳成 0，页面显示「最新采样（1 秒前）」并当作当前值。调用方
 * 用本函数识别超前样本，并改用「采样时间超前 N（请检查时钟）」文案。
 */
export function sampleTimeAheadMs(
  createdAt: string | null | undefined,
  nowMs: number | null | undefined,
): number | null {
  if (!createdAt) {
    return null;
  }
  const created = Date.parse(createdAt);
  const now = finiteOrNull(nowMs);
  if (!Number.isFinite(created) || now === null) {
    return null;
  }
  const ahead = created - now;
  return ahead > 0 ? ahead : null;
}

/**
 * 采样时间超前的判定容差（毫秒）。只吸收请求往返、时间戳取整造成的秒级
 * 抖动，**不能**拿整个采集间隔当容差：采集间隔调到 600s 后，超前 9 分钟
 * 的样本会落在容差内，被 Top12 当作当前值显示「1 秒前」并排第 1，而趋势
 * SQL 已按服务端 now 严格排除它（走查第 17 轮 medium）。三处必须同口径。
 */
export const FUTURE_SAMPLE_TOLERANCE_MS = 1_000;

/**
 * 样本是否超前服务端时钟。默认只容忍 1 秒：超过即视为时钟错误，排除出
 * 「当前值」、年龄文案与 Top12 排名。调用方若要更严格可传 0。
 */
export function isFutureSample(
  createdAt: string | null | undefined,
  nowMs: number | null | undefined,
  toleranceMs: number | null | undefined = FUTURE_SAMPLE_TOLERANCE_MS,
): boolean {
  const ahead = sampleTimeAheadMs(createdAt, nowMs);
  if (ahead === null) {
    return false;
  }
  const tolerance =
    typeof toleranceMs === 'number' && Number.isFinite(toleranceMs) && toleranceMs >= 0
      ? toleranceMs
      : FUTURE_SAMPLE_TOLERANCE_MS;
  return ahead > tolerance;
}

/**
 * 按采集间隔推导「采样过期」阈值：至少 5 分钟，且不小于两个采集周期。
 *
 * 走查第 2 轮：采集间隔允许 30..3600s，固定 5 分钟会把 10 分钟间隔下的
 * 正常采样全部标成过期。取 2×间隔给采集调度抖动留余量；间隔缺失/非法时
 * 回落 5 分钟。
 */
export function staleThresholdMs(intervalSeconds?: number | null): number {
  const interval = finiteOrNull(intervalSeconds);
  if (interval === null || interval <= 0) {
    return 300_000;
  }
  return Math.max(300_000, interval * 2_000);
}

/**
 * 单机折线的时间窗（epoch ms）。采集暂停时冻结在最后一个采样点：
 * 数据不再更新而窗口继续随真实时钟右移，所有点会被钳到绘图区左缘、
 * 横轴时间与采样时间脱节（走查第 13 轮 medium）。采集正常时按 now
 * 滑动，但终点不早于最后一个采样点——请求恰在挂载之后完成时，最新
 * 点可能比 nowMs 晚几百毫秒，严格裁剪会把它丢掉。暂停但尚无采样时
 * 仍以 now 为终点（图会显示空态）。
 *
 * minutes 非法/非正时按 1 分钟处理；lastSampleMs 非法时回落 now。
 */
export function metricChartWindow(
  minutes: number,
  nowMs: number,
  lastSampleMs: number | null,
  collectorPaused: boolean,
): { start: number; end: number } {
  const spanMinutes = Math.max(1, finiteOrNull(minutes) ?? 1);
  const now = finiteOrNull(nowMs) ?? 0;
  const latest = finiteOrNull(lastSampleMs);
  const end = collectorPaused ? (latest ?? now) : Math.max(now, latest ?? now);
  return { start: end - spanMinutes * 60_000, end };
}

const sampleTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

/** 采样时刻文案（月-日 时:分:秒）；非法时间给「时间未知」。 */
export function formatSampleTime(createdAt: string | null | undefined): string {
  const parsed = createdAt ? Date.parse(createdAt) : Number.NaN;
  if (!Number.isFinite(parsed)) {
    return '时间未知';
  }
  return sampleTimeFormatter.format(parsed);
}

const inspectionTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

/**
 * 巡检报告时间文案（月/日 时:分，第 11 轮 low）。
 *
 * 概要卡「最近巡检均分」此前只显示分数：巡检 5 天没跑时仍是「87分」，
 * 运维无法判断这是今天的还是旧结果。卡片第二行展示报告时间；只到分钟，
 * 390 宽下也能与分数同屏。非法时间给「时间未知」。
 */
export function formatInspectionTime(createdAt: string | null | undefined): string {
  const parsed = createdAt ? Date.parse(createdAt) : Number.NaN;
  if (!Number.isFinite(parsed)) {
    return '时间未知';
  }
  return inspectionTimeFormatter.format(parsed);
}

/** 采样年龄短文案（小时/分钟/秒）。 */
export function formatSampleAge(ageMs: number): string {
  if (ageMs >= 3_600_000) {
    return `${Math.floor(ageMs / 3_600_000)} 小时`;
  }
  if (ageMs >= 60_000) {
    return `${Math.floor(ageMs / 60_000)} 分钟`;
  }
  return `${Math.max(1, Math.floor(ageMs / 1000))} 秒`;
}

/** 采样是否过期（默认 5 分钟；严格大于阈值才算过期）。 */
export function isStale(
  createdAt: string | null | undefined,
  nowMs: number | null | undefined,
  staleAfterMs = 300_000,
): boolean {
  const age = staleAgeMs(createdAt, nowMs);
  const threshold = Math.max(0, finiteOrNull(staleAfterMs) ?? 0);
  return age !== null && age > threshold;
}

/** 一组采样时刻中的最晚值（毫秒）；无有效时间为 null。 */
export function latestTimestamp(rows: readonly { created_at: string }[]): number | null {
  let latest: number | null = null;
  for (const row of rows) {
    const time = Date.parse(row.created_at);
    if (Number.isFinite(time) && (latest === null || time > latest)) {
      latest = time;
    }
  }
  return latest;
}
