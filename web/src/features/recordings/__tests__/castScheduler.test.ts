// F1 单测：回放调度器（假定时器；契约 9.4 只测纯逻辑）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { CastEvent } from '../castParser';
import { CastScheduler, type CastSink } from '../castScheduler';

interface RecordingSink extends CastSink {
  writes: string[];
  resizes: string[];
  resets: number;
}

function makeSink(): RecordingSink {
  return {
    writes: [],
    resizes: [],
    resets: 0,
    write(data) {
      this.writes.push(data);
    },
    resize(cols, rows) {
      this.resizes.push(`${cols}x${rows}`);
    },
    reset() {
      this.resets += 1;
      this.writes.push('<reset>');
    },
  };
}

function makeEvents(): CastEvent[] {
  return [
    { kind: 'o', time: 1, data: 'a' },
    { kind: 'o', time: 2, data: 'b' },
    { kind: 'r', time: 2, cols: 100, rows: 30 },
    { kind: 'o', time: 5, data: 'c' },
  ];
}

function makeScheduler(sink: RecordingSink, events = makeEvents()) {
  // 不注入定时器实现：vi.useFakeTimers 已接管全局 setTimeout/clearTimeout，
  // 调度器按默认路径使用（契约：注入面保留给生产/其他环境）。
  return new CastScheduler({ events, sink });
}

describe('CastScheduler', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('按时间差调度 o/r 事件，同刻事件同 tick 应用', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.play();

    vi.advanceTimersByTime(1000);
    expect(sink.writes).toEqual(['a']);

    vi.advanceTimersByTime(1000);
    // t=2 的 o 与 r 同刻：一次回调内先后应用。
    expect(sink.writes).toEqual(['a', 'b']);
    expect(sink.resizes).toEqual(['100x30']);

    // 距 t=5 还有 3s。
    vi.advanceTimersByTime(2999);
    expect(sink.writes).toEqual(['a', 'b']);
    vi.advanceTimersByTime(1);
    expect(sink.writes).toEqual(['a', 'b', 'c']);
    expect(scheduler.isPlaying).toBe(false);
    expect(scheduler.currentPosition).toBe(5);
  });

  it('播放到末尾触发 ended 回调', () => {
    const sink = makeSink();
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events: makeEvents(),
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.play();
    vi.advanceTimersByTime(5000);
    expect(scheduler.currentPosition).toBe(5);
    const last = ticks[ticks.length - 1];
    expect(last).toEqual([5, true]);
    // 播完后 play 不再排程（需 seek(0) 重放）。
    scheduler.play();
    vi.advanceTimersByTime(10_000);
    expect(sink.writes).toEqual(['a', 'b', 'c']);
  });

  it('暂停停止调度，恢复后从插值位置继续', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.play();
    vi.advanceTimersByTime(1500); // a 已落地；插值心跳把播放头推到 1.5
    scheduler.pause();
    // 暂停位置 = 墙钟插值位置（1.5），不是最后事件时间（1）——进度条不回跳。
    expect(scheduler.currentPosition).toBeCloseTo(1.5, 5);
    vi.advanceTimersByTime(10_000);
    expect(sink.writes).toEqual(['a']);

    scheduler.play();
    // 剩余 0.5s（1.5 → t=2）。
    vi.advanceTimersByTime(499);
    expect(sink.writes).toEqual(['a']);
    vi.advanceTimersByTime(1);
    expect(sink.writes).toEqual(['a', 'b']);
    expect(sink.resizes).toEqual(['100x30']);
  });

  it('2x 倍速：等待时间减半', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.setSpeed(2);
    scheduler.play();

    vi.advanceTimersByTime(500);
    expect(sink.writes).toEqual(['a']);
    vi.advanceTimersByTime(500);
    expect(sink.writes).toEqual(['a', 'b']);
  });

  it('播放中切倍速按剩余时间重排', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.play();
    vi.advanceTimersByTime(1000); // a 已出，下一事件 t=2 差 1s
    scheduler.setSpeed(4);
    vi.advanceTimersByTime(250);
    expect(sink.writes).toEqual(['a', 'b']);
  });

  it('seek：清屏后同步快进到目标时间', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.seek(2);
    expect(sink.resets).toBe(1);
    expect(sink.writes).toEqual(['<reset>', 'a', 'b']);
    expect(sink.resizes).toEqual(['100x30']);
    expect(scheduler.currentPosition).toBe(2);
    // 未播放：无定时器在跑。
    vi.advanceTimersByTime(10_000);
    expect(sink.writes).toEqual(['<reset>', 'a', 'b']);

    scheduler.play();
    vi.advanceTimersByTime(3000);
    expect(sink.writes).toEqual(['<reset>', 'a', 'b', 'c']);
  });

  it('seek 播放中：快进后继续调度；越界钳到时长并结束', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.play();
    scheduler.seek(4);
    expect(scheduler.currentPosition).toBe(4);
    vi.advanceTimersByTime(1000);
    expect(sink.writes).toEqual(['<reset>', 'a', 'b', 'c']);
    expect(scheduler.isPlaying).toBe(false);

    scheduler.play();
    scheduler.seek(99);
    expect(scheduler.currentPosition).toBe(5);
    expect(scheduler.isPlaying).toBe(false);
  });

  it('dispose 清定时器：后续 advance 不再写入；幂等', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink);
    scheduler.play();
    scheduler.dispose();
    scheduler.dispose(); // 幂等
    vi.advanceTimersByTime(60_000);
    expect(sink.writes).toEqual([]);
    // dispose 后 play/seek 均为 no-op。
    scheduler.play();
    scheduler.seek(1);
    expect(sink.resets).toBe(0);
  });

  it('duration 为 0（空事件）时 play 不排程', () => {
    const sink = makeSink();
    const scheduler = makeScheduler(sink, []);
    expect(scheduler.duration).toBe(0);
    scheduler.play();
    expect(scheduler.isPlaying).toBe(false);
    scheduler.seek(3);
    expect(scheduler.currentPosition).toBe(0);
  });
});

describe('播放态插值心跳（走查 w2：事件间静默段进度冻结）', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // 真实走查形态：t≈0.04 一批事件后，直到 t≈10.1 才有下一批（等命令
  // 输出/思考时间）——修复前 onTick 只在事件落地时回调，播放 2.5s 时间
  // 仍显示 0:00/0:10，播放器看起来像卡死。
  const quietEvents: CastEvent[] = [
    { kind: 'o', time: 0.04, data: 'a' },
    { kind: 'o', time: 10.1, data: 'b' },
  ];

  it('静默段 250ms 心跳按墙钟插值连续 onTick，position 持续推进', () => {
    const sink = makeSink();
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events: quietEvents,
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.play();
    vi.advanceTimersByTime(1000); // 事件 0.04 落地 + 心跳 0.25/0.5/0.75/1.0
    expect(sink.writes).toEqual(['a']);
    // 事件落地一次 + 4 次插值心跳（4 次/秒），位置单调推进。
    const positions = ticks.filter(([, ended]) => !ended).map(([position]) => position);
    expect(positions).toHaveLength(5);
    expect(positions[0]).toBe(0.04); // 事件落地：权威精确值
    expect(positions[1]).toBeCloseTo(0.25, 6); // 插值：墙钟换算（浮点近似）
    expect(positions[2]).toBeCloseTo(0.5, 6);
    expect(positions[3]).toBeCloseTo(0.75, 6);
    expect(positions[4]).toBeCloseTo(1.0, 6);
    expect(ticks.every(([, ended]) => !ended)).toBe(true); // 未播完
    expect(scheduler.currentPosition).toBeCloseTo(1.0, 6);
  });

  it('2x 倍速下插值速度翻倍', () => {
    const sink = makeSink();
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events: quietEvents,
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.setSpeed(2);
    scheduler.play();
    vi.advanceTimersByTime(1000); // 心跳 250/500/750/1000ms → 0.5/1.0/1.5/2.0
    const positions = ticks.filter(([, ended]) => !ended).map(([position]) => position);
    expect(positions).toHaveLength(5);
    expect(positions[0]).toBe(0.04); // 事件落地（20ms 处，2x）
    expect(positions[1]).toBeCloseTo(0.5, 6);
    expect(positions[2]).toBeCloseTo(1.0, 6);
    expect(positions[3]).toBeCloseTo(1.5, 6);
    expect(positions[4]).toBeCloseTo(2.0, 6);
  });

  it('暂停即停心跳；恢复后从插值位置继续插值', () => {
    const sink = makeSink();
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events: quietEvents,
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.play();
    vi.advanceTimersByTime(500);
    ticks.length = 0;
    scheduler.pause();
    vi.advanceTimersByTime(2000);
    expect(ticks).toEqual([]); // 心跳已停：暂停期间无 onTick
    expect(scheduler.currentPosition).toBeCloseTo(0.5, 5);

    scheduler.play();
    vi.advanceTimersByTime(250);
    const positions = ticks.filter(([, ended]) => !ended).map(([position]) => position);
    expect(positions).toHaveLength(1);
    expect(positions[0]).toBeCloseTo(0.75, 6); // 从 0.5 恢复插值
  });

  it('插值不越过下一个事件与总时长（事件落地回调是权威推进点）', () => {
    const sink = makeSink();
    const events: CastEvent[] = [
      { kind: 'o', time: 0.9, data: 'a' },
      { kind: 'o', time: 1.0, data: 'b' },
      { kind: 'o', time: 30, data: 'c' },
    ];
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events,
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.play();
    vi.advanceTimersByTime(1250);
    // a、b 已落地；插值封顶到下一事件 c 的 t=30，但心跳推进不越权。
    expect(sink.writes).toEqual(['a', 'b']);
    const positions = ticks.map(([position]) => position);
    expect(Math.max(...positions)).toBeLessThanOrEqual(1.25);
    expect(scheduler.currentPosition).toBeCloseTo(1.25, 5);
  });

  it('播完即停：事件全部落地后心跳停止，无额外 onTick', () => {
    const sink = makeSink();
    const events: CastEvent[] = [
      { kind: 'o', time: 0.2, data: 'a' },
      { kind: 'o', time: 0.4, data: 'b' },
    ];
    const ticks: Array<[number, boolean]> = [];
    const scheduler = new CastScheduler({
      events,
      sink,
      onTick: (position, ended) => ticks.push([position, ended]),
    });
    scheduler.play();
    vi.advanceTimersByTime(600); // 0.4s 全部落地 → ended
    ticks.length = 0;
    vi.advanceTimersByTime(5000);
    expect(ticks).toEqual([]);
    expect(scheduler.isPlaying).toBe(false);
    expect(scheduler.currentPosition).toBe(0.4);
  });
});

describe('墙钟漂移校正（评审发现：理论间隔 setTimeout 链累积触发延迟）', () => {
  it('每个 tick 实际晚 4ms（嵌套 setTimeout 下限）：密集事件按绝对 deadline 调度，墙钟不漂移', async () => {
    // 200 个 1ms 间隔事件，理论时长 0.2s。修复前：每 tick 晚 4ms 累积，
    // 走完全程需 ~1s（评审实测 999ms）；修复后：deadline 调度扣回延迟，
    // 全程墙钟 ≈ 理论时长 + 单次下限。
    const events: CastEvent[] = Array.from({ length: 200 }, (_, i) => ({
      time: (i + 1) / 1000,
      kind: 'o' as const,
      data: 'x',
    }));
    const sink = makeSink();

    // 手动时钟：setTimeout(handler, ms) 在 max(ms, 4) 后执行 handler 并推进 4ms。
    let clock = 0;
    const pending: Array<{ at: number; handler: () => void }> = [];
    const setTimeoutFn = (handler: () => void, ms: number) => {
      pending.push({ at: clock + Math.max(ms, 4), handler });
      return pending.length - 1;
    };
    const clearTimeoutFn = (handle: unknown) => {
      const idx = handle as number;
      if (idx >= 0 && idx < pending.length) {
        pending[idx] = { at: -1, handler: () => {} };
      }
    };
    const drain = async () => {
      for (;;) {
        const next = pending.filter((p) => p.at > clock).sort((a, b) => a.at - b.at)[0];
        if (!next) {
          return;
        }
        clock = next.at;
        next.handler();
        await Promise.resolve();
      }
    };

    const scheduler = new CastScheduler({
      events,
      sink,
      setTimeoutFn,
      clearTimeoutFn,
      nowFn: () => clock,
    });
    scheduler.play();
    await drain();

    const wallMs = clock;
    expect(wallMs).toBeLessThan(250); // 修复前实测 ~999ms
    expect(sink.writes).toHaveLength(200);
    expect(scheduler.currentPosition).toBeCloseTo(0.2, 5);
  });
});
