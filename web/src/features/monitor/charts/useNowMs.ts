// 图表内「当前时刻」的唯一来源（走查第 14 轮 low）：
// 未走完的小时/6 小时桶要按 now 判定「进行中」。生产可由页面注入服务端
// 时间（overview.server_time 偏移），测试可注入固定时钟；都没有时每 60s
// 走一次浏览器时钟。定时器与初始 timeout 在卸载时清理，组件渲染期不
// 直接调用 Date.now（与 TopServerBarChart 的既有约束一致）。
import { useEffect, useState } from 'react';

export function useNowMs(fixed?: number, intervalMs = 60_000): number {
  const [clock, setClock] = useState(0);
  useEffect(() => {
    if (fixed !== undefined) {
      return;
    }
    const tick = () => {
      setClock(Date.now());
    };
    const initial = setTimeout(tick, 0);
    const timer = setInterval(tick, intervalMs);
    return () => {
      clearTimeout(initial);
      clearInterval(timer);
    };
  }, [fixed, intervalMs]);
  return fixed ?? clock;
}
