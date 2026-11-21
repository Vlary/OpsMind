// M4 五期：MetricChart 的坐标数学（纯函数，node 环境直测）。
// y 轴自动量纲（8.7 监控页）：按数据范围生成“好看”的刻度上限与步长，
// 恒从 0 起步（CPU/内存/磁盘是百分比，负载/流量从 0 起更直观）。

export interface YScale {
  /** 刻度从 0 到 max，含 4 段（5 条网格线）。 */
  max: number;
  /** 每段步长 = max / 4。 */
  step: number;
  /** 数据实际最大值（未放大前）。 */
  dataMax: number;
}

const TICKS = 4;

/** 1/2/5×10^n 的“nice number”对齐（刻度可读）。 */
function niceCeil(value: number): number {
  if (value <= 0) {
    return 1;
  }
  const exponent = Math.floor(Math.log10(value));
  const fraction = value / 10 ** exponent;
  const niceFraction = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10;
  return niceFraction * 10 ** exponent;
}

/**
 * y 轴刻度：数据全 0/空时给 0..100 的百分比缺省量纲。
 *
 * cap 是可选硬上限：数据未超过 cap 时轴固定为 0..cap，只有数据真的超过
 * cap 才允许向上扩张。百分比序列必须传 cap=100——否则 99.25% 的满载经
 * 1.1 倍留白 + niceCeil 会扩到 200%，把满载压到图高一半，视觉上误判成
 * 中等压力。bps/负载等无上界序列不传 cap，保持自动量纲。
 */
export function yScale(maxValue: number, cap?: number): YScale {
  const limit = Number.isFinite(cap) && (cap as number) > 0 ? (cap as number) : null;
  if (!Number.isFinite(maxValue) || maxValue <= 0) {
    const fallback = limit ?? 100;
    return { max: fallback, step: fallback / TICKS, dataMax: 0 };
  }
  if (limit !== null && maxValue <= limit) {
    return { max: limit, step: limit / TICKS, dataMax: maxValue };
  }
  // 顶部留 ~10% 视觉余量后取 nice number。
  const padded = maxValue * 1.1;
  const max = niceCeil(padded);
  return { max, step: max / TICKS, dataMax: maxValue };
}

/** 百分比序列的 y 轴：100% 是硬上限，只有数据越界才扩张。 */
export function percentYScale(maxValue: number): YScale {
  return yScale(maxValue, 100);
}

/** x ∈ [xMin,xMax] → [0,width] 的线性映射；退化区间钉在左缘。 */
export function mapX(x: number, xMin: number, xMax: number, width: number): number {
  if (xMax <= xMin) {
    return 0;
  }
  return ((x - xMin) / (xMax - xMin)) * width;
}

/** y ∈ [0,yMax] → [height,0]（SVG y 向下）。 */
export function mapY(y: number, yMax: number, height: number): number {
  if (yMax <= 0) {
    return height;
  }
  return height - (y / yMax) * height;
}

/** 网格刻度值：0, step, 2step, …, max。 */
export function tickValues(scale: YScale): number[] {
  const values: number[] = [];
  for (let i = 0; i <= TICKS; i++) {
    values.push(+(scale.step * i).toFixed(6));
  }
  return values;
}

/**
 * 通用数值格式：最多 3 位有效数字，极端浮点不再展开成 20 位小数/长指数。
 *
 * 走查第 7 轮 low：负载当前值此前 `toFixed(2)` 对 float32 最大值输出
 * `3.3999999521443642e+38`；三处单位（轴/当前值/0 刻度）也不一致。
 * 常规范围保留短小数（3.4 / 0.42 / 123），≥1e6 或 <1e-3 用科学计数
 * （3.40e38），长度可控且有效位一致。
 */
export function formatSignificant(value: number, digits = 3): string {
  if (!Number.isFinite(value)) {
    return '—';
  }
  const safeDigits = Math.max(1, Math.min(6, Math.floor(digits)));
  const absolute = Math.abs(value);
  if (absolute === 0) {
    return '0';
  }
  if (absolute >= 1e6 || absolute < 1e-3) {
    return value.toExponential(safeDigits - 1).replace('e+', 'e');
  }
  // 常规范围：按有效数字截断并去掉无意义的尾零。
  return String(Number(value.toPrecision(safeDigits)));
}

/**
 * 通用数值轴刻度：按轴 max 统一选一种记法，不在同一根轴上混合普通数字
 * 与科学计数（走查第 9 轮 low）。
 *
 * 旧行为逐刻度调用 formatSignificant：轴 0..2e6 输出
 * `0 | 500000 | 1.00e6 | 1.50e6 | 2.00e6`，读者要在两种记法之间换算。
 * 现在 max≥1e6 时所有非零刻度统一 `toExponential(2)`（0 恒为 "0"），
 * 小量程仍用普通有效数字。是否科学计数只看轴 max，与单个刻度值无关。
 */
export function formatNumericAxisTick(value: number, axisMax: number): string {
  if (!Number.isFinite(value)) {
    return '—';
  }
  if (value === 0) {
    return '0';
  }
  if (Number.isFinite(axisMax) && Math.abs(axisMax) >= 1e6) {
    return value.toExponential(2).replace('e+', 'e');
  }
  return formatSignificant(value, 3);
}

/** 字节速率分级（十进制，契约 7.7 字节/秒）：B/KB/MB/GB/TB/PB/EB。 */
const BPS_UNITS: ReadonlyArray<{ scale: number; label: string }> = [
  { scale: 1e18, label: 'EB/s' },
  { scale: 1e15, label: 'PB/s' },
  { scale: 1e12, label: 'TB/s' },
  { scale: 1e9, label: 'GB/s' },
  { scale: 1e6, label: 'MB/s' },
  { scale: 1e3, label: 'KB/s' },
];

/**
 * 字节速率人类可读：1.23 GB/s / 890 KB/s / 45 B/s。
 * net_in/out_bps 的字段语义是字节/秒（契约 7.7：/proc/net/dev 字节差 ÷
 * interval），此前误标 Kbps/Mbps/Gbps（bit 单位）读数相差 8 倍——五期
 * 评审发现，统一改 byte 单位标签。
 *
 * 走查第 7 轮 low：此前最高只到 GB/s，9.22e18 B/s 被写成
 * `9223372036.85 GB/s`、轴刻度又是丢 B 的 `5000000000G/s`。现在补齐
 * TB/PB/EB；超过 EB 的异常采样（float32 最大值等）用 3 位有效数字的
 * 科学计数回退到 B/s，单位始终可读。
 */
export function formatBps(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond)) {
    return '—';
  }
  const absolute = Math.abs(bytesPerSecond);
  if (absolute < 1_000) {
    return `${Math.round(bytesPerSecond)} B/s`;
  }
  for (let index = 0; index < BPS_UNITS.length; index++) {
    const unit = BPS_UNITS[index];
    if (!unit || absolute < unit.scale) {
      continue;
    }
    const decimals = unit.scale >= 1e6 ? 2 : 1;
    const scaled = bytesPerSecond / unit.scale;
    // 第 11 轮 low：999,999,999,999 B/s 的 999.999999999 GB 经 toFixed(2)
    // 四舍五入成 "1000.00 GB/s"，而同图轴刻度/网络入是 "1.00 TB/s"，
    // 读者会误以为进出相差 1000 倍。格式化后若达到 1000，改用上一档单位
    // （值回到 1.00 TB/s）；视觉柱宽与数值本身都不变。
    if (Math.abs(Number(scaled.toFixed(decimals))) >= 1_000) {
      const bigger = BPS_UNITS[index - 1];
      if (bigger) {
        const biggerDecimals = bigger.scale >= 1e6 ? 2 : 1;
        return `${(bytesPerSecond / bigger.scale).toFixed(biggerDecimals)} ${bigger.label}`;
      }
      // 最大单位 EB 也已到 1000：保持原口径，用科学计数回退到 B/s。
      return `${formatSignificant(bytesPerSecond)} B/s`;
    }
    return `${scaled.toFixed(decimals)} ${unit.label}`;
  }
  return `${Math.round(bytesPerSecond)} B/s`;
}

/** 轴标签允许的原始字符数：超过后走共享的 compact 格式。 */
const AXIS_LABEL_MAX_CHARS = 12;

/**
 * y 轴刻度文案：与卡片当前值共用同一格式（轴与卡头口径一致）。
 *
 * 走查第 2/3 轮曾为省左留白把 "20.00 GB/s" 压成 "20G/s"、丢掉 B；
 * 第 7 轮 low 指出同一张卡里「轴 12.5M/s / 0 刻度 0 B/s / 卡头
 * 20.00 MB/s」三种写法。左留白现已按标签宽度动态计算（上限 96px，
 * 可容纳 14 字符），因此优先原样保留带 B 的完整单位。只有调用方格式
 * 超长时才回退到 formatBps 的紧凑输出（仍带 B 与 /s）。
 */
export function formatAxisValue(value: number, formatValue: (value: number) => string): string {
  const formatted = formatValue(value);
  if (formatted.length <= AXIS_LABEL_MAX_CHARS) {
    return formatted;
  }
  if (formatted.endsWith('/s')) {
    // 轴与卡头共用同一 bps 格式，不再产出丢 B 的 "12.5M/s"。
    return formatBps(value);
  }
  if (formatted.endsWith('%')) {
    return `${value.toFixed(0)}%`;
  }
  // 未知长单位：宁可溢出也不丢单位（避免把字节速率误读成 bit 速率）。
  return formatted;
}

/** y 轴刻度文字按 10px 字号估算的字符宽度（数字/字母约 5–6px）。 */
const AXIS_LABEL_CHAR_PX = 6;
/** 刻度文字与绘图区之间至少留 6px，另加 2px 抗锯齿余量。 */
const AXIS_LABEL_GAP_PX = 8;
/** 左留白下限：短刻度（百分数）沿用原 44px。 */
export const AXIS_LABEL_MIN_PADDING = 44;
/** 左留白上限：未知长单位也不让绘图区被吃掉。 */
export const AXIS_LABEL_MAX_PADDING = 96;

/** 估算刻度文字宽度（纯函数，供布局与渲染测试共用）。 */
export function estimateAxisLabelWidth(label: string): number {
  return label.length * AXIS_LABEL_CHAR_PX;
}

/**
 * 按本轴最宽刻度动态给左留白：`x = paddingLeft - 6` 且
 * `textAnchor="end"`，所以只要 `paddingLeft - 6 >= 文字宽`，标签左缘
 * 就不会为负。极端 bps 轴（"20.00 GB/s" / "9.22 EB/s"）与 "1.5 KB/s"
 * 都能容纳。
 */
export function axisPaddingLeft(
  labels: readonly string[],
  minimum = AXIS_LABEL_MIN_PADDING,
  maximum = AXIS_LABEL_MAX_PADDING,
): number {
  let widest = 0;
  for (const label of labels) {
    if (label.length > widest) {
      widest = label.length;
    }
  }
  return Math.min(maximum, Math.max(minimum, widest * AXIS_LABEL_CHAR_PX + AXIS_LABEL_GAP_PX));
}

/** 时间轴长跨度阈值：≥12h 的窗口在刻度与 tooltip 里补月/日。 */
export const LONG_TIME_SPAN_MS = 12 * 3_600_000;

const shortTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

const longTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

/**
 * 图表时间文案：跨度 ≥12h（含 24h/7d 窗口与跨午夜的长窗口）带月/日，
 * 短窗口只给时:分；非法时间给破折号。
 */
export function formatChartTime(time: number, spanMs: number): string {
  if (!Number.isFinite(time)) {
    return '—';
  }
  const formatter = spanMs >= LONG_TIME_SPAN_MS ? longTimeFormatter : shortTimeFormatter;
  return formatter.format(time);
}

/**
 * 短时长文案：不足 1 分钟 / N 分钟 / N 小时 / N 天。
 * 1h/6h/24h 窗口与 2 小时数据的组合要能读成「2 小时」，不是 "0.08 天"。
 * 覆盖范围提示需要分钟精度时用 formatCoverageDurationPrecise。
 */
export function formatCoverageDuration(spanMs: number): string {
  if (!Number.isFinite(spanMs) || spanMs <= 0) {
    return '不足 1 分钟';
  }
  const minutes = spanMs / 60_000;
  if (minutes < 1) {
    return '不足 1 分钟';
  }
  if (minutes < 60) {
    return `${Math.round(minutes)} 分钟`;
  }
  const hours = minutes / 60;
  // 24h 窗口本身也读作「24 小时」，只有真正跨天（>24h）才切「天」。
  if (hours <= 24) {
    return `${hours >= 10 ? Math.round(hours) : Math.round(hours * 10) / 10} 小时`;
  }
  const days = hours / 24;
  return `${days >= 10 ? Math.round(days) : Math.round(days * 10) / 10} 天`;
}

/**
 * 覆盖提示专用的时长文案（分钟精度）。与 formatCoverageDuration 的区别：
 * 小时段保留分钟，23h49m 不会被四舍五入成「24 小时」——那会与「24 小时
 * 窗口」同值，值班员看不出到底缺了多少（走查第 16 轮 low）。
 *
 * 分钟按四舍五入而不是 floor：窗口终点取「当前时刻」，末个采样与它天然
 * 差几十秒；floor 会把 3 小时缺口算成「20 小时 59 分钟」、把 11 分钟
 * 缺口再少报 1 分钟。四舍五入后 11 分钟级缺口仍完整保留（23h49m），
 * 而不足 1 分钟的缺口本来就被 coverage 容差挡掉、不会出现提示，因此
 * 不会出现「覆盖 24 小时」与窗口同值的误导。
 */
export function formatCoverageDurationPrecise(spanMs: number): string {
  if (!Number.isFinite(spanMs) || spanMs <= 0 || spanMs < 60_000) {
    return '不足 1 分钟';
  }
  const totalMinutes = Math.round(spanMs / 60_000);
  if (totalMinutes < 1) {
    return '不足 1 分钟';
  }
  if (totalMinutes < 60) {
    return `${totalMinutes} 分钟`;
  }
  const hours = Math.floor(totalMinutes / 60);
  const restMinutes = totalMinutes % 60;
  // 24h 窗口本身也读作「24 小时」，只有真正跨天（>24h）才切「天」。
  if (totalMinutes <= 24 * 60) {
    return restMinutes === 0 ? `${hours} 小时` : `${hours} 小时 ${restMinutes} 分钟`;
  }
  const days = totalMinutes / (24 * 60);
  return `${days >= 10 ? Math.round(days) : Math.round(days * 10) / 10} 天`;
}

/**
 * 选中窗口的等距刻度时间：首尾分别贴绘图区左右缘，中间按比例定位。
 * 稀疏数据（如 24h 窗口里只有 2h 采样）也必须让轴表达完整窗口，
 * 不能拿首/末采样点当轴范围。非法窗口返回空数组。
 */
export function windowTickTimes(start: number, end: number, count = 5): number[] {
  const ticks = Math.max(2, Math.min(8, Math.floor(count)));
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) {
    return [];
  }
  return Array.from({ length: ticks }, (_, index) => start + ((end - start) * index) / (ticks - 1));
}

/** 折线 path（polyline points 串）；空数据返回空串。 */
export function buildPolyline(points: ReadonlyArray<{ x: number; y: number }>): string {
  return points.map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`).join(' ');
}

/**
 * 按 x 找最近的点（线性扫描，点数 ≤500 可接受）。
 *
 * hover 与触屏点按共用同一套「最近点」判定：触屏没有 mousemove，点按
 * 时用触点 x 复用此函数选中桶/采样点。空数组返回 -1。
 */
export function nearestIndexByX(coords: ReadonlyArray<{ x: number }>, targetX: number): number {
  let nearestIndex = -1;
  let nearestDistance = Number.POSITIVE_INFINITY;
  for (let index = 0; index < coords.length; index++) {
    const coord = coords[index];
    if (!coord) {
      continue;
    }
    const distance = Math.abs(coord.x - targetX);
    if (distance < nearestDistance) {
      nearestDistance = distance;
      nearestIndex = index;
    }
  }
  return nearestIndex;
}
