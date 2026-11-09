// F1：终端 WebSocket 客户端（CONTRACT.md 6.4 / 8.4）。
//
// - 连接 /api/term/sessions/{id}/ws，子协议必须是 opsmind-term-v1
//   （缺失/不匹配服务端 400 TERM_WS_PROTOCOL）。
// - 帧为 WS 文本帧 + 单行 JSON；终端 I/O 的 data 字段是原始字节的
//   base64（StdEncoding），不能按字符串直传。
// - 服务端每 30s 发 WS ping 控制帧，浏览器自动回 pong，前端无需处理。
// - CONNECTING 期间发送的帧排队（resize 只保留最新值），onopen 后按序
//   flush——挂载即上报的首次 resize 不再被静默丢弃（p2 sprint 5）。
// - RDP 的 WS 不经本文件（契约 8.4）：由 guacamole-common-js 的
//   WebSocketTunnel 直连 /api/rdp/sessions/{id}/ws。

export const TERM_WS_PROTOCOL = 'opsmind-term-v1';

/** 单帧 input 解码后上限 8KB（契约 5.13：超限服务端断开 COMMAND_TOO_LARGE）。 */
export const TERM_INPUT_MAX_BYTES = 8 * 1024;

/** 服务端 → 客户端帧（窄化后的联合类型，与 schema TermFrame* 对齐）。 */
export type TermServerFrame =
  | { type: 'output'; data: Uint8Array }
  | { type: 'resized'; cols: number; rows: number }
  | { type: 'error'; code: string; message: string }
  | { type: 'closed'; reason: string };

/** 客户端 → 服务端帧载荷（序列化前的对象形态）。 */
export interface TermClientInputFrame {
  type: 'input';
  data: string;
}

export interface TermClientResizeFrame {
  type: 'resize';
  cols: number;
  rows: number;
}

export type TermClientFrame = TermClientInputFrame | TermClientResizeFrame;

// 浏览器 btoa/atob 的字节安全包装（分块避免 call stack 上限）。
const BTOA_CHUNK = 0x8000;

/**
 * 把输入字节按单帧上限切块（契约 5.13：单帧 input ≤ TERM_INPUT_MAX_BYTES）。
 * 粘贴大段文本时分帧发送而不是丢弃整段；按字节切分，服务端按字节流写
 * 入 PTY，跨帧拼接即还原原文。
 */
export function chunkInput(data: Uint8Array, maxBytes: number): Uint8Array[] {
  if (data.length === 0) {
    return [];
  }
  if (maxBytes <= 0) {
    return [data.subarray()];
  }
  const chunks: Uint8Array[] = [];
  for (let offset = 0; offset < data.length; offset += maxBytes) {
    chunks.push(data.subarray(offset, Math.min(offset + maxBytes, data.length)));
  }
  return chunks;
}

export function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  for (let offset = 0; offset < bytes.length; offset += BTOA_CHUNK) {
    const slice = bytes.subarray(offset, Math.min(offset + BTOA_CHUNK, bytes.length));
    binary += String.fromCharCode(...slice);
  }
  return btoa(binary);
}

export function base64ToBytes(value: string): Uint8Array {
  let binary: string;
  try {
    binary = atob(value);
  } catch {
    throw new Error(`invalid base64 payload (length ${value.length})`);
  }
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

/** 把未知 JSON 值窄化为 TermServerFrame；不合法返回 null（不抛错）。 */
export function narrowTermServerFrame(value: unknown): TermServerFrame | null {
  if (typeof value !== 'object' || value === null) {
    return null;
  }
  const frame = value as Record<string, unknown>;
  switch (frame.type) {
    case 'output':
      if (typeof frame.data !== 'string') {
        return null;
      }
      try {
        return { type: 'output', data: base64ToBytes(frame.data) };
      } catch {
        return null; // 非法 base64 载荷：整帧丢弃
      }
    case 'resized':
      if (typeof frame.cols !== 'number' || typeof frame.rows !== 'number') {
        return null;
      }
      return { type: 'resized', cols: frame.cols, rows: frame.rows };
    case 'error':
      if (typeof frame.code !== 'string' || typeof frame.message !== 'string') {
        return null;
      }
      return { type: 'error', code: frame.code, message: frame.message };
    case 'closed':
      if (typeof frame.reason !== 'string') {
        return null;
      }
      return { type: 'closed', reason: frame.reason };
    default:
      return null;
  }
}

/** 连接状态（不含业务帧）：供 UI 呈现连接中/已断开。 */
export type TermSocketStatus = 'connecting' | 'open' | 'closed';

export interface TermSocketHandlers {
  onStatus?: (status: TermSocketStatus) => void;
  onFrame?: (frame: TermServerFrame) => void;
  /** 连接层错误（升级失败、异常断开等），业务错误走 error 帧。 */
  onSocketError?: (event: Event) => void;
}

export interface TermSocket {
  sendInput(data: Uint8Array): boolean;
  sendResize(cols: number, rows: number): void;
  /** 幂等关闭：重复调用安全。 */
  dispose(): void;
}

export function termSocketUrl(sessionId: string): string {
  const base = window.location.host;
  const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${base}/api/term/sessions/${encodeURIComponent(sessionId)}/ws`;
}

/**
 * 打开终端 WS（契约 8.4 指定入口名）。cookie same-origin 随连接自动携带，
 * 与 REST 鉴权一致；Origin 由浏览器按页面同源发送，满足 6.4 校验。
 */
export function openTermSocket(sessionId: string, handlers: TermSocketHandlers): TermSocket {
  const ws = new WebSocket(termSocketUrl(sessionId), TERM_WS_PROTOCOL);
  let disposed = false;
  // CONNECTING 期间排队的出站帧（p2 sprint 5）：TermLive 挂载即 applyFit
  // 上报视口真实尺寸，此时 WS 尚在握手——修复前非 OPEN 一律静默丢弃，
  // 首个 resize 永远发不出去，服务端 PTY 停留在创建时的 120×40，
  // 换行错位、全屏 TUI 必乱（走查实测零 resize 帧）。排队规则：resize
  // 只保留最新值（旧尺寸无意义），input 保序；onopen 后按序 flush。
  let outboundQueue: TermClientFrame[] = [];

  ws.onopen = () => {
    if (disposed) {
      return;
    }
    const queued = outboundQueue;
    outboundQueue = [];
    for (const frame of queued) {
      ws.send(JSON.stringify(frame));
    }
    handlers.onStatus?.('open');
  };
  ws.onmessage = (event: MessageEvent<string>) => {
    if (disposed) {
      return;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(event.data);
    } catch {
      return; // 非法帧直接丢弃（服务端契约内不应出现）
    }
    const frame = narrowTermServerFrame(parsed);
    if (frame) {
      handlers.onFrame?.(frame);
    }
  };
  ws.onerror = (event: Event) => {
    if (!disposed) {
      handlers.onSocketError?.(event);
    }
  };
  ws.onclose = () => {
    if (!disposed) {
      handlers.onStatus?.('closed');
    }
  };

  function send(payload: TermClientFrame): void {
    if (disposed) {
      return;
    }
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(payload));
      return;
    }
    if (ws.readyState === WebSocket.CONNECTING) {
      if (payload.type === 'resize') {
        outboundQueue = outboundQueue.filter((frame) => frame.type !== 'resize');
      }
      outboundQueue.push(payload);
    }
    // CLOSING/CLOSED：丢弃（连接已断或已 dispose，契约 6.4 不自动重连）。
  }

  return {
    sendInput(data: Uint8Array): boolean {
      if (data.length === 0) {
        return false;
      }
      // 契约 5.13 单帧 8KB 上限：粘贴大段文本按 8KB 分帧发送而不是静默
      // 丢弃整段（评审发现：丢弃无任何反馈，用户以为已粘贴成功）。
      // UTF-8 按字节切分——服务端按字节流写入 PTY，跨帧拼接即还原原文。
      for (const chunk of chunkInput(data, TERM_INPUT_MAX_BYTES)) {
        send({ type: 'input', data: bytesToBase64(chunk) });
      }
      return true;
    },
    sendResize(cols: number, rows: number) {
      send({ type: 'resize', cols, rows });
    },
    dispose() {
      if (disposed) {
        return;
      }
      disposed = true;
      outboundQueue = [];
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close();
      }
    },
  };
}
