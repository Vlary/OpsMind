// F1 单测：RDP 会话控制器（guacamole 桥接与卸载全量清理；契约 6.5/9.4）。
// node 环境（无 DOM/WebSocket）：注入 fake Guacamole 工厂与 fake 元素，
// 验证 display 挂载、键鼠 state 桥、状态映射、剪贴板流、dispose 后
// client.disconnect/WS 关闭/回调置空。
import { describe, expect, it, vi } from 'vitest';

import type { components } from '../../../api/schema';
import {
  GUAC_CLIENT_STATE,
  GUAC_TUNNEL_STATE,
  RdpSessionController,
  TUNNEL_FAILURE_MESSAGE,
  type GuacamoleClient,
  type GuacamoleDisplay,
  type GuacamoleFactory,
  type GuacamoleKeyboard,
  type GuacamoleMouse,
  type GuacamoleTunnel,
} from '../rdpSession';

type RdpSession = components['schemas']['RdpSession'];

/** 最小 DOM 替身：只实现 controller 用到的 appendChild/contains/remove/
 * addEventListener（键盘监听元素侧 no-op，事件由 fake 直接回调）。 */
class FakeElement {
  parent: FakeElement | null = null;

  appendChild(child: unknown): void {
    if (child instanceof FakeElement) {
      child.parent = this;
    }
  }

  contains(node: unknown): boolean {
    let cursor: FakeElement | null =
      typeof node === 'object' && node instanceof FakeElement ? node : null;
    while (cursor) {
      if (cursor === this) {
        return true;
      }
      cursor = cursor.parent;
    }
    return false;
  }

  remove(): void {
    this.parent = null;
  }

  addEventListener(): void {
    // no-op：fake Keyboard 不真正挂 DOM 监听。
  }
}

function fakeSession(): RdpSession {
  return {
    id: '0d8e6f3a-9c1d-4a2b-b3c4-d5e6f708192a',
    server_id: '11111111-2222-3333-4444-555555555555',
    server_name: 'win-01',
    width: 1280,
    height: 800,
    dpi: 96,
    status: 'active',
    ended_reason: '',
    has_recording: false,
    bytes_written: 0,
    started_at: '2026-10-25T10:00:00Z',
    last_activity_at: '2026-10-25T10:00:00Z',
    ended_at: null,
    created_at: '2026-10-25T10:00:00Z',
    updated_at: '2026-10-25T10:00:00Z',
  };
}

class FakeDisplay implements GuacamoleDisplay {
  element = new FakeElement();
  scaleCalls: number[] = [];

  getElement(): FakeElement {
    return this.element;
  }

  getWidth(): number {
    return 1280;
  }

  getHeight(): number {
    return 800;
  }

  scale(factor: number): void {
    this.scaleCalls.push(factor);
  }
}

interface ClipboardStream {
  onblob: ((base64: string) => void) | null;
  onend: (() => void) | null;
}

class FakeClient implements GuacamoleClient {
  display = new FakeDisplay();
  connected = false;
  disconnected = false;
  connectArgs: Array<string | undefined> = [];
  keyEvents: Array<{ pressed: boolean; keysym: number }> = [];
  mouseEvents: number[] = [];
  clipboardStreams: string[] = [];
  onstatechange: ((state: number) => void) | null = null;
  onerror: ((status: { code: number; message: string }) => void) | null = null;
  onclipboard: ((stream: ClipboardStream, mimeType: string) => void) | null = null;

  connect(data?: string): void {
    this.connectArgs.push(data);
    this.connected = true;
  }

  disconnect(): void {
    this.disconnected = true;
  }

  getDisplay(): GuacamoleDisplay {
    return this.display;
  }

  sendKeyEvent(pressed: boolean, keysym: number): void {
    this.keyEvents.push({ pressed, keysym });
  }

  sendMouseState(state: { x: number }, applyDisplayScale?: boolean): void {
    this.mouseEvents.push(state.x * (applyDisplayScale ? 2 : 1));
  }

  createClipboardStream(mimeType: string): { writeBlob(blob: Blob): void } {
    this.clipboardStreams.push(mimeType);
    return { writeBlob: () => {} };
  }
}

class FakeTunnel implements GuacamoleTunnel {
  connected = false;
  disconnected = false;
  onerror: ((status: { code: number; message: string }) => void) | null = null;
  onstatechange: ((state: number) => void) | null = null;

  connect(): void {
    this.connected = true;
  }

  disconnect(): void {
    this.disconnected = true;
  }
}

class FakeKeyboard implements GuacamoleKeyboard {
  onkeydown: ((keysym: number) => boolean) | null = null;
  onkeyup: ((keysym: number) => boolean) | null = null;
}

interface FakeMouseState {
  x: number;
}

class FakeMouse implements GuacamoleMouse {
  onmousedown: ((state: FakeMouseState) => boolean) | null = null;
  onmousemove: ((state: FakeMouseState) => boolean) | null = null;
  onmouseup: ((state: FakeMouseState) => boolean) | null = null;
}

function setup() {
  const client = new FakeClient();
  const tunnel = new FakeTunnel();
  const keyboard = new FakeKeyboard();
  const mouse = new FakeMouse();
  const stage = new FakeElement();
  const factory: GuacamoleFactory = {
    createTunnel: () => tunnel,
    createClient: () => client,
    createKeyboard: () => keyboard,
    createMouse: () => mouse,
  };
  const controller = new RdpSessionController({
    session: fakeSession(),
    wsUrl: '/api/rdp/sessions/x/ws',
    mountCanvas: (canvas) => {
      stage.appendChild(canvas);
    },
    keyboardElement: stage,
    factory,
  });
  return { controller, client, tunnel, keyboard, mouse, stage };
}

function b64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

describe('RdpSessionController', () => {
  it('构造即连接：display 挂载 stage、client.connect("")、初始 connecting', () => {
    const { controller, client, stage } = setup();
    expect(client.connected).toBe(true);
    // connect 传空串：WebSocketTunnel 把 data 拼进 URL（url + "?" + data），
    // 缺省 undefined 会让真实 WS 地址带 "?undefined"（走查实测）。
    expect(client.connectArgs).toEqual(['']);
    expect(stage.contains(client.display.getElement())).toBe(true);
    expect(controller.getState().phase).toBe('connecting');
  });

  it('键鼠 state 桥：keydown/keyup → sendKeyEvent；鼠标 → sendMouseState（含显示比例）', () => {
    const { client, keyboard, mouse } = setup();
    keyboard.onkeydown?.(65);
    keyboard.onkeyup?.(65);
    mouse.onmousedown?.({ x: 10 });
    expect(client.keyEvents).toEqual([
      { pressed: true, keysym: 65 },
      { pressed: false, keysym: 65 },
    ]);
    // applyDisplayScale=true：fake 按 x2 体现换算发生。
    expect(client.mouseEvents).toEqual([20]);
  });

  it('状态映射：CONNECTING/WAITING/CONNECTED/DISCONNECTED', () => {
    const { controller, client } = setup();
    client.onstatechange?.(GUAC_CLIENT_STATE.CONNECTED);
    expect(controller.getState().phase).toBe('connected');
    client.onstatechange?.(GUAC_CLIENT_STATE.WAITING);
    expect(controller.getState().phase).toBe('waiting');
    client.onstatechange?.(GUAC_CLIENT_STATE.CONNECTING);
    expect(controller.getState().phase).toBe('connecting');
    client.onstatechange?.(GUAC_CLIENT_STATE.DISCONNECTED);
    expect(controller.getState().phase).toBe('disconnected');
  });

  it('onerror → failed 并携带 message', () => {
    const { controller, client } = setup();
    client.onerror?.({ code: 0x0201, message: 'guacd is down' });
    expect(controller.getState().phase).toBe('failed');
    expect(controller.getState().error).toBe('guacd is down');
  });

  it('隧道层事件已挂：tunnel.onerror/onstatechange 由 controller 接管', () => {
    const { tunnel } = setup();
    expect(tunnel.onerror).not.toBeNull();
    expect(tunnel.onstatechange).not.toBeNull();
  });

  it('隧道握手失败（tunnel.onerror）→ failed 透出 message（w5b：不挂则永远 connecting）', () => {
    const { controller, tunnel } = setup();
    tunnel.onerror?.({ code: 0x0202, message: 'Upstream not found' });
    expect(controller.getState().phase).toBe('failed');
    expect(controller.getState().error).toBe('Upstream not found');
  });

  it('服务端无错误状态直接关 WS（tunnel CLOSED）：连接期 failed 兜底、已连接后 disconnected', () => {
    // 连接期（NormalClosure 1000 → SUCCESS 不触发 onerror，只有状态回环）。
    const first = setup();
    first.tunnel.onstatechange?.(GUAC_TUNNEL_STATE.CLOSED);
    expect(first.controller.getState().phase).toBe('failed');
    expect(first.controller.getState().error).toBe(TUNNEL_FAILURE_MESSAGE);

    // 已连接后被服务端断开：按断开呈现而非失败。
    const second = setup();
    second.client.onstatechange?.(GUAC_CLIENT_STATE.CONNECTED);
    second.tunnel.onstatechange?.(GUAC_TUNNEL_STATE.CLOSED);
    expect(second.controller.getState().phase).toBe('disconnected');
  });

  it('隧道 UNSTABLE（连接期 1.5s 无数据）→ waiting 给出可见反馈', () => {
    const { controller, tunnel } = setup();
    tunnel.onstatechange?.(GUAC_TUNNEL_STATE.UNSTABLE);
    expect(controller.getState().phase).toBe('waiting');
    // 已 connected 后的 UNSTABLE 不回退状态。
    const second = setup();
    second.client.onstatechange?.(GUAC_CLIENT_STATE.CONNECTED);
    second.tunnel.onstatechange?.(GUAC_TUNNEL_STATE.UNSTABLE);
    expect(second.controller.getState().phase).toBe('connected');
  });

  it('用户主动断开后的隧道回环（disconnect 内部触发）不再改状态', () => {
    const { controller, tunnel } = setup();
    controller.disconnectByUser();
    tunnel.onstatechange?.(GUAC_TUNNEL_STATE.CLOSED);
    tunnel.onerror?.({ code: 511, message: 'late error' });
    expect(controller.getState().phase).toBe('connecting');
    expect(controller.getState().error).toBeNull();
  });

  it('剪贴板：发送走 text/plain 流；接收按 base64 分片拼 UTF-8 文本', () => {
    const { controller, client } = setup();
    controller.sendClipboard('你好');
    expect(client.clipboardStreams).toEqual(['text/plain']);
    controller.sendClipboard('');
    expect(client.clipboardStreams).toHaveLength(1); // 空串不发送

    // 模拟远端 clipboard 指令：controller 注入 onblob/onend 后分片送达。
    client.onclipboard?.({ onblob: null, onend: null }, 'text/plain');
    let received: ClipboardStream | null = null;
    const original = client.onclipboard;
    client.onclipboard = (stream, mimeType) => {
      if (mimeType === 'text/plain') {
        received = stream;
      }
      original?.(stream, mimeType);
    };
    client.onclipboard({ onblob: null, onend: null }, 'text/plain');
    expect(received).not.toBeNull();
    received!.onblob?.(b64('远端'));
    received!.onblob?.(b64('复制'));
    received!.onend?.();
    expect(controller.getState().remoteClipboard).toBe('远端复制');
  });

  it('applyScale：按容器/画面比例缩放；非法尺寸忽略', () => {
    const { controller, client } = setup();
    controller.applyScale(640, 400);
    expect(client.display.scaleCalls).toEqual([0.5]);
    controller.applyScale(0, 0);
    controller.applyScale(-10, 100);
    expect(client.display.scaleCalls).toHaveLength(1);
  });

  it('dispose 全量清理：disconnect、键鼠/回调置空、display 移出；幂等', () => {
    const { controller, client, tunnel, keyboard, mouse, stage } = setup();
    const listener = vi.fn();
    controller.subscribe(listener);

    controller.dispose();
    controller.dispose(); // 幂等

    expect(client.disconnected).toBe(true);
    expect(keyboard.onkeydown).toBeNull();
    expect(keyboard.onkeyup).toBeNull();
    expect(mouse.onmousedown).toBeNull();
    expect(mouse.onmousemove).toBeNull();
    expect(mouse.onmouseup).toBeNull();
    expect(client.onstatechange).toBeNull();
    expect(client.onerror).toBeNull();
    expect(client.onclipboard).toBeNull();
    expect(tunnel.onerror).toBeNull();
    expect(tunnel.onstatechange).toBeNull();
    expect(stage.contains(client.display.getElement())).toBe(false);

    // dispose 后：状态回调不再传播、剪贴板不再发送。
    listener.mockClear();
    client.onstatechange?.(GUAC_CLIENT_STATE.CONNECTED);
    controller.sendClipboard('x');
    expect(client.clipboardStreams).toHaveLength(0);
    expect(listener).not.toHaveBeenCalled();
  });

  it('disconnectByUser 标记用户主动断开', () => {
    const { controller, client } = setup();
    controller.disconnectByUser();
    expect(client.disconnected).toBe(true);
    expect(controller.wasDisconnectedByUser()).toBe(true);
  });
});
