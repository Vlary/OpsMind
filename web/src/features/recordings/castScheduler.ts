// F1：asciicast 回放调度器（纯逻辑）。
//
// 给定按时间排序的事件序列与 sink（write/resize/reset），按相邻事件的时间
// 差 / 倍速 用 setTimeout 链调度；seek 先 reset 再同步快进重放已过事件。
// 定时器函数可注入，Vitest 假定时器直测（契约 9.4：只测纯逻辑）。

import type { CastEvent } from './castParser';

export interface CastSink {
  /** 输出事件：写入终端。 */
  write(data: string): void;
  /** resize 事件：调整终端尺寸。 */
  resize(cols: number, rows: number): void;
  /** 回到初始画面（seek 快进前的清屏）。 */
  reset(): void;
}

export interface CastSchedulerOptions {
  events: CastEvent[];
  sink: CastSink;
  /** 默认全局 setTimeout/clearTimeout；测试注入假实现。 */
  setTimeoutFn?: (handler: () => void, timeoutMs: number) => unknown;
  clearTimeoutFn?: (handle: unknown) => void;
  /** 默认 Date.now；测试注入受控时钟。 */
  nowFn?: () => number;
  /** 播放头推进（秒）；UI 借此刷新进度条。播放态下按墙钟插值连续回调。 */
  onTick?: (position: number, ended: boolean) => void;
}

type TimerHandle = unknown;

/**
 * 播放态插值心跳间隔（ms）：4 次/秒（走查 w2：事件间长静默段进度条与
 * 时间显示完全冻结，播放器看起来像卡死——真实终端录制中数秒静默极常见）。
 */
const TICK_INTERVAL_MS = 250;

export class CastScheduler {
  private readonly events: CastEvent[];
  private readonly sink: CastSink;
  private readonly setTimeoutFn: (handler: () => void, timeoutMs: number) => unknown;
  private readonly clearTimeoutFn: (handle: unknown) => void;
  private readonly nowFn: () => number;
  private readonly onTick: ((position: number, ended: boolean) => void) | undefined;

  private timer: TimerHandle = null;
  private tickTimer: TimerHandle = null;
  private index = 0;
  private position = 0;
  private speed = 1;
  private playing = false;
  private disposed = false;
  // 墙钟锚点：anchorWallMs 时刻的播放头位于 anchorPosition。每个事件的
  // 定时按「绝对 deadline - 已流逝墙钟」排——setTimeout 的实际触发延迟
  // （嵌套下限 ≥4ms、主线程繁忙）不会累积到后续事件（评审实测：纯
  // 理论间隔的 setTimeout 链在 200 个 1ms 事件上累积 5 倍墙钟偏差）。
  private anchorWallMs = 0;
  private anchorPosition = 0;

  constructor(options: CastSchedulerOptions) {
    this.events = options.events;
    this.sink = options.sink;
    this.setTimeoutFn = options.setTimeoutFn ?? ((handler, ms) => setTimeout(handler, ms));
    this.clearTimeoutFn = options.clearTimeoutFn ?? ((handle) => clearTimeout(handle as number));
    this.nowFn = options.nowFn ?? Date.now;
    this.onTick = options.onTick;
  }

  /** 播放头位置（秒）。 */
  get currentPosition(): number {
    return this.position;
  }

  get isPlaying(): boolean {
    return this.playing;
  }

  get currentSpeed(): number {
    return this.speed;
  }

  /** 事件总时长（最后一个事件的时间；进度条 max）。 */
  get duration(): number {
    const last = this.events[this.events.length - 1];
    return last ? last.time : 0;
  }

  play(): void {
    if (this.disposed || this.playing) {
      return;
    }
    if (this.index >= this.events.length) {
      return; // 已播完：由 UI 先 seek(0) 再 play（「重新播放」）
    }
    this.playing = true;
    this.reanchor();
    this.scheduleNext();
    this.startTickLoop();
  }

  pause(): void {
    if (this.disposed || !this.playing) {
      return;
    }
    // 先取插值位置再停（interpolatedPosition 在非播放态短路返回 position）。
    const pausedAt = this.interpolatedPosition();
    this.playing = false;
    this.cancelTimer();
    this.stopTickLoop();
    // 暂停位置对齐插值心跳最后一次推进的墙钟位置（进度条不回跳）。
    this.position = pausedAt;
  }

  setSpeed(speed: number): void {
    if (!Number.isFinite(speed) || speed <= 0) {
      return;
    }
    if (speed === this.speed) {
      return;
    }
    // 先按旧倍速把插值位置落定，再以新倍速重锚（否则换速瞬间跳变）。
    this.position = this.interpolatedPosition();
    this.speed = speed;
    if (this.playing) {
      // 以新倍速重排下一个事件（重锚后剩余等待时间按比例缩放）。
      this.cancelTimer();
      this.reanchor();
      this.scheduleNext();
    }
  }

  /**
   * 跳转：清定时器 → sink.reset() → 同步应用 t <= target 的全部事件
   * （快进）→ 播放头落 target；播放中则按新位置继续调度。
   */
  seek(target: number): void {
    if (this.disposed) {
      return;
    }
    const clamped = Math.max(0, Math.min(target, this.duration));
    this.cancelTimer();
    this.sink.reset();
    let index = 0;
    for (; index < this.events.length; index += 1) {
      const event = this.events[index];
      if (event === undefined || event.time > clamped) {
        break;
      }
      this.applyEvent(event);
    }
    this.index = index;
    this.position = clamped;
    if (this.playing) {
      if (this.index >= this.events.length) {
        this.playing = false;
        this.stopTickLoop();
        this.onTick?.(this.position, true);
      } else {
        this.reanchor();
        this.scheduleNext();
      }
    } else {
      this.onTick?.(this.position, this.index >= this.events.length);
    }
  }

  /** 停止并清定时器；幂等。之后 play/seek 均为 no-op。 */
  dispose(): void {
    this.disposed = true;
    this.playing = false;
    this.cancelTimer();
    this.stopTickLoop();
  }

  /** 重锚：把当前理论播放头与墙钟对齐（调度起点）。 */
  private reanchor(): void {
    this.anchorWallMs = this.nowFn();
    this.anchorPosition = this.position;
  }

  private scheduleNext(): void {
    const next = this.events[this.index];
    if (next === undefined) {
      this.playing = false;
      this.stopTickLoop();
      this.onTick?.(this.position, true);
      return;
    }
    // 绝对 deadline 调度：到该事件的理论剩余时间减去锚点以来已流逝的
    // 墙钟；前序定时器的触发延迟被自动扣除，不累积到后续事件。
    const remainingTheoryMs = ((next.time - this.anchorPosition) * 1000) / this.speed;
    const elapsedWallMs = this.nowFn() - this.anchorWallMs;
    const delayMs = Math.max(0, remainingTheoryMs - elapsedWallMs);
    this.timer = this.setTimeoutFn(() => {
      this.timer = null;
      if (!this.playing || this.disposed) {
        return;
      }
      // 按墙钟换算的理论应放位置批量应用事件：setTimeout 的触发下限
      // （嵌套 ≥4ms）让单个 tick 可能跨过多个密集事件，一次性补齐，
      // 而不是每个事件再等一轮下限（评审实测漂移 5 倍的根因之一）。
      const theoretical =
        this.anchorPosition + ((this.nowFn() - this.anchorWallMs) / 1000) * this.speed;
      let applied = 0;
      while (this.index < this.events.length) {
        const event = this.events[this.index];
        if (event === undefined || event.time > theoretical) {
          break;
        }
        this.position = event.time;
        this.applyEvent(event);
        this.index += 1;
        applied += 1;
      }
      if (applied === 0) {
        // 防御：deadline 计算偏差导致回调早于事件理论时间（不应发生）。
        this.position = Math.min(Math.max(theoretical, this.position), next.time);
      }
      if (this.index >= this.events.length) {
        this.playing = false;
        this.stopTickLoop();
        this.onTick?.(this.position, true);
        return;
      }
      this.onTick?.(this.position, false);
      this.reanchor();
      this.scheduleNext();
    }, delayMs);
  }

  // -------------------------------------------------------------------------
  // 播放态插值心跳（走查 w2）：onTick 原本只在事件落地时回调，两个事件
  // 之间的静默段（真实录制中数秒常见）进度条与时间完全冻结。心跳按
  // 「锚点墙钟换算」连续回调理论播放头，UI 无需改动即得连续进度。
  // -------------------------------------------------------------------------

  private startTickLoop(): void {
    this.stopTickLoop();
    this.scheduleTick();
  }

  private stopTickLoop(): void {
    if (this.tickTimer !== null) {
      this.clearTimeoutFn(this.tickTimer);
      this.tickTimer = null;
    }
  }

  private scheduleTick(): void {
    this.tickTimer = this.setTimeoutFn(() => {
      this.tickTimer = null;
      if (!this.playing || this.disposed) {
        return;
      }
      const position = this.interpolatedPosition();
      if (position > this.position) {
        this.position = position;
        this.onTick?.(this.position, false);
      }
      this.scheduleTick();
    }, TICK_INTERVAL_MS);
  }

  /**
   * 锚点墙钟换算的理论播放头：anchorPosition + elapsed × speed，封顶到
   * 下一个待落地事件与总时长（事件落地回调是权威推进点，插值不越权）。
   * 非播放态返回当前 position。
   */
  private interpolatedPosition(): number {
    if (!this.playing) {
      return this.position;
    }
    const theoretical =
      this.anchorPosition + ((this.nowFn() - this.anchorWallMs) / 1000) * this.speed;
    const next = this.events[this.index];
    let capped = Math.min(theoretical, this.duration);
    if (next !== undefined && capped > next.time) {
      capped = next.time;
    }
    return Math.max(this.position, capped);
  }

  private applyEvent(event: CastEvent): void {
    if (event.kind === 'o') {
      this.sink.write(event.data);
      return;
    }
    this.sink.resize(event.cols, event.rows);
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      this.clearTimeoutFn(this.timer);
      this.timer = null;
    }
  }
}
