// 告警事件分页顺序（纯函数，node 环境直测）。
//
// 后端 GET /api/alert-events 固定 `ORDER BY fired_at DESC, id DESC`
// （internal/store/postgres/alert_events.go）。「加载更早事件」把当前页压入
// olderPages、游标推进到 next_before_id，因此 olderPages 天然是「新页 →
// 旧页」，每页内部也是降序；直接顺序展开即为全局降序。
//
// 走查第 2 轮：此前用 `[...olderPages.flat().reverse(), ...items].reverse()`
// 双重反转，旧页跑到最前且页内反而升序，时间线无法顺着排查。

export function mergeEventPageHistory<T>(
  olderPages: readonly (readonly T[])[],
  activePage: readonly T[],
): T[] {
  const merged: T[] = [];
  for (const page of olderPages) {
    for (const event of page) {
      merged.push(event);
    }
  }
  for (const event of activePage) {
    merged.push(event);
  }
  return merged;
}
