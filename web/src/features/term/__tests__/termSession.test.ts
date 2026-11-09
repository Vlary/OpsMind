// F1 单测：终端会话控制器（输出/输入流、resize 节流、closed 状态、
// dispose 全量清理——契约 6.4/9.4：组件卸载清理逻辑以纯类直测）。
import { describe, expect, it, vi } from 'vitest';

import type { TermServerFrame, TermSocket } from '../../../api/ws';
import { clampWindowSize, TermSessionController, type TerminalLike } from '../termSession';

type StatusHandler = (status: 'connecting' | 'open' | 'closed') => void;
type FrameHandler = (frame: TermServerFrame) => void;

class FakeSocket implements TermSocket {
  sent: Array<{ type: 'input'; data: string } | { type: 'resize'; cols: number; rows: number }> =
    [];
  disposed = false;
  onStatus: StatusHandler = () => {};
  onFrame: FrameHandler = () => {};

  sendInput(data: Uint8Array): boolean {
    this.sent.push({ type: 'input', data: new TextDecoder().decode(data) });
    return true;
  }

  sendResize(cols: number, rows: number): void {
    this.sent.push({ type: 'resize', cols, rows });
  }

  dispose(): void {
    this.disposed = true;
  }
}

class FakeTerminal implements TerminalLike {
  writes: Array<string | Uint8Array> = [];
  resizes: Array<{ cols: number; rows: number }> = [];
  dataHandler: ((data: string) => void) | null = null;

  write(data: string | Uint8Array): void {
    this.writes.push(data);
  }

  resize(cols: number, rows: number): void {
    this.resizes.push({ cols, rows });
  }

  onData(handler: (data: string) => void): { dispose(): void } {
    this.dataHandler = handler;
    return {
      dispose: () => {
        this.dataHandler = null;
      },
    };
  }

  reset(): void {
    this.writes = [];
  }
}

function setup() {
  const socket = new FakeSocket();
  const terminal = new FakeTerminal();
  const controller = new TermSessionController({
    terminal,
    connect: (handlers) => {
      socket.onStatus = handlers.onStatus;
      socket.onFrame = handlers.onFrame;
      return socket;
    },
  });
  return { controller, socket, terminal };
}

describe('TermSessionController', () => {
  it('output 帧写入终端（Uint8Array 直写）', () => {
    const { controller, socket, terminal } = setup();
    socket.onStatus('open');
    socket.onFrame({ type: 'output', data: new TextEncoder().encode('ls -la\r\n') });
    expect(terminal.writes).toHaveLength(1);
    expect(new TextDecoder().decode(terminal.writes[0] as Uint8Array)).toBe('ls -la\r\n');
    expect(controller.getState().socket).toBe('open');
  });

  it('终端输入 → input 帧；closed 后拒绝发送', () => {
    const { controller, socket, terminal } = setup();
    socket.onStatus('open');
    terminal.dataHandler?.('echo hi\r');
    socket.onFrame({ type: 'closed', reason: 'idle_timeout' });
    terminal.dataHandler?.('rm -rf /\r');
    expect(socket.sent).toEqual([{ type: 'input', data: 'echo hi\r' }]);
    expect(controller.getState().closedReason).toBe('idle_timeout');
  });

  it('resized 帧回写本地终端尺寸', () => {
    const { socket, terminal } = setup();
    socket.onFrame({ type: 'resized', cols: 200, rows: 60 });
    expect(terminal.resizes).toEqual([{ cols: 200, rows: 60 }]);
  });

  it('error 帧记录 code/message', () => {
    const { controller, socket } = setup();
    socket.onFrame({ type: 'error', code: 'COMMAND_TOO_LARGE', message: 'frame too large' });
    expect(controller.getState().errorCode).toBe('COMMAND_TOO_LARGE');
    expect(controller.getState().errorMessage).toBe('frame too large');
  });

  it('WS 意外断开（无 closed 帧）置 abnormalClose', () => {
    const { controller, socket } = setup();
    socket.onStatus('open');
    socket.onStatus('closed');
    const state = controller.getState();
    expect(state.abnormalClose).toBe(true);
    expect(state.closedReason).toBeNull();
  });

  it('服务端 closed 帧后的 closed 状态不算异常断开', () => {
    const { controller, socket } = setup();
    socket.onStatus('open');
    socket.onFrame({ type: 'closed', reason: 'max_duration' });
    socket.onStatus('closed');
    const state = controller.getState();
    expect(state.abnormalClose).toBe(false);
    expect(state.closedReason).toBe('max_duration');
  });

  it('resize 节流：200ms 内与同值不重复上报', () => {
    let clock = 1000;
    const socket = new FakeSocket();
    const terminal = new FakeTerminal();
    const controller = new TermSessionController({
      terminal,
      connect: (handlers) => {
        socket.onStatus = handlers.onStatus;
        socket.onFrame = handlers.onFrame;
        return socket;
      },
      now: () => clock,
    });

    controller.reportResize(120, 40);
    controller.reportResize(121, 41); // 节流窗口内：暂存待补发
    expect(socket.sent).toEqual([{ type: 'resize', cols: 120, rows: 40 }]);
    controller.dispose(); // 清掉 trailing 定时器，避免泄漏到后续用例。

    clock += 300;
    controller.reportResize(121, 41); // dispose 后不再上报
    expect(socket.sent).toEqual([{ type: 'resize', cols: 120, rows: 40 }]);
  });

  it('resize 尾值补发：拖拽最后一次 fit 落在节流窗口内不再被永久丢弃（评审发现）', async () => {
    vi.useFakeTimers();
    try {
      let clock = 1000;
      const socket = new FakeSocket();
      const terminal = new FakeTerminal();
      const controller = new TermSessionController({
        terminal,
        connect: (handlers) => {
          socket.onStatus = handlers.onStatus;
          socket.onFrame = handlers.onFrame;
          return socket;
        },
        now: () => clock,
      });

      controller.reportResize(120, 40); // leading 上报
      controller.reportResize(130, 45); // 窗口内：暂存
      controller.reportResize(135, 48); // 窗口内：刷新暂存为最终尺寸
      expect(socket.sent).toEqual([{ type: 'resize', cols: 120, rows: 40 }]);

      clock += 200;
      await vi.advanceTimersByTimeAsync(200); // trailing 定时器到期：补发最终尺寸
      expect(socket.sent).toEqual([
        { type: 'resize', cols: 120, rows: 40 },
        { type: 'resize', cols: 135, rows: 48 },
      ]);

      controller.dispose();
    } finally {
      vi.useRealTimers();
    }
  });

  it('resize 上报前 clamp 到服务端合法范围 [10,500]×[4,200]（p2 sprint 5）', () => {
    let clock = 1000;
    const socket = new FakeSocket();
    const terminal = new FakeTerminal();
    const controller = new TermSessionController({
      terminal,
      connect: (handlers) => {
        socket.onStatus = handlers.onStatus;
        socket.onFrame = handlers.onFrame;
        return socket;
      },
      now: () => clock,
    });
    // 抽屉动画等瞬态布局下 fit 可能算出越界值：clamp 到边界而不是发送
    // 被 VALIDATION_FAILED 拒绝的原始值（走查实测首屏偶发红色横幅）。
    controller.reportResize(2, 25);
    clock += 300; // 越过节流窗口，三次上报各自 leading 生效
    controller.reportResize(120, 1000);
    clock += 300;
    controller.reportResize(96.6, 24.4); // 非整数取整
    expect(socket.sent).toEqual([
      { type: 'resize', cols: 10, rows: 25 },
      { type: 'resize', cols: 120, rows: 200 },
      { type: 'resize', cols: 97, rows: 24 },
    ]);
    controller.dispose();
  });

  it('clamp 后与已上报值相同的越界输入不再上报（阈值比较用 clamp 值）', () => {
    const { controller, socket } = setup();
    controller.reportResize(10, 25);
    controller.reportResize(3, 25); // clamp 后仍是 10×25：数值未变不上报
    expect(socket.sent).toEqual([{ type: 'resize', cols: 10, rows: 25 }]);
    controller.dispose();
  });

  it('clampWindowSize：非有限数返回 null；范围内取整', () => {
    expect(clampWindowSize(Number.NaN, 25)).toBeNull();
    expect(clampWindowSize(80, Number.POSITIVE_INFINITY)).toBeNull();
    expect(clampWindowSize(80.4, 24.6)).toEqual({ cols: 80, rows: 25 });
    expect(clampWindowSize(500, 200)).toEqual({ cols: 500, rows: 200 });
  });

  it('closeByUser：关 socket 并置 client_closed', () => {
    const { controller, socket } = setup();
    socket.onStatus('open');
    controller.closeByUser();
    expect(socket.disposed).toBe(true);
    expect(controller.getState().closedReason).toBe('client_closed');
  });

  it('dispose 全量清理：WS dispose、onData 解绑、后续帧不再写终端；幂等', () => {
    const { controller, socket, terminal } = setup();
    socket.onStatus('open');
    const listener = vi.fn();
    controller.subscribe(listener);

    controller.dispose();
    controller.dispose(); // 幂等

    expect(socket.disposed).toBe(true);
    expect(terminal.dataHandler).toBeNull();
    socket.onFrame({ type: 'output', data: new TextEncoder().encode('late') });
    expect(terminal.writes).toHaveLength(0);
    terminal.dataHandler?.('input after dispose');
    expect(socket.sent).toHaveLength(0);
    listener.mockClear();
  });
});
