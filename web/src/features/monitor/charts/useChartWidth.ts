// 图表容器宽度自适应：ResizeObserver 在挂载时测量、尺寸变化时更新，
// 卸载即 disconnect（与 MetricChart 相同做法，避免监听泄漏）。
// 首帧宽度为 0 时由调用方用 fallback 宽度绘制，避免图表闪一下空白。

import { useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';

export function useChartWidth(): [RefObject<HTMLDivElement | null>, number] {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(0);

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

  return [containerRef, width];
}
