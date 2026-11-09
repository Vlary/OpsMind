// F1 单测：终端 WS 帧纯函数（base64 编解码 + 服务端帧窄化；契约 6.4）
// 与 openTermSocket 的 CONNECTING 排队行为（p2 sprint 5，注入 fake
// WebSocket/window 后在 node 直测）。
import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  base64ToBytes,
  bytesToBase64,
  chunkInput,
  narrowTermServerFrame,
  openTermSocket,
  TERM_WS_PROTOCOL,
} from './ws';

describe('bytesToBase64 / base64ToBytes', () => {
  it('往返保持字节一致（含非 UTF-8 字节与控制序列）', () => {
    const bytes = new Uint8Array([0, 1, 127, 128, 200, 255, 27, 91, 65]);
    expect(Array.from(base64ToBytes(bytesToBase64(bytes)))).toEqual(Array.from(bytes));
  });

  it('分块路径：超 32KB 的输入不炸栈且往返一致', () => {
    const bytes = new Uint8Array(0x9000);
    for (let index = 0; index < bytes.length; index += 1) {
      bytes[index] = index % 256;
    }
    const round = base64ToBytes(bytesToBase64(bytes));
    expect(round.length).toBe(bytes.length);
    expect(round[0x8000]).toBe(bytes[0x8000]);
    expect(round[bytes.length - 1]).toBe(bytes[bytes.length - 1]);
  });

  it('文本经 UTF-8 字节往返', () => {
    const text = 'echo 中文测试 ✓';
    const encoded = new TextEncoder().encode(text);
    expect(new TextDecoder().decode(base64ToBytes(bytesToBase64(encoded)))).toBe(text);
  });
});

describe('narrowTermServerFrame', () => {
  it('output：data 解码为字节', () => {
    const bytes = new TextEncoder().encode('hi\n');
    const frame = narrowTermServerFrame({ type: 'output', data: bytesToBase64(bytes) });
    expect(frame).toEqual({ type: 'output', data: bytes });
  });

  it('resized/error/closed 各自窄化', () => {
    expect(narrowTermServerFrame({ type: 'resized', cols: 80, rows: 24 })).toEqual({
      type: 'resized',
      cols: 80,
      rows: 24,
    });
    expect(
      narrowTermServerFrame({ type: 'error', code: 'TERM_CONNECT_FAILED', message: 'x' }),
    ).toEqual({ type: 'error', code: 'TERM_CONNECT_FAILED', message: 'x' });
    expect(narrowTermServerFrame({ type: 'closed', reason: 'idle_timeout' })).toEqual({
      type: 'closed',
      reason: 'idle_timeout',
    });
  });

  it('非法形态返回 null（不抛错）', () => {
    expect(narrowTermServerFrame(null)).toBeNull();
    expect(narrowTermServerFrame('output')).toBeNull();
    expect(narrowTermServerFrame({ type: 'input', data: 'x' })).toBeNull(); // 客户端帧类型
    expect(narrowTermServerFrame({ type: 'resized', cols: '80', rows: 24 })).toBeNull();
    expect(narrowTermServerFrame({ type: 'output', data: 123 })).toBeNull();
    expect(narrowTermServerFrame({ type: 'error', code: 'X' })).toBeNull();
    expect(narrowTermServerFrame({ type: 'closed' })).toBeNull();
    expect(narrowTermServerFrame({ type: 'output', data: '!!not base64!!' })).toBeNull();
  });
});

describe('chunkInput 大段粘贴分帧（契约 5.13 单帧上限；评审发现静默丢弃）', () => {
  it('超过单帧上限的输入按上限切块，跨帧拼接还原原文', () => {
    const text = 'x'.repeat(8192 * 2 + 123);
    const data = new TextEncoder().encode(text);
    const chunks = chunkInput(data, 8192);
    expect(chunks.length).toBe(3);
    expect(chunks[0]?.length).toBe(8192);
    expect(chunks[1]?.length).toBe(8192);
    expect(chunks[2]?.length).toBe(123);
    const joined = new Uint8Array(data.length);
    let offset = 0;
    for (const chunk of chunks) {
      joined.set(chunk, offset);
      offset += chunk.length;
    }
    expect(new TextDecoder().decode(joined)).toBe(text);
  });

  it('未超限输入单帧返回；空输入返回空数组', () => {
    expect(chunkInput(new TextEncoder().encode('hi'), 8192)).toHaveLength(1);
    expect(chunkInput(new Uint8Array(0), 8192)).toEqual([]);
  });

  it('多字节 UTF-8 按字节切分：拼接后仍还原中文原文', () => {
    const text = '配置'.repeat(6000); // 每字 3 字节，总 36KB
    const data = new TextEncoder().encode(text);
    const chunks = chunkInput(data, 8192);
    expect(chunks.length).toBeGreaterThanOrEqual(2);
    const joined = new Uint8Array(data.length);
    let offset = 0;
    for (const chunk of chunks) {
      joined.set(chunk, offset);
      offset += chunk.length;
    }
    expect(new TextDecoder().decode(joined)).toBe(text);
  });
});

// ---------------------------------------------------------------------------
// openTermSocket 的 CONNECTING 排队（p2 sprint 5 走查缺陷）：
// TermLive 挂载即 applyFit 上报 resize，此时 WS 尚在握手——修复前非 OPEN
// 一律静默丢弃，首个 resize 永远发不出去，服务端 PTY 停在创建时的
// 120×40。这里注入 fake WebSocket/window 验证排队与 flush 语义。
// ---------------------------------------------------------------------------

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readonly url: string;
  readonly protocols: string | string[];
  readyState = FakeWebSocket.CONNECTING;
  sent: string[] = [];
  closeCalls = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(url: string, protocols?: string | string[]) {
    this.url = url;
    this.protocols = protocols ?? [];
    onConstruct?.(this);
  }

  send(data: string): void {
    if (this.readyState !== FakeWebSocket.OPEN) {
      throw new Error('InvalidStateError: send while not open');
    }
    this.sent.push(data);
  }

  close(): void {
    this.closeCalls += 1;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.();
  }

  /** 测试辅助：模拟握手完成。 */
  simulateOpen(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
}

/** 最近一次 openTermSocket 构造出的 fake 实例。 */
let currentFake: FakeWebSocket | null = null;
/** 构造钩子：installFakeWebSocket 安装，afterEach 清除。 */
let onConstruct: ((ws: FakeWebSocket) => void) | null = null;

/** 注入记录实例的 fake WebSocket 与 window（termSocketUrl 读 location）。 */
function installFakeWebSocket() {
  onConstruct = (ws) => {
    currentFake = ws;
  };
  vi.stubGlobal('WebSocket', FakeWebSocket as unknown as typeof WebSocket);
  vi.stubGlobal('window', {
    location: { host: 'localhost:4173', protocol: 'http:' },
  });
}

function currentSocket(): FakeWebSocket {
  if (currentFake === null) {
    throw new Error('no fake WebSocket constructed yet');
  }
  return currentFake;
}

afterEach(() => {
  vi.unstubAllGlobals();
  onConstruct = null;
  currentFake = null;
});

describe('openTermSocket CONNECTING 排队（p2 sprint 5）', () => {
  it('握手期间的 resize 入队，onopen 后 flush 到服务端', () => {
    installFakeWebSocket();
    const socket = openTermSocket('sess-1', {});
    socket.sendResize(96, 25);
    const ws = currentSocket();
    expect(ws.sent).toEqual([]); // CONNECTING：尚未发出
    ws.simulateOpen();
    expect(ws.sent).toEqual([JSON.stringify({ type: 'resize', cols: 96, rows: 25 })]);
    socket.dispose();
  });

  it('多次排队 resize 只保留最新值；input 保序', () => {
    installFakeWebSocket();
    const socket = openTermSocket('sess-1', {});
    socket.sendResize(120, 40);
    socket.sendInput(new TextEncoder().encode('ls\r'));
    socket.sendResize(96, 25);
    const ws = currentSocket();
    ws.simulateOpen();
    expect(ws.sent).toEqual([
      JSON.stringify({ type: 'input', data: bytesToBase64(new TextEncoder().encode('ls\r')) }),
      JSON.stringify({ type: 'resize', cols: 96, rows: 25 }),
    ]);
    socket.dispose();
  });

  it('OPEN 后发送直发；dispose 清空队列且不再 flush', () => {
    installFakeWebSocket();
    const socket = openTermSocket('sess-1', {});
    const ws = currentSocket();
    socket.sendResize(96, 25); // 入队
    ws.simulateOpen(); // flush
    expect(ws.sent).toHaveLength(1);
    socket.sendResize(80, 24);
    expect(ws.sent[1]).toBe(JSON.stringify({ type: 'resize', cols: 80, rows: 24 }));

    // dispose 后队列清空：即使 readyState 回到 CONNECTING 也不会再发出。
    ws.readyState = FakeWebSocket.CONNECTING;
    socket.sendResize(60, 20);
    socket.dispose();
    expect(ws.sent).toHaveLength(2);
    expect(ws.closeCalls).toBe(1);
  });

  it('构造使用 term 子协议与 ws_url', () => {
    installFakeWebSocket();
    const socket = openTermSocket('sess-1', {});
    const ws = currentSocket();
    expect(ws.url).toBe('ws://localhost:4173/api/term/sessions/sess-1/ws');
    expect(ws.protocols).toBe(TERM_WS_PROTOCOL);
    socket.dispose();
  });
});
