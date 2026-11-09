// F1：终端/回放/RDP 页共用的纯展示格式化函数（字节、时钟）。

/**
 * 字节 → 人类可读大小。
 *
 * unitBase 缺省 1024（KiB 语义，终端流量/回放体积等自产数据）；容器镜像
 * 大小传 1000——后端 parseHumanSize 按 SI 单位解析 docker CLI 的 human
 * size（412MB = 412,000,000B），1024 进制会渲染成「393 MB」，同一镜像
 * 与 docker images 输出数字对不上（四期走查 T6）。
 */
export function formatBytes(bytes: number | null | undefined, unitBase = 1024): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes) || bytes < 0) {
    return '—';
  }
  if (bytes < unitBase) {
    return `${bytes} B`;
  }
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unitIndex = -1;
  do {
    value /= unitBase;
    unitIndex += 1;
  } while (value >= unitBase && unitIndex < units.length - 1);
  const digits = value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${units[unitIndex] ?? 'PB'}`;
}

/** 秒（float）→ m:ss / h:mm:ss（asciicast 时间轴显示）。 */
export function formatClockSeconds(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  const two = (value: number) => String(value).padStart(2, '0');
  return hours > 0 ? `${hours}:${two(minutes)}:${two(secs)}` : `${minutes}:${two(secs)}`;
}

/** 毫秒 → mm:ss / h:mm:ss（会话/录制时长显示）。 */
export function formatDurationClock(milliseconds: number): string {
  return formatClockSeconds(milliseconds / 1000);
}

/** 会话进行时长：started_at → nowMs（TermLive 页头每秒刷新）。 */
export function formatElapsed(fromIso: string, nowMs: number): string {
  const started = new Date(fromIso).getTime();
  if (Number.isNaN(started)) {
    return '--:--';
  }
  return formatDurationClock(Math.max(0, nowMs - started));
}

/** 录制时长：ended_at - started_at（回放/列表列）。 */
export function recordingDurationMs(recording: {
  started_at: string;
  ended_at: string;
}): number | null {
  const start = new Date(recording.started_at).getTime();
  const end = new Date(recording.ended_at).getTime();
  if (Number.isNaN(start) || Number.isNaN(end)) {
    return null;
  }
  return Math.max(0, end - start);
}
