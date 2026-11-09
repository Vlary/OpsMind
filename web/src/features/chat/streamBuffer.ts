/**
 * 流式 token 缓冲：token 只写入字符串缓冲，最多每帧（或 50ms）回调一次。
 *
 * - 合并同一帧内的所有 append；
 * - 下一帧渲染（requestAnimationFrame）；
 * - 页面隐藏 / 无 RAF 时由 50ms setTimeout 兜底；
 * - flush 可随时强制同步输出（工具、审批、done 事件前必须调用）。
 */

type TimeoutHandle = ReturnType<typeof setTimeout>;

export interface StreamBufferOptions {
  onFlush: (text: string) => void;
  /** 默认用 requestAnimationFrame；没有时退化为 16ms setTimeout。 */
  raf?: (callback: () => void) => number;
  cancelRaf?: (handle: number) => void;
  setTimeoutFn?: (callback: () => void, ms: number) => TimeoutHandle;
  clearTimeoutFn?: (handle: TimeoutHandle) => void;
  fallbackMs?: number;
}

const defaultRaf: (callback: () => void) => number =
  typeof requestAnimationFrame === 'function'
    ? (callback) => requestAnimationFrame(callback)
    : (callback) => Number(setTimeout(callback, 16));

const defaultCancelRaf: (handle: number) => void =
  typeof cancelAnimationFrame === 'function'
    ? (handle) => cancelAnimationFrame(handle)
    : (handle) => clearTimeout(handle);

export class StreamBuffer {
  private buffer = '';
  private scheduled = false;
  private rafHandle: number | null = null;
  private timeoutHandle: TimeoutHandle | null = null;

  private readonly onFlush: (text: string) => void;
  private readonly raf: (callback: () => void) => number;
  private readonly cancelRaf: (handle: number) => void;
  private readonly setTimeoutFn: (callback: () => void, ms: number) => TimeoutHandle;
  private readonly clearTimeoutFn: (handle: TimeoutHandle) => void;
  private readonly fallbackMs: number;

  constructor(options: StreamBufferOptions) {
    this.onFlush = options.onFlush;
    this.raf = options.raf ?? defaultRaf;
    this.cancelRaf = options.cancelRaf ?? defaultCancelRaf;
    this.setTimeoutFn = options.setTimeoutFn ?? ((callback, ms) => setTimeout(callback, ms));
    this.clearTimeoutFn = options.clearTimeoutFn ?? ((handle) => clearTimeout(handle));
    this.fallbackMs = options.fallbackMs ?? 50;
  }

  /** 追加 token；同一帧内多次调用只触发一次 flush。 */
  append(delta: string): void {
    if (!delta) {
      return;
    }
    this.buffer += delta;
    this.schedule();
  }

  /** 立即输出缓冲内容并取消已排队的刷新。 */
  flush(): void {
    this.cancelScheduled();
    const text = this.buffer;
    this.buffer = '';
    if (text) {
      this.onFlush(text);
    }
  }

  /** 丢弃未输出的内容并清理定时器（卸载时调用）。 */
  dispose(): void {
    this.cancelScheduled();
    this.buffer = '';
  }

  get pendingLength(): number {
    return this.buffer.length;
  }

  private schedule(): void {
    if (this.scheduled) {
      return;
    }
    this.scheduled = true;
    this.rafHandle = this.raf(() => this.flush());
    this.timeoutHandle = this.setTimeoutFn(() => this.flush(), this.fallbackMs);
  }

  private cancelScheduled(): void {
    this.scheduled = false;
    if (this.rafHandle !== null) {
      this.cancelRaf(this.rafHandle);
      this.rafHandle = null;
    }
    if (this.timeoutHandle !== null) {
      this.clearTimeoutFn(this.timeoutHandle);
      this.timeoutHandle = null;
    }
  }
}
