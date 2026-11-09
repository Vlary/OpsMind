// M4 五期：原生 SVG 指标折线图（CONTRACT.md 8.0——不引图表库；8.7 监控页
// ——网格、y 轴自动量纲、hover 竖线 tooltip、点数 >500 抽样绘制）。
//
// 尺寸自适应走 ResizeObserver（卸载即 disconnect，满足 8.7“页面卸载全量
// 清理”）；hover 状态为组件本地 state，不进全局 store。

import { useEffect, useMemo, useRef, useState } from 'react';

import { CHART_MAX_POINTS, downsample, needsDownsample, type SeriesPoint } from './downsample';
import { buildPolyline, mapX, mapY, tickValues, yScale } from './chartMath';

const CHART_HEIGHT = 160;
const PADDING_LEFT = 44;
const PADDING_RIGHT = 8;
const PADDING_TOP = 8;
const PADDING_BOTTOM = 20;

export interface MetricChartProps {
  /** 折线数据（时间升序）。 */
  points: SeriesPoint[];
  /** 数值格式化（缺省两位小数百分比；bps 用 formatBps）。 */
  formatValue?: (value: number) => string;
  /** 折线颜色（语义令牌 `var(--color-chart-N)`，缺省 `--color-chart-1`）。 */
  color?: string;
}

interface HoverState {
  /** 图内 x 坐标。 */
  x: number;
  point: SeriesPoint;
}

const timeFormatter = new Intl.DateTimeFormat('zh-CN', {
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

export default function MetricChart({
  points,
  formatValue = (value) => `${value.toFixed(1)}%`,
  color = 'var(--color-chart-1)',
}: MetricChartProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(0);
  const [hover, setHover] = useState<HoverState | null>(null);

  // 宽度自适应：ResizeObserver 在容器尺寸变化时更新（含首次挂载）；
  // 卸载/重挂载清理 observer，避免泄漏。
  useEffect(() => {
    const element = containerRef.current;
    if (!element) {
      return;
    }
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        setWidth(entry.contentRect.width);
      }
    });
    observer.observe(element);
    return () => {
      observer.disconnect();
    };
  }, []);

  const sampled = useMemo(
    () => (needsDownsample(points) ? downsample(points, CHART_MAX_POINTS) : points),
    [points],
  );
  const sampledCount = sampled.length;

  const scale = useMemo(() => {
    let max = 0;
    for (const point of sampled) {
      if (point.v > max) {
        max = point.v;
      }
    }
    return yScale(max);
  }, [sampled]);

  const plotWidth = Math.max(0, width - PADDING_LEFT - PADDING_RIGHT);
  const plotHeight = CHART_HEIGHT - PADDING_TOP - PADDING_BOTTOM;

  const geometry = useMemo(() => {
    if (sampledCount < 2 || plotWidth <= 0) {
      return null;
    }
    const tMin = (sampled[0] as SeriesPoint).t;
    const tMax = (sampled[sampledCount - 1] as SeriesPoint).t;
    const coords = sampled.map((point) => ({
      x: PADDING_LEFT + mapX(point.t, tMin, tMax, plotWidth),
      y: PADDING_TOP + mapY(point.v, scale.max, plotHeight),
    }));
    return { tMin, tMax, coords };
  }, [sampled, sampledCount, plotWidth, plotHeight, scale]);

  function handleMove(event: React.MouseEvent<SVGSVGElement>): void {
    if (!geometry || plotWidth <= 0) {
      return;
    }
    const rect = event.currentTarget.getBoundingClientRect();
    const svgX = ((event.clientX - rect.left) / rect.width) * width;
    if (svgX < PADDING_LEFT || svgX > width - PADDING_RIGHT) {
      setHover(null);
      return;
    }
    // 最近点查找（点数 ≤500，线性扫描可接受）。
    let nearest = geometry.coords[0];
    let nearestDistance = Number.POSITIVE_INFINITY;
    for (const coord of geometry.coords) {
      const distance = Math.abs(coord.x - svgX);
      if (distance < nearestDistance) {
        nearestDistance = distance;
        nearest = coord;
      }
    }
    if (!nearest) {
      setHover(null);
      return;
    }
    const index = geometry.coords.indexOf(nearest);
    const source = sampled[index];
    if (source) {
      setHover({ x: nearest.x, point: source });
    }
  }

  if (sampledCount === 0) {
    return (
      <div
        ref={containerRef}
        style={{
          height: CHART_HEIGHT,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          color: 'var(--color-text-tertiary)',
          fontSize: 12,
        }}
      >
        暂无采集数据
      </div>
    );
  }

  return (
    <div ref={containerRef} style={{ position: 'relative' }}>
      <svg
        width={width || 320}
        height={CHART_HEIGHT}
        style={{ display: 'block', width: '100%' }}
        role="img"
        onMouseMove={handleMove}
        onMouseLeave={() => {
          setHover(null);
        }}
      >
        {/* 横向网格 + y 轴刻度。 */}
        {tickValues(scale).map((value, index) => {
          const y = PADDING_TOP + mapY(value, scale.max, plotHeight);
          return (
            <g key={index}>
              <line
                x1={PADDING_LEFT}
                x2={width - PADDING_RIGHT}
                y1={y}
                y2={y}
                stroke="var(--color-border-subtle)"
                strokeWidth={1}
              />
              <text
                x={PADDING_LEFT - 6}
                y={y + 3}
                textAnchor="end"
                fontSize={10}
                fill="var(--color-text-tertiary)"
              >
                {formatAxisValue(value, formatValue)}
              </text>
            </g>
          );
        })}
        {geometry ? (
          <>
            <polyline
              points={buildPolyline(geometry.coords)}
              fill="none"
              stroke={color}
              strokeWidth={1.6}
            />
            {hover ? (
              <g>
                <line
                  x1={hover.x}
                  x2={hover.x}
                  y1={PADDING_TOP}
                  y2={PADDING_TOP + plotHeight}
                  stroke="var(--color-border-strong)"
                  strokeDasharray="3 3"
                />
                <circle
                  cx={hover.x}
                  cy={PADDING_TOP + mapY(hover.point.v, scale.max, plotHeight)}
                  r={3}
                  fill={color}
                />
              </g>
            ) : null}
          </>
        ) : (
          <text
            x={PADDING_LEFT + plotWidth / 2}
            y={PADDING_TOP + plotHeight / 2}
            textAnchor="middle"
            fontSize={12}
            fill="var(--color-text-tertiary)"
          >
            点数不足，等待更多采集数据
          </text>
        )}
      </svg>
      {hover ? (
        <div
          data-testid="metric-chart-tooltip"
          style={{
            position: 'absolute',
            left: Math.min(Math.max(hover.x + 10, 4), Math.max(width - 150, 4)),
            top: PADDING_TOP,
            background: 'var(--color-bg-elevated)',
            // 七期走查：浅色下浮层底与卡片底同为 --color-bg-container，只有
            // 背景色时浮层边界完全消失（同底色叠同底色）。两主题同名变量
            // 分层：浅色靠 --color-shadow、深色靠 --color-border 描边
            // （8.3.7 的「不得用阴影代替描边」反之亦然——两者都给，任何
            // 主题都不会塌）。
            border: '1px solid var(--color-border)',
            boxShadow: 'var(--color-shadow)',
            borderRadius: 6,
            padding: '4px 8px',
            fontSize: 12,
            color: 'var(--color-text)',
            pointerEvents: 'none',
            whiteSpace: 'nowrap',
          }}
        >
          <div>{timeFormatter.format(new Date(hover.point.t))}</div>
          <div style={{ fontWeight: 600 }}>{formatValue(hover.point.v)}</div>
        </div>
      ) : null}
    </div>
  );
}

/** y 轴刻度短文案：bps 用紧凑格式，其他沿用 value 格式化。 */
function formatAxisValue(value: number, formatValue: (value: number) => string): string {
  const formatted = formatValue(value);
  if (formatted.length <= 8) {
    return formatted;
  }
  // 长文案（如 "1234.00 B/s"）压缩为 "1.2K" 风格。
  if (value >= 1_000_000_000) {
    return `${(value / 1_000_000_000).toFixed(1)}G`;
  }
  if (value >= 1_000_000) {
    return `${(value / 1_000_000).toFixed(1)}M`;
  }
  if (value >= 1_000) {
    return `${(value / 1_000).toFixed(1)}K`;
  }
  return String(Math.round(value));
}
