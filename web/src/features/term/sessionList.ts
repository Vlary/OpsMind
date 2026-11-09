// F1：会话列表断开后的乐观更新（p2 sprint 4，终端与 RDP 共用）。
//
// 缺陷形态（实测 w4-rdp2，终端同）：断开后页头已提示「已断开/会话已结束」，
// 服务端的收尾却是异步的（termx 封盘+关行 / rdpx bridge 停止+封盘+关行，
// rdpx 日志 "bridge goroutines did not stop in time"，实测约 2.3s）。此时
// 立即 invalidateQueries 触发的 refetch 拿回的仍是 active 行，会话列表
// 「进行中/录制中…」会与页头矛盾地残留 2-3 秒。
//
// 修复：会话结束回调里先 setQueryData 乐观把当前行置 closed（与页头立即
// 一致），再延迟到服务端收尾完成后 invalidate 拉取真实数据（bytes_written
// / has_recording / ended_at）。

/** 会话行的最小结构（TerminalSession 与 RdpSession 均满足）。 */
export interface SessionRowLike {
  id: string;
  status: string;
  ended_at: string | null;
  bytes_written?: number;
}

/**
 * 乐观结束行 bytes_written 的占位值（p2 sprint 5）：断开瞬间缓存里的
 * 旧值是建会话列表拉取时的 0，直接渲染会显示误导性的「0 B」——用户
 * 误以为没录上（走查实测 2-3 秒后才被真实大小覆盖）。渲染层据此显示
 * 「封盘中…」，延迟 invalidate 拉回真实大小后自然衔接。
 */
export const BYTES_FINALIZING = -1;

/**
 * 延迟失效间隔：须 ≥ 服务端收尾实测耗时（rdpx bridgeStopTimeout 2s + 关行
 * ≈2.3s）再 refetch，否则拿回 active 行覆盖乐观值、矛盾重现；也不宜过长，
 * 否则录制大小/has_recording 长时间为占位值。
 */
export const SESSION_END_REFETCH_DELAY_MS = 3000;

/**
 * 乐观把列表缓存中指定会话行置为已结束（status=closed、ended_at=调用方
 * 给定的 ISO 时刻；bytes_written 置为 BYTES_FINALIZING 占位——封盘未完成，
 * 真实大小未知），其余行原样保留；不修改入参数组（React Query 缓存
 * 不可变更新）。
 */
export function markSessionEnded<T extends SessionRowLike>(
  sessions: readonly T[],
  sessionId: string,
  endedAtIso: string,
): T[] {
  return sessions.map((session) =>
    session.id === sessionId
      ? ({
          ...session,
          status: 'closed',
          ended_at: endedAtIso,
          bytes_written: BYTES_FINALIZING,
        } as T)
      : session,
  );
}
