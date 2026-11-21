// 图表 hover/focus 浮层。两主题都分层：浅色靠 --color-shadow，深色靠
// --color-bg-elevated + 描边（CONTRACT.md 8.7 监控页浮层边界要求）。
// 位置由调用方给出图内坐标，组件负责在容器内做左右钳制，避免越界被裁。

import type { ReactNode } from 'react';

export interface ChartTooltipProps {
  /** 图内 x 坐标（相对容器左缘）。 */
  x: number;
  /** 容器宽度，用于右侧钳制。 */
  containerWidth: number;
  /** 距容器顶部的偏移，默认 8。 */
  top?: number;
  children: ReactNode;
  testId?: string;
}

const TOOLTIP_WIDTH = 220;
const EDGE_GAP = 8;

export default function ChartTooltip({
  x,
  containerWidth,
  top = 8,
  children,
  testId,
}: ChartTooltipProps) {
  const maxLeft = Math.max(EDGE_GAP, containerWidth - TOOLTIP_WIDTH - EDGE_GAP);
  const left = Math.min(Math.max(x + 12, EDGE_GAP), maxLeft);
  return (
    <div
      data-testid={testId}
      aria-hidden="true"
      style={{
        position: 'absolute',
        left,
        top,
        width: TOOLTIP_WIDTH,
        maxWidth: 'calc(100% - 16px)',
        background: 'var(--color-bg-elevated)',
        border: '1px solid var(--color-border)',
        boxShadow: 'var(--color-shadow)',
        borderRadius: 6,
        padding: '6px 8px',
        fontSize: 12,
        lineHeight: 1.55,
        color: 'var(--color-text)',
        pointerEvents: 'none',
        zIndex: 2,
        wordBreak: 'break-word',
      }}
    >
      {children}
    </div>
  );
}
