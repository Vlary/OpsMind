// F1：RDP 会话控制器（guacamole-common-js 封装，纯逻辑可单测）。
//
// 组装链路（CONTRACT.md 6.5 / 8.7）：
//   Guacamole.WebSocketTunnel(/api/rdp/sessions/{id}/ws) → Guacamole.Client
//   → display canvas 挂容器 → Keyboard/Mouse 输入桥（onkeydown/onkeyup →
//   sendKeyEvent；onmousedown/onmousemove/onmouseup → sendMouseState，即
//   state 桥接）→ client.connect()。
//
// 契约 6.5：RDP WS 文本帧透传 guacamole 协议——官方 WebSocketTunnel
// 正是该协议的 WS 承载（oninstruction/sendInstruction 由 tunnel 内部
// 完成），前端无需自实现帧编解码；WebSocketTunnel 以子协议 "guacamole"
// 发起升级（服务端 upgrader 已声明同名子协议回显）。
//
// dispose 全量清理：client.disconnect()（内部 tunnel.disconnect() 关 WS）、
// keyboard/mouse 回调置空、display 元素移除；幂等。

import type { components } from '../../api/schema';

type RdpSession = components['schemas']['RdpSession'];

/** display 画布元素的最小能力（真实为 HTMLElement；测试用 fake）。 */
export interface CanvasElementLike {
  remove(): void;
}

/** 键鼠监听元素（真实为 HTMLElement；测试用 fake）。 */
export interface ListenerElementLike {
  addEventListener(type: string, listener: EventListener): void;
}

/** 注入面：生产传真实 guacamole-common-js 适配；单测传 fake。 */
export interface GuacamoleFactory {
  createTunnel(wsUrl: string): GuacamoleTunnel;
  createClient(tunnel: GuacamoleTunnel): GuacamoleClient;
  createKeyboard(element: ListenerElementLike): GuacamoleKeyboard;
  createMouse(element: CanvasElementLike): GuacamoleMouse;
}

export interface GuacamoleTunnel {
  connect(data?: string): void;
  disconnect(): void;
  /** 隧道层错误（WS 握手失败/服务端超时/上游不可达；官方 Client 不传导）。 */
  onerror: ((status: { code: number; message: string }) => void) | null;
  /** 隧道状态变化（CONNECTING/OPEN/CLOSED/UNSTABLE；官方 Client 不传导）。 */
  onstatechange: ((state: number) => void) | null;
}

export interface GuacamoleDisplay {
  getElement(): CanvasElementLike;
  getWidth(): number;
  getHeight(): number;
  scale(factor: number): void;
}

export interface GuacamoleClient {
  connect(data?: string): void;
  disconnect(): void;
  getDisplay(): GuacamoleDisplay;
  sendKeyEvent(pressed: boolean, keysym: number): void;
  sendMouseState(state: GuacamoleMouseState, applyDisplayScale?: boolean): void;
  createClipboardStream(mimeType: string): { writeBlob(blob: Blob): void };
  onstatechange: ((state: number) => void) | null;
  onerror: ((status: { code: number; message: string }) => void) | null;
  onclipboard:
    | ((
        stream: { onblob: ((base64: string) => void) | null; onend: (() => void) | null },
        mimeType: string,
      ) => void)
    | null;
}

export interface GuacamoleKeyboard {
  onkeydown: ((keysym: number) => boolean) | null;
  onkeyup: ((keysym: number) => boolean) | null;
}

export interface GuacamoleMouse {
  onmousedown: ((state: GuacamoleMouseState) => boolean) | null;
  onmousemove: ((state: GuacamoleMouseState) => boolean) | null;
  onmouseup: ((state: GuacamoleMouseState) => boolean) | null;
}

export interface GuacamoleMouseState {
  x: number;
  y: number;
  left: boolean;
  middle: boolean;
  right: boolean;
}

/** Client.State.* 常量（guacamole-common-js 1.5.0）。 */
export const GUAC_CLIENT_STATE = {
  IDLE: 0,
  CONNECTING: 1,
  WAITING: 2,
  CONNECTED: 3,
  DISCONNECTING: 4,
  DISCONNECTED: 5,
} as const;

/** Tunnel.State.* 常量（guacamole-common-js 1.5.0）。 */
export const GUAC_TUNNEL_STATE = {
  CONNECTING: 0,
  OPEN: 1,
  CLOSED: 2,
  UNSTABLE: 3,
} as const;

/**
 * 隧道层（WS 握手/超时）失败的兜底文案（走查 w5b：握手失败不传导到
 * client.onerror，页面永久停在「正在建立 RDP 连接」）。
 */
export const TUNNEL_FAILURE_MESSAGE = '网关连接失败（服务端未响应）';

export type RdpPhase = 'connecting' | 'connected' | 'waiting' | 'disconnected' | 'failed';

export interface RdpSessionState {
  session: RdpSession;
  phase: RdpPhase;
  /** 失败/断开的可读原因（onerror message 或 null）。 */
  error: string | null;
  /** 最近一次收到的远端剪贴板文本。 */
  remoteClipboard: string | null;
}

export type RdpStateListener = (state: RdpSessionState) => void;

export interface RdpSessionControllerOptions {
  session: RdpSession;
  /** ws_url（相对路径 /api/rdp/sessions/{id}/ws），WebSocketTunnel 直接消费。 */
  wsUrl: string;
  /** display canvas 挂载回调（真实实现 append 到 stage；测试注入 fake）。 */
  mountCanvas: (canvas: CanvasElementLike) => void;
  /** 键盘输入监听元素（聚焦后接收 keydown/keyup）。 */
  keyboardElement: ListenerElementLike;
  factory: GuacamoleFactory;
  onState?: RdpStateListener;
}

export function rdpWsUrl(sessionId: string): string {
  return `/api/rdp/sessions/${encodeURIComponent(sessionId)}/ws`;
}

export class RdpSessionController {
  private state: RdpSessionState;
  private readonly listeners = new Set<RdpStateListener>();
  private readonly client: GuacamoleClient;
  private readonly tunnel: GuacamoleTunnel;
  private readonly keyboard: GuacamoleKeyboard;
  private readonly mouse: GuacamoleMouse;
  private readonly display: GuacamoleDisplay;
  private disposed = false;
  private disconnectedByUser = false;

  constructor(options: RdpSessionControllerOptions) {
    this.state = {
      session: options.session,
      phase: 'connecting',
      error: null,
      remoteClipboard: null,
    };
    if (options.onState) {
      this.listeners.add(options.onState);
    }

    this.tunnel = options.factory.createTunnel(options.wsUrl);
    this.client = options.factory.createClient(this.tunnel);
    this.display = this.client.getDisplay();

    // 隧道层失败兜底（走查 w5b）：官方 Client 只挂 tunnel.oninstruction，
    // WS 握手失败/服务端超时只触发 tunnel.onerror/onstatechange——不挂则
    // phase 永远停在 connecting，页面无任何失败提示。
    this.tunnel.onerror = (status) => this.handleTunnelError(status);
    this.tunnel.onstatechange = (state) => this.handleTunnelStateChange(state);

    this.client.onstatechange = (state) => this.handleStateChange(state);
    this.client.onerror = (status) => {
      this.updateState({
        phase: 'failed',
        error: status.message || `guacamole error ${status.code}`,
      });
    };
    this.client.onclipboard = (stream, mimeType) => this.receiveClipboard(stream, mimeType);

    // 输入桥：Keyboard/Mouse 的 state 回调 → client 发送（任务：桥接
    // oninstruction/sendState；oninstruction 由官方 tunnel 完成，这里补
    // 键鼠 state 侧）。返回 true 交由 guacamole preventDefault。
    this.keyboard = options.factory.createKeyboard(options.keyboardElement);
    this.keyboard.onkeydown = (keysym) => {
      this.client.sendKeyEvent(true, keysym);
      return true;
    };
    this.keyboard.onkeyup = (keysym) => {
      this.client.sendKeyEvent(false, keysym);
      return true;
    };
    this.mouse = options.factory.createMouse(this.display.getElement());
    const forwardMouse = (state: GuacamoleMouseState) => {
      // display 已 scale：按显示比例换算回远端坐标（1.5.0 第二参）。
      this.client.sendMouseState(state, true);
      return true;
    };
    this.mouse.onmousedown = forwardMouse;
    this.mouse.onmousemove = forwardMouse;
    this.mouse.onmouseup = forwardMouse;

    options.mountCanvas(this.display.getElement());
    // connect 传空串：WebSocketTunnel 把 data 拼进 URL（url + "?" + data），
    // 缺省 undefined 会让真实 WS 地址带 "?undefined" 垃圾参数（走查实测）。
    // 凭据在网关侧 guacd 握手注入（契约 5.14），无需连接数据。
    this.client.connect('');
  }

  getState(): RdpSessionState {
    return this.state;
  }

  subscribe(listener: RdpStateListener): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /** 发送本地文本到远端剪贴板（text/plain 流）。 */
  sendClipboard(text: string): void {
    if (this.disposed || text === '') {
      return;
    }
    this.client.createClipboardStream('text/plain').writeBlob(new Blob([text]));
  }

  /** 容器尺寸变化时自适应缩放（RDP 分辨率固定，不 resize 远端）。 */
  applyScale(containerWidth: number, containerHeight: number): void {
    if (this.disposed || containerWidth <= 0 || containerHeight <= 0) {
      return;
    }
    const width = this.display.getWidth();
    const height = this.display.getHeight();
    if (width <= 0 || height <= 0) {
      return;
    }
    const factor = Math.min(containerWidth / width, containerHeight / height);
    if (Number.isFinite(factor) && factor > 0) {
      this.display.scale(factor);
    }
  }

  /** 用户点击「断开」。 */
  disconnectByUser(): void {
    if (this.disposed) {
      return;
    }
    this.disconnectedByUser = true;
    this.client.disconnect();
  }

  /** 全量清理：client/tunnel（WS）、键鼠回调、display DOM；幂等。 */
  dispose(): void {
    if (this.disposed) {
      return;
    }
    this.disposed = true;
    this.keyboard.onkeydown = null;
    this.keyboard.onkeyup = null;
    this.mouse.onmousedown = null;
    this.mouse.onmousemove = null;
    this.mouse.onmouseup = null;
    this.client.onerror = null;
    this.client.onstatechange = null;
    this.client.onclipboard = null;
    this.tunnel.onerror = null;
    this.tunnel.onstatechange = null;
    // disconnect 会向 tunnel 传播（官方实现内部 tunnel.disconnect() 关 WS）。
    this.client.disconnect();
    const element = this.display.getElement();
    element.remove();
    this.listeners.clear();
  }

  wasDisconnectedByUser(): boolean {
    return this.disconnectedByUser;
  }

  /**
   * 隧道层错误（WS 握手失败/服务端超时/上游不可达）：官方 Client 只挂
   * tunnel.oninstruction，onerror 不会传导到 client.onerror——此处直接
   * 置 failed 并透出底层 message（用户主动断开后的回环错误忽略）。
   */
  private handleTunnelError(status: { code: number; message: string }): void {
    if (this.disposed || this.disconnectedByUser) {
      return;
    }
    this.updateState({
      phase: 'failed',
      error: status.message || `tunnel error ${status.code}`,
    });
  }

  /**
   * 隧道状态兜底：CLOSED 且从未 connected 时（服务端直接关 WS 且无错误
   * 状态——如 guacd 拨号失败后网关以 NormalClosure 关连接，1000 映射
   * SUCCESS 不触发 onerror）也置 failed，避免 phase 永远停在 connecting；
   * 已 connected 后被服务端断开则按 disconnected 呈现。UNSTABLE（1.5s
   * 无数据）在连接期映射为 waiting，给出「等待远端桌面响应」的可见反馈。
   * 用户主动断开（disconnect 内 close_tunnel(SUCCESS) 只走状态不走错误）
   * 由 disconnectedByUser 挡回。
   */
  private handleTunnelStateChange(state: number): void {
    if (this.disposed || this.disconnectedByUser) {
      return;
    }
    if (state === GUAC_TUNNEL_STATE.CLOSED) {
      if (this.state.phase === 'failed' || this.state.phase === 'disconnected') {
        return;
      }
      if (this.state.phase === 'connected') {
        this.updateState({ phase: 'disconnected', error: this.state.error });
        return;
      }
      this.updateState({ phase: 'failed', error: TUNNEL_FAILURE_MESSAGE });
      return;
    }
    if (state === GUAC_TUNNEL_STATE.UNSTABLE && this.state.phase === 'connecting') {
      this.updateState({ phase: 'waiting' });
    }
  }

  private handleStateChange(state: number): void {
    if (this.disposed) {
      return;
    }
    switch (state) {
      case GUAC_CLIENT_STATE.IDLE:
      case GUAC_CLIENT_STATE.DISCONNECTED:
        this.updateState({
          phase: 'disconnected',
          error: this.state.error,
        });
        return;
      case GUAC_CLIENT_STATE.CONNECTING:
        this.updateState({ phase: 'connecting' });
        return;
      case GUAC_CLIENT_STATE.WAITING:
        this.updateState({ phase: 'waiting' });
        return;
      case GUAC_CLIENT_STATE.CONNECTED:
        this.updateState({ phase: 'connected' });
        return;
      default:
        return;
    }
  }

  private receiveClipboard(
    stream: { onblob: ((base64: string) => void) | null; onend: (() => void) | null },
    mimeType: string,
  ): void {
    if (this.disposed || !mimeType.startsWith('text/')) {
      return;
    }
    // blob 为 UTF-8 字节的 base64：分片累积，结束后一次性解码（剪贴板
    // 文本量小，合并成本可忽略；分段 decode 需维护跨片状态，不值得）。
    const chunks: Uint8Array[] = [];
    stream.onblob = (base64) => {
      const binary = atob(base64);
      const bytes = new Uint8Array(binary.length);
      for (let index = 0; index < binary.length; index += 1) {
        bytes[index] = binary.charCodeAt(index);
      }
      chunks.push(bytes);
    };
    stream.onend = () => {
      const total = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
      const merged = new Uint8Array(total);
      let offset = 0;
      for (const chunk of chunks) {
        merged.set(chunk, offset);
        offset += chunk.length;
      }
      this.updateState({ remoteClipboard: new TextDecoder('utf-8').decode(merged) });
    };
  }

  private updateState(patch: Partial<RdpSessionState>): void {
    this.state = { ...this.state, ...patch };
    for (const listener of this.listeners) {
      listener(this.state);
    }
  }
}
