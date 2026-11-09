// F1：终端实时会话控制器（纯逻辑，React 组件之外便于 Vitest 直测）。
//
// 职责：把 TermSocket（api/ws.ts）与 xterm 终端实例的双向数据流、
// resize 上报与会话状态机组装起来；TermLive.tsx 只负责 DOM 挂载与
// fit 计算，卸载时统一 dispose（WS/xterm/定时器全量清理，契约 6.4）。
//
// 通过窄接口注入 socket 与终端实例：单测在 node 环境用 fake 实现，
// 不依赖 DOM 与真实 WebSocket。

import type { TermServerFrame, TermSocket, TermSocketStatus } from '../../api/ws';

/** 终端实例的最小接口（@xterm/xterm Terminal 的结构子类型）。 */
export interface TerminalLike {
  write(data: string | Uint8Array): void;
  resize(cols: number, rows: number): void;
  onData(handler: (data: string) => void): { dispose(): void };
  reset(): void;
}

/** 服务端 closed 帧的 reason（契约 6.4）。 */
export type TermClosedReason =
  'client_closed' | 'idle_timeout' | 'max_duration' | 'server_shutdown' | 'error' | (string & {});

export interface TermSessionState {
  /** socket 连接状态。 */
  socket: TermSocketStatus;
  /** 服务端 error 帧内容；连接层失败由 socket=closed + closeHint 表达。 */
  errorCode: string | null;
  errorMessage: string | null;
  /** 服务端 closed 帧原因；契约 6.4：收到后引导新建会话，不自动重连。 */
  closedReason: TermClosedReason | null;
  /** WS 意外断开（未收到 closed 帧）时为 true：提示异常断开。 */
  abnormalClose: boolean;
}

export type TermSessionStateListener = (state: TermSessionState) => void;

/** resize 上报的最小间隔（毫秒）：避免拖拽窗口期间帧风暴。 */
const RESIZE_REPORT_MIN_INTERVAL_MS = 200;

/**
 * 服务端合法窗口范围（契约 5.13：resize 越界回 VALIDATION_FAILED 错误
 * 帧且不生效）。上报前在客户端 clamp 到该范围——fit 在抽屉动画等瞬态
 * 布局下可能算出 2 列之类的越界值（走查实测首屏偶发红色
 * VALIDATION_FAILED 横幅即由此来），clamp 后不再骚扰用户。
 */
export const TERM_MIN_COLS = 10;
export const TERM_MAX_COLS = 500;
export const TERM_MIN_ROWS = 4;
export const TERM_MAX_ROWS = 200;

/** 把 fit 结果 clamp 到服务端合法范围；非法输入（非有限数）返回 null。 */
export function clampWindowSize(cols: number, rows: number): { cols: number; rows: number } | null {
  if (!Number.isFinite(cols) || !Number.isFinite(rows)) {
    return null;
  }
  const clamp = (value: number, min: number, max: number) =>
    Math.min(max, Math.max(min, Math.round(value)));
  return {
    cols: clamp(cols, TERM_MIN_COLS, TERM_MAX_COLS),
    rows: clamp(rows, TERM_MIN_ROWS, TERM_MAX_ROWS),
  };
}

export interface TermSessionOptions {
  terminal: TerminalLike;
  /** 建立连接的工厂：测试注入 fake，生产传 openTermSocket 包装。 */
  connect: (handlers: {
    onStatus: (status: TermSocketStatus) => void;
    onFrame: (frame: TermServerFrame) => void;
  }) => TermSocket;
  onState?: TermSessionStateListener;
  now?: () => number;
}

export class TermSessionController {
  private readonly terminal: TerminalLike;
  private readonly socket: TermSocket;
  private readonly listeners = new Set<TermSessionStateListener>();
  private readonly dataSubscription: { dispose(): void };
  private readonly now: () => number;
  private state: TermSessionState = {
    socket: 'connecting',
    errorCode: null,
    errorMessage: null,
    closedReason: null,
    abnormalClose: false,
  };
  private lastResizeReportAt = 0;
  private lastReportedCols = 0;
  private lastReportedRows = 0;
  private disposed = false;

  constructor(options: TermSessionOptions) {
    this.terminal = options.terminal;
    this.now = options.now ?? Date.now;
    if (options.onState) {
      this.listeners.add(options.onState);
    }
    this.socket = options.connect({
      onStatus: (status) => this.handleStatus(status),
      onFrame: (frame) => this.handleFrame(frame),
    });
    this.dataSubscription = this.terminal.onData((data) => {
      this.sendInput(data);
    });
  }

  getState(): TermSessionState {
    return this.state;
  }

  subscribe(listener: TermSessionStateListener): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /** xterm 用户输入 → input 帧（base64 由 ws 层处理）。 */
  sendInput(text: string): void {
    if (this.disposed || this.state.closedReason !== null) {
      return;
    }
    const bytes = new TextEncoder().encode(text);
    this.socket.sendInput(bytes);
  }

  /**
   * fit 计算出新的 cols/rows 后上报；服务端回 resized 帧时本地同步生效。
   * 上报前 clamp 到服务端合法范围 [10,500]×[4,200]（契约 5.13），越界值
   * 会被服务端以 VALIDATION_FAILED 错误帧拒绝且不生效。
   * 节流：拖拽窗口期间至多每 200ms 一帧（leading）；节流窗口内丢弃的值由
   * trailing 定时器在窗口结束时补发最终尺寸——否则拖拽结束的最后一次
   * fit 落在窗口内被永久丢弃，服务端 PTY 停留在旧尺寸而本地 xterm 已是
   * 新尺寸，输出换行错位（评审发现）。数值未变不上报。
   */
  reportResize(rawCols: number, rawRows: number): void {
    if (this.disposed) {
      return;
    }
    const clamped = clampWindowSize(rawCols, rawRows);
    if (clamped === null || clamped.cols <= 0 || clamped.rows <= 0) {
      return;
    }
    const { cols, rows } = clamped;
    if (cols === this.lastReportedCols && rows === this.lastReportedRows) {
      return;
    }
    const timestamp = this.now();
    if (timestamp - this.lastResizeReportAt >= RESIZE_REPORT_MIN_INTERVAL_MS) {
      this.clearTrailingResize();
      this.lastResizeReportAt = timestamp;
      this.lastReportedCols = cols;
      this.lastReportedRows = rows;
      this.socket.sendResize(cols, rows);
      return;
    }
    // 节流窗口内：暂存最新尺寸，窗口结束时补发（不断刷新定时器会让补发
    // 无限推迟，固定 deadline 只推迟到窗口末尾）。
    this.pendingResize = { cols, rows };
    if (this.trailingResizeTimer === null) {
      const wait = RESIZE_REPORT_MIN_INTERVAL_MS - (timestamp - this.lastResizeReportAt);
      this.trailingResizeTimer = setTimeout(
        () => {
          this.trailingResizeTimer = null;
          const pending = this.pendingResize;
          this.pendingResize = null;
          if (!pending || this.disposed) {
            return;
          }
          this.lastResizeReportAt = this.now();
          this.lastReportedCols = pending.cols;
          this.lastReportedRows = pending.rows;
          this.socket.sendResize(pending.cols, pending.rows);
        },
        Math.max(wait, 0),
      );
    }
  }

  private pendingResize: { cols: number; rows: number } | null = null;
  private trailingResizeTimer: ReturnType<typeof setTimeout> | null = null;

  private clearTrailingResize(): void {
    if (this.trailingResizeTimer !== null) {
      clearTimeout(this.trailingResizeTimer);
      this.trailingResizeTimer = null;
    }
    this.pendingResize = null;
  }

  /** 用户点击「断开」：本地立即收尾，随后 socket 关闭由服务端 closed 帧或断开完成。 */
  closeByUser(): void {
    if (this.disposed) {
      return;
    }
    this.socket.dispose();
    this.updateState({ closedReason: 'client_closed', abnormalClose: false });
  }

  /** 全量清理：WS、xterm 订阅、监听器与 trailing 定时器；幂等。 */
  dispose(): void {
    if (this.disposed) {
      return;
    }
    this.disposed = true;
    this.clearTrailingResize();
    this.dataSubscription.dispose();
    this.socket.dispose();
    this.listeners.clear();
  }

  private handleStatus(status: TermSocketStatus): void {
    if (this.disposed) {
      return;
    }
    if (status === 'closed' && this.state.closedReason === null) {
      // 未收到服务端 closed 帧的断开：网络/代理异常（契约 6.4 不自动重连）。
      this.updateState({ socket: status, abnormalClose: true });
      return;
    }
    this.updateState({ socket: status });
  }

  private handleFrame(frame: TermServerFrame): void {
    if (this.disposed) {
      return;
    }
    switch (frame.type) {
      case 'output':
        this.terminal.write(frame.data);
        return;
      case 'resized':
        // 服务端确认的最终值回写本地（PTY 可能 clamp 到合法范围）；
        // 待补发值与确认一致时无需再发。
        if (
          this.pendingResize !== null &&
          this.pendingResize.cols === frame.cols &&
          this.pendingResize.rows === frame.rows
        ) {
          this.clearTrailingResize();
        }
        this.lastReportedCols = frame.cols;
        this.lastReportedRows = frame.rows;
        this.terminal.resize(frame.cols, frame.rows);
        return;
      case 'error':
        this.updateState({ errorCode: frame.code, errorMessage: frame.message });
        return;
      case 'closed':
        this.updateState({ closedReason: frame.reason, abnormalClose: false });
        return;
    }
  }

  private updateState(patch: Partial<TermSessionState>): void {
    this.state = { ...this.state, ...patch };
    for (const listener of this.listeners) {
      listener(this.state);
    }
  }
}
