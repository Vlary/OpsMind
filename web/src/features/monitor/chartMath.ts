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

/** y 轴刻度：数据全 0/空时给 0..100 的百分比缺省量纲。 */
export function yScale(maxValue: number): YScale {
  if (!Number.isFinite(maxValue) || maxValue <= 0) {
    return { max: 100, step: 100 / TICKS, dataMax: 0 };
  }
  // 顶部留 ~10% 视觉余量后取 nice number。
  const padded = maxValue * 1.1;
  const max = niceCeil(padded);
  return { max, step: max / TICKS, dataMax: maxValue };
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

/** 字节速率人类可读：1.2 MB/s / 890 KB/s / 45 B/s。
 * net_in/out_bps 的字段语义是字节/秒（契约 7.7：/proc/net/dev 字节差 ÷
 * interval），此前误标 Kbps/Mbps/Gbps（bit 单位）读数相差 8 倍——五期
 * 评审发现，统一改 byte 单位标签。 */
export function formatBps(bytesPerSecond: number): string {
  const value = Math.abs(bytesPerSecond);
  if (value >= 1_000_000_000) {
    return `${(bytesPerSecond / 1_000_000_000).toFixed(2)} GB/s`;
  }
  if (value >= 1_000_000) {
    return `${(bytesPerSecond / 1_000_000).toFixed(2)} MB/s`;
  }
  if (value >= 1_000) {
    return `${(bytesPerSecond / 1_000).toFixed(1)} KB/s`;
  }
  return `${Math.round(bytesPerSecond)} B/s`;
}

/** 折线 path（polyline points 串）；空数据返回空串。 */
export function buildPolyline(points: ReadonlyArray<{ x: number; y: number }>): string {
  return points.map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`).join(' ');
}
