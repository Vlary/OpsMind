import { afterEach, describe, expect, it, vi } from 'vitest';

import { APPROVAL_REFETCH_DELAYS_MS, scheduleApprovalRefetch } from '../useChatStream';

// 第 10 轮打磨：非流发起窗口（第二个浏览器窗口打开同一会话）里审批
// decide 成功后，卡片终态靠 approvals invalidate 同步，但后续 tool_result
// 与最终回答依赖流连接内的 done 帧触发 refetch——该窗口没有流连接也没有
// 轮询，结果只能手动刷新。applyApproval 现按 APPROVAL_REFETCH_DELAYS_MS
// 做延迟补偿 invalidate（messages/toolCalls），调度行为在此矩阵回归。

interface ScheduledCall {
  fn: () => void;
  delayMs: number;
}

function collectingSchedule(): { calls: ScheduledCall[]; run: (index: number) => void } {
  const calls: ScheduledCall[] = [];
  return {
    calls,
    run: (index) => calls[index]?.fn(),
  };
}

describe('APPROVAL_REFETCH_DELAYS_MS 延迟点（第 10 轮）', () => {
  it('三个延迟点且递增，覆盖后端决策后的落库窗口', () => {
    expect([...APPROVAL_REFETCH_DELAYS_MS]).toHaveLength(3);
    expect([...APPROVAL_REFETCH_DELAYS_MS]).toEqual([2_000, 5_000, 10_000]);
  });
});

describe('scheduleApprovalRefetch 调度行为', () => {
  it('按每个延迟点各调度一次', () => {
    const { calls } = collectingSchedule();
    scheduleApprovalRefetch(
      'c1',
      () => {},
      (fn, delayMs) => {
        calls.push({ fn, delayMs });
        return null as unknown as ReturnType<typeof setTimeout>;
      },
    );
    expect(calls.map((call) => call.delayMs)).toEqual([2_000, 5_000, 10_000]);
  });

  it('回调触发时 invalidate 收到原会话 id（切会话后也不串）', () => {
    const { calls, run } = collectingSchedule();
    const received: string[] = [];
    scheduleApprovalRefetch(
      'conv-b',
      (cid) => received.push(cid),
      (fn, delayMs) => {
        calls.push({ fn, delayMs });
        return null as unknown as ReturnType<typeof setTimeout>;
      },
    );
    run(0);
    run(1);
    run(2);
    expect(received).toEqual(['conv-b', 'conv-b', 'conv-b']);
  });

  it('invalidate 抛错不阻断后续延迟点（各回调独立）', () => {
    const { calls, run } = collectingSchedule();
    let fired = 0;
    const failing = (cid: string): void => {
      fired += 1;
      if (fired === 1) {
        throw new Error(`boom ${cid}`);
      }
    };
    scheduleApprovalRefetch('c9', failing, (fn, delayMs) => {
      calls.push({ fn, delayMs });
      return null as unknown as ReturnType<typeof setTimeout>;
    });
    expect(() => run(0)).toThrow('boom c9');
    expect(() => run(1)).not.toThrow();
    expect(fired).toBe(2);
  });
});

describe('scheduleApprovalRefetch 默认调度器（真实 setTimeout 路径）', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('各延迟点到期后触发 invalidate，与延迟点一一对应', () => {
    vi.useFakeTimers();
    const received: string[] = [];
    scheduleApprovalRefetch('conv-x', (cid) => received.push(cid));
    expect(received).toEqual([]);
    vi.advanceTimersByTime(2_000);
    expect(received).toEqual(['conv-x']);
    vi.advanceTimersByTime(3_000);
    expect(received).toEqual(['conv-x', 'conv-x']);
    vi.advanceTimersByTime(5_000);
    expect(received).toEqual(['conv-x', 'conv-x', 'conv-x']);
    // 全部延迟点结束后不再触发。
    vi.advanceTimersByTime(60_000);
    expect(received).toHaveLength(3);
  });
});
