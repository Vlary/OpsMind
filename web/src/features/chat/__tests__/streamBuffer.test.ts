import { describe, expect, it } from 'vitest';

import { StreamBuffer } from '../streamBuffer';

type TimeoutHandle = ReturnType<typeof setTimeout>;

function createHarness() {
  const flushes: string[] = [];
  let rafCallback: (() => void) | null = null;
  let timeoutCallback: (() => void) | null = null;

  const buffer = new StreamBuffer({
    onFlush: (text) => flushes.push(text),
    raf: (callback) => {
      rafCallback = callback;
      return 1;
    },
    cancelRaf: () => {
      rafCallback = null;
    },
    setTimeoutFn: (callback) => {
      timeoutCallback = callback;
      return 1 as unknown as TimeoutHandle;
    },
    clearTimeoutFn: () => {
      timeoutCallback = null;
    },
    fallbackMs: 50,
  });

  return {
    buffer,
    flushes,
    runRaf: () => {
      const callback = rafCallback;
      rafCallback = null;
      callback?.();
    },
    runTimeout: () => {
      const callback = timeoutCallback;
      timeoutCallback = null;
      callback?.();
    },
  };
}

describe('StreamBuffer', () => {
  it('多次 append 只触发一次 flush', () => {
    const harness = createHarness();
    harness.buffer.append('a');
    harness.buffer.append('b');
    harness.buffer.append('c');

    expect(harness.flushes).toEqual([]);
    harness.runRaf();
    expect(harness.flushes).toEqual(['abc']);
  });

  it('无 RAF 时由 50ms 兜底刷新', () => {
    const harness = createHarness();
    harness.buffer.append('fallback');
    harness.runTimeout();
    expect(harness.flushes).toEqual(['fallback']);
  });

  it('flush 立即输出并取消已排队的刷新', () => {
    const harness = createHarness();
    harness.buffer.append('x');
    harness.buffer.flush();
    expect(harness.flushes).toEqual(['x']);

    harness.runRaf();
    harness.runTimeout();
    expect(harness.flushes).toEqual(['x']);
  });

  it('多次刷新保持追加顺序', () => {
    const harness = createHarness();
    harness.buffer.append('a');
    harness.runRaf();
    harness.buffer.append('b');
    harness.runTimeout();
    harness.buffer.append('c');
    harness.buffer.flush();

    expect(harness.flushes).toEqual(['a', 'b', 'c']);
  });

  it('dispose 清理定时器并丢弃未输出内容', () => {
    const harness = createHarness();
    harness.buffer.append('discard');
    harness.buffer.dispose();
    harness.runRaf();
    harness.runTimeout();
    expect(harness.flushes).toEqual([]);
    expect(harness.buffer.pendingLength).toBe(0);
  });
});
