import { describe, expect, it } from 'vitest';

import { StreamBuffer } from '../streamBuffer';

// 流式 token 缓冲的调度与取消语义：RAF 主路径、50ms 兜底、flush/dispose。
// 通过注入 raf/setTimeout 桩完全脱离真实定时器（与既有 streamBuffer.test.ts
// 的基础合并语义互补）。

interface Harness {
  flushed: string[];
  rafCallbacks: Array<() => void>;
  timeoutCallbacks: Array<() => void>;
  cancelledRaf: number[];
  cancelledTimeout: number[];
  buffer: StreamBuffer;
}

function createHarness(fallbackMs = 50): Harness {
  const harness: Harness = {
    flushed: [],
    rafCallbacks: [],
    timeoutCallbacks: [],
    cancelledRaf: [],
    cancelledTimeout: [],
    buffer: null as unknown as StreamBuffer,
  };
  let rafId = 1;
  let timeoutId = 1;
  harness.buffer = new StreamBuffer({
    onFlush: (text) => harness.flushed.push(text),
    raf: (callback) => {
      harness.rafCallbacks.push(callback);
      return rafId++;
    },
    cancelRaf: (handle) => harness.cancelledRaf.push(handle),
    setTimeoutFn: (callback) => {
      harness.timeoutCallbacks.push(callback);
      return timeoutId++ as unknown as ReturnType<typeof setTimeout>;
    },
    clearTimeoutFn: (handle) => harness.cancelledTimeout.push(handle as unknown as number),
    fallbackMs,
  });
  return harness;
}

describe('StreamBuffer 调度与取消', () => {
  it('raf 回调触发时一次性输出全部缓冲 token', () => {
    const h = createHarness();
    h.buffer.append('你');
    h.buffer.append('好');
    expect(h.flushed).toEqual([]);
    h.rafCallbacks[0]?.();
    expect(h.flushed).toEqual(['你好']);
  });

  it('同一帧内多次 append 只注册一次 raf + 一次兜底 timeout', () => {
    const h = createHarness();
    h.buffer.append('a');
    h.buffer.append('b');
    h.buffer.append('c');
    expect(h.rafCallbacks).toHaveLength(1);
    expect(h.timeoutCallbacks).toHaveLength(1);
  });

  it('flush 后再次 append 会重新注册调度', () => {
    const h = createHarness();
    h.buffer.append('a');
    h.buffer.flush();
    h.buffer.append('b');
    expect(h.rafCallbacks).toHaveLength(2);
    h.rafCallbacks[1]?.();
    expect(h.flushed).toEqual(['a', 'b']);
  });

  it('raf 先到：兜底 timeout 再触发时缓冲已空，不重复输出', () => {
    const h = createHarness();
    h.buffer.append('token');
    h.rafCallbacks[0]?.();
    h.timeoutCallbacks[0]?.();
    expect(h.flushed).toEqual(['token']);
  });

  it('无 raf 环境由兜底 setTimeout 输出（页面隐藏等场景）', () => {
    const h = createHarness();
    h.buffer.append('隐藏页时到达的 token');
    expect(h.timeoutCallbacks).toHaveLength(1);
    h.timeoutCallbacks[0]?.();
    expect(h.flushed).toEqual(['隐藏页时到达的 token']);
  });

  it('手动 flush 取消已排队的 raf 与兜底定时器', () => {
    const h = createHarness();
    h.buffer.append('x');
    h.buffer.flush();
    expect(h.cancelledRaf).toHaveLength(1);
    expect(h.cancelledTimeout).toHaveLength(1);
    // 已取消的回调再触发也不应输出。
    h.rafCallbacks[0]?.();
    h.timeoutCallbacks[0]?.();
    expect(h.flushed).toEqual(['x']);
  });

  it('dispose 丢弃未输出内容并清理定时器', () => {
    const h = createHarness();
    h.buffer.append('将被丢弃');
    h.buffer.dispose();
    expect(h.cancelledRaf).toHaveLength(1);
    expect(h.buffer.pendingLength).toBe(0);
    h.rafCallbacks[0]?.();
    h.timeoutCallbacks[0]?.();
    expect(h.flushed).toEqual([]);
  });

  it('pendingLength 跟随缓冲增减；flush 后归零', () => {
    const h = createHarness();
    expect(h.buffer.pendingLength).toBe(0);
    h.buffer.append('abc');
    expect(h.buffer.pendingLength).toBe(3);
    h.buffer.flush();
    expect(h.buffer.pendingLength).toBe(0);
  });

  it('空字符串 append 既不缓冲也不调度；flush 空缓冲不回调', () => {
    const h = createHarness();
    h.buffer.append('');
    expect(h.rafCallbacks).toHaveLength(0);
    h.buffer.flush();
    expect(h.flushed).toEqual([]);
  });
});
