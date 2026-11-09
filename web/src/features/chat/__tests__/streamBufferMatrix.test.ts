import { describe, expect, it } from 'vitest';

import { StreamBuffer } from '../streamBuffer';

// StreamBuffer 的补充矩阵：与 streamBuffer.test.ts / streamBufferMore.test.ts
// 互补，覆盖交错调度、多次 flush/dispose、RAF 抖动与回调时序。

interface Harness {
  flushed: string[];
  rafQueue: Array<() => void>;
  timeoutQueue: Array<() => void>;
  rafCount: number;
  timeoutCount: number;
  buffer: StreamBuffer;
}

function createHarness(fallbackMs = 50): Harness {
  const harness: Harness = {
    flushed: [],
    rafQueue: [],
    timeoutQueue: [],
    rafCount: 0,
    timeoutCount: 0,
    buffer: null as unknown as StreamBuffer,
  };
  let handle = 1;
  harness.buffer = new StreamBuffer({
    onFlush: (text) => harness.flushed.push(text),
    raf: (callback) => {
      harness.rafQueue.push(callback);
      harness.rafCount += 1;
      return handle++;
    },
    cancelRaf: () => undefined,
    setTimeoutFn: (callback) => {
      harness.timeoutQueue.push(callback);
      harness.timeoutCount += 1;
      return handle++ as unknown as ReturnType<typeof setTimeout>;
    },
    clearTimeoutFn: () => undefined,
    fallbackMs,
  });
  return harness;
}

describe('StreamBuffer 补充矩阵', () => {
  it('append→flush→append→raf 的交错序列按顺序输出', () => {
    const h = createHarness();
    h.buffer.append('1');
    h.buffer.flush();
    h.buffer.append('2');
    h.rafQueue[0]?.();
    h.buffer.append('3');
    h.buffer.flush();
    expect(h.flushed).toEqual(['1', '2', '3']);
  });

  it('兜底 timeout 先到（RAF 被节流）：输出后 RAF 再触发不重复', () => {
    const h = createHarness();
    h.buffer.append('页面隐藏时的 token');
    h.timeoutQueue[0]?.();
    h.rafQueue[0]?.();
    expect(h.flushed).toEqual(['页面隐藏时的 token']);
  });

  it('同一批内容只输出一次，无论回调触发多少次', () => {
    const h = createHarness();
    h.buffer.append('abc');
    h.rafQueue[0]?.();
    h.rafQueue[0]?.();
    h.timeoutQueue[0]?.();
    h.timeoutQueue[0]?.();
    expect(h.flushed).toEqual(['abc']);
  });

  it('raf 与 timeout 都不触发时内容滞留缓冲（pendingLength 可观测）', () => {
    const h = createHarness();
    h.buffer.append('pending');
    expect(h.flushed).toEqual([]);
    expect(h.buffer.pendingLength).toBe(7);
  });

  it('连续多帧 append/flush 保持内容顺序与完整性', () => {
    const h = createHarness();
    const parts = ['检查', '磁盘', '使用率', '，', '输出', '结论'];
    for (const part of parts) {
      h.buffer.append(part);
      h.rafQueue.at(-1)?.();
    }
    expect(h.flushed).toEqual(parts);
  });

  it('flush 后 pendingLength 归零且新 append 重新调度', () => {
    const h = createHarness();
    h.buffer.append('x');
    h.buffer.flush();
    expect(h.buffer.pendingLength).toBe(0);
    expect(h.rafCount).toBe(1);
    h.buffer.append('y');
    expect(h.rafCount).toBe(2);
  });

  it('dispose 后继续 append 会重新调度（对象仍可用）', () => {
    const h = createHarness();
    h.buffer.append('丢弃');
    h.buffer.dispose();
    h.buffer.append('复活');
    h.rafQueue.at(-1)?.();
    expect(h.flushed).toEqual(['复活']);
  });

  it('两次 dispose 幂等，不抛错', () => {
    const h = createHarness();
    h.buffer.append('x');
    h.buffer.dispose();
    h.buffer.dispose();
    expect(h.flushed).toEqual([]);
  });

  it('自定义 fallbackMs 生效于兜底调度参数', () => {
    const delays: number[] = [];
    const buffer = new StreamBuffer({
      onFlush: () => undefined,
      raf: () => 1,
      cancelRaf: () => undefined,
      setTimeoutFn: (_callback, ms) => {
        delays.push(ms);
        return 1 as unknown as ReturnType<typeof setTimeout>;
      },
      clearTimeoutFn: () => undefined,
      fallbackMs: 120,
    });
    buffer.append('x');
    expect(delays).toEqual([120]);
  });

  it('unicode 增补平面字符的 token 不因 flush 边界撕裂', () => {
    const h = createHarness();
    h.buffer.append('🔴');
    h.buffer.append('🚀');
    h.buffer.flush();
    expect(h.flushed).toEqual(['🔴🚀']);
  });

  it('append 后立即 dispose 再 flush：不输出任何内容', () => {
    const h = createHarness();
    h.buffer.append('gone');
    h.buffer.dispose();
    h.buffer.flush();
    expect(h.flushed).toEqual([]);
  });
});
