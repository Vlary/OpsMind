// M4 五期：指标折线降采样（CONTRACT.md 8.7 监控页——点数 >500 时抽样
// 绘制、不额外请求；11 节风险表第 15 条同款要求）。
//
// 采用 LTTB（Largest-Triangle-Three-Buckets）：按桶选「与前一选中点及
// 下一桶均值构成最大三角形面积」的点，保峰保形优于等距抽样；首尾点
// 恒保留。纯函数、零依赖，node 环境直测（__tests__/downsample.test.ts）。

export interface SeriesPoint {
  /** 毫秒时间戳。 */
  t: number;
  v: number;
}

export const CHART_MAX_POINTS = 500;

/** 是否需要抽样（>500 点才抽，与契约阈值一致）。 */
export function needsDownsample(points: readonly SeriesPoint[], max = CHART_MAX_POINTS): boolean {
  return points.length > max;
}

/**
 * LTTB 降采样：n 点 → 至多 max 点（max >= 2 时恒保留首尾）。
 * 输入点数 <= max 时原样返回（同一引用，避免无谓拷贝）。
 * 输入与 max 无效（空数组 / max < 2）时返回空数组。
 */
export function downsample(points: readonly SeriesPoint[], max = CHART_MAX_POINTS): SeriesPoint[] {
  if (points.length <= max) {
    return points.slice();
  }
  if (max < 2 || points.length < 2) {
    return points.slice(0, Math.max(0, max));
  }

  const sampled: SeriesPoint[] = [];
  const bucketSize = (points.length - 2) / (max - 2);
  sampled.push(points[0] as SeriesPoint);
  let previousIndex = 0;

  for (let i = 1; i < max - 1; i++) {
    const bucketStart = Math.floor((i - 1) * bucketSize) + 1;
    const bucketEnd = Math.min(Math.floor(i * bucketSize) + 1, points.length - 1);
    const nextStart = Math.min(Math.floor(i * bucketSize) + 1, points.length - 1);
    const nextEnd = Math.min(Math.floor((i + 1) * bucketSize) + 1, points.length - 1);

    // 下一桶均值（LTTB 的 third point；末桶即尾点）。
    let avgT = 0;
    let avgV = 0;
    let count = 0;
    for (let j = nextStart; j < nextEnd; j++) {
      const point = points[j];
      if (point) {
        avgT += point.t;
        avgV += point.v;
        count++;
      }
    }
    if (count === 0) {
      const tail = points[points.length - 1];
      if (tail) {
        avgT = tail.t;
        avgV = tail.v;
      }
    } else {
      avgT /= count;
      avgV /= count;
    }

    // 当前桶内选三角形面积最大的点。
    const anchor = points[previousIndex];
    let bestIndex = bucketStart;
    let bestArea = -1;
    if (anchor) {
      for (let j = bucketStart; j < bucketEnd; j++) {
        const candidate = points[j];
        if (!candidate) {
          continue;
        }
        const area =
          Math.abs(
            (anchor.t - avgT) * (candidate.v - anchor.v) -
              (anchor.t - candidate.t) * (avgV - anchor.v),
          ) / 2;
        if (area > bestArea) {
          bestArea = area;
          bestIndex = j;
        }
      }
    }
    sampled.push(points[bestIndex] as SeriesPoint);
    previousIndex = bestIndex;
  }

  sampled.push(points[points.length - 1] as SeriesPoint);
  return sampled;
}

/** 抽样后点数上限校验（守卫：抽样绝不允许放大点数）。 */
export function downsampleGuard(points: readonly SeriesPoint[], max = CHART_MAX_POINTS): number {
  const out = downsample(points, max);
  return out.length;
}
