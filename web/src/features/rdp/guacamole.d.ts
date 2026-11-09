// F1：guacamole-common-js@1.5.0 类型垫片（CONTRACT.md 8.0：官方 npm 包无
// TS 类型，垫片只声明 F1 实际用到的接口；静态结构与 dist 源码实测对齐：
// Client.State.* 为构造函数上的静态对象，Mouse/Keyboard 保留 legacy on* 回调）。

declare module 'guacamole-common-js' {
  namespace Guacamole {
    /** 鼠标瞬时状态（含坐标与按键），回传给 client.sendMouseState。 */
    interface MouseState {
      x: number;
      y: number;
      left: boolean;
      middle: boolean;
      right: boolean;
    }

    /** 显示画布（Layer 能力子集）：getElement 挂 DOM，scale 自适应缩放。 */
    interface Display {
      getElement(): HTMLElement;
      getWidth(): number;
      getHeight(): number;
      scale(factor: number): void;
    }

    /** 远端输入流（剪贴板接收）。 */
    interface InputStream {
      onblob: ((base64Data: string) => void) | null;
      onend: (() => void) | null;
    }

    /** 远端输出流（剪贴板发送）。 */
    interface OutputStream {
      writeBlob(blob: Blob, callback?: () => void): void;
      sendEnd(): void;
    }

    interface Client {
      connect(data?: string): void;
      disconnect(): void;
      getDisplay(): Display;
      sendKeyEvent(pressed: boolean, keysym: number): void;
      sendMouseState(state: MouseState, applyDisplayScale?: boolean): void;
      createClipboardStream(mimeType: string): OutputStream;
      onstatechange: ((state: number) => void) | null;
      onerror: ((status: { code: number; message: string }) => void) | null;
      onclipboard: ((stream: InputStream, mimeType: string) => void) | null;
    }

    /** Client.State.* 常量（1.5.0：IDLE 0 … DISCONNECTED 5）。 */
    interface ClientStateConstants {
      readonly IDLE: 0;
      readonly CONNECTING: 1;
      readonly WAITING: 2;
      readonly CONNECTED: 3;
      readonly DISCONNECTING: 4;
      readonly DISCONNECTED: 5;
    }

    interface ClientConstructor {
      new (tunnel: Tunnel): Client;
      State: ClientStateConstants;
    }

    interface Tunnel {
      connect(data?: string): void;
      disconnect(): void;
      /** 隧道层错误（WS 握手失败/服务端超时/上游不可达；Tunnel 基类事件）。 */
      onerror: ((status: { code: number; message: string }) => void) | null;
      /** 隧道状态变化（Tunnel.State：CONNECTING 0 / OPEN 1 / CLOSED 2 / UNSTABLE 3）。 */
      onstatechange: ((state: number) => void) | null;
    }

    interface TunnelConstructor {
      new (tunnelURL: string): Tunnel;
    }

    interface Keyboard {
      onkeydown: ((keysym: number) => boolean) | null;
      onkeyup: ((keysym: number) => boolean) | null;
    }

    interface KeyboardConstructor {
      new (element: Element | Document): Keyboard;
    }

    interface Mouse {
      onmousedown: ((state: MouseState) => boolean) | null;
      onmousemove: ((state: MouseState) => boolean) | null;
      onmouseup: ((state: MouseState) => boolean) | null;
    }

    interface MouseConstructor {
      new (element: Element | Document): Mouse;
    }

    const Client: ClientConstructor;
    const WebSocketTunnel: TunnelConstructor;
    const Keyboard: KeyboardConstructor;
    const Mouse: MouseConstructor;
  }

  export default Guacamole;
}
