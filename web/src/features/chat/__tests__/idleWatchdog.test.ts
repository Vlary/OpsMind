import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { components } from '../../../api/schema';
import {
  IdleWatchdog,
  STREAM_IDLE_TIMEOUT_MS,
  shouldHoldIdleWatchdog,
  type LiveTurn,
} from '../useChatStream';

type Approval = components['schemas']['Approval'];

const NOW = Date.parse('2026-01-01T00:10:00Z');

function liveOf(overrides: Partial<LiveTurn> = {}): LiveTurn {
  return {
    userContent: '问',
    text: '',
    reasoning: '',
    reasoningStartedAt: null,
    reasoningEndedAt: null,
    tools: [],
    approvals: [],
    error: null,
    done: null,
    stopped: false,
    settled: false,
    persisted: false,
    startedAt: NOW - 1000,
    ...overrides,
  };
}

function approvalOf(overrides: Partial<Approval>): Approval {
  return {
    id: 'ap1',
    conversation_id: 'c1',
    tool_call_id: 'tc1',
    source: 'chat',
    server_id: 's1',
    server_name: 'web-1',
    command: 'uptime',
    risk_level: 'needs_approval',
    rule_id: 'RM_ANY',
    reason: 'risk',
    status: 'pending',
    requested_at: '2026-01-01T00:09:00Z',
    expires_at: '2026-01-01T00:12:00Z',
    decided_at: null,
    decided_by: null,
    note: '',
    approval_mode: 'standard',
    edited_command: '',
    edited_risk_level: '',
    edited_rule_id: '',
    ...overrides,
  };
}

// 看门狗 fire 时的续期判定：审批等待 / 工具执行 / 首 token 前都是合法的
// 长无帧窗口，不能按固定 30s 一刀切（否则 E2E 审批等待用例即被误杀）。
describe('shouldHoldIdleWatchdog 合法无帧窗口', () => {
  it('无任何等待状态且已收到过帧：不续期（判定挂死）', () => {
    expect(shouldHoldIdleWatchdog(liveOf(), NOW, true)).toBe(false);
  });

  it('live 为 null：不续期', () => {
    expect(shouldHoldIdleWatchdog(null, NOW, true)).toBe(false);
  });

  it('待处理审批未过期：续期等待用户决策', () => {
    const live = liveOf({ approvals: [approvalOf({ status: 'pending' })] });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('待处理审批过期超过缓冲（后端应已补发 expired 帧）：不续期', () => {
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: '2026-01-01T00:05:00Z' })],
    });
    // NOW 与过期时刻相差 5 分钟，远超 5s 缓冲。
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('审批刚过期但在缓冲窗口内：仍续期（等 approval_resolved 帧到达）', () => {
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: '2026-01-01T00:09:58Z' })],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('已决策审批（approved/denied/expired）不算等待：不续期', () => {
    const live = liveOf({ approvals: [approvalOf({ status: 'approved' })] });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('started 工具在 SSH 上限（300s）+ 余量内：续期', () => {
    const live = liveOf({
      tools: [
        {
          id: 'tc1',
          providerCallId: '',
          name: 'ssh_exec',
          arguments: {},
          result: null,
          error: null,
          status: 'started',
          riskLevel: '',
          durationMs: 0,
          approvalId: '',
          approvalMode: '',
          startedAt: NOW - 300_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
    // 超过 330s 仍未有 tool_result：连接挂死，不续期。
    const dead = liveOf({
      tools: [
        {
          id: 'tc1',
          providerCallId: '',
          name: 'ssh_exec',
          arguments: {},
          result: null,
          error: null,
          status: 'started',
          riskLevel: '',
          durationMs: 0,
          approvalId: '',
          approvalMode: '',
          startedAt: NOW - 331_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(dead, NOW, true)).toBe(false);
  });

  it('awaiting_approval 工具同样续期（审批等待的工具体态）', () => {
    const live = liveOf({
      tools: [
        {
          id: 'tc1',
          providerCallId: '',
          name: 'ssh_exec',
          arguments: {},
          result: null,
          error: null,
          status: 'awaiting_approval',
          riskLevel: 'needs_approval',
          durationMs: 0,
          approvalId: 'ap1',
          approvalMode: '',
          startedAt: NOW - 60_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('首帧之前（LLM 首 token 慢）：在后端 LLM 超时（180s）+ 余量内续期', () => {
    expect(shouldHoldIdleWatchdog(liveOf(), NOW, false)).toBe(true); // 仅 1s
    const slow = liveOf({ startedAt: NOW - 209_000 });
    expect(shouldHoldIdleWatchdog(slow, NOW, false)).toBe(true); // 209s < 210s
    const tooSlow = liveOf({ startedAt: NOW - 211_000 });
    expect(shouldHoldIdleWatchdog(tooSlow, NOW, false)).toBe(false); // 超过 210s
  });

  it('已收到过帧后不再适用首帧宽限（token 间隙挂死按 30s 判）', () => {
    const live = liveOf({ startedAt: NOW - 100_000, text: '部分回答' });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });
});

// 走查 A2（SSE 经反向代理被挂住）：空闲看门狗是浏览器侧唯一的兜底。
// 这里用假时钟验证它的计时语义：超时触发一次、arm 重置、cancel 抑制。
describe('IdleWatchdog', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('arm 后到达超时触发一次 onFire', () => {
    const onFire = vi.fn();
    const watchdog = new IdleWatchdog(30_000, onFire);
    watchdog.arm();

    expect(onFire).not.toHaveBeenCalled();
    vi.advanceTimersByTime(29_999);
    expect(onFire).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(onFire).toHaveBeenCalledTimes(1);
  });

  it('触发后不再重复；再次 arm 重新开始计时', () => {
    const onFire = vi.fn();
    const watchdog = new IdleWatchdog(1000, onFire);
    watchdog.arm();
    vi.advanceTimersByTime(2000);
    expect(onFire).toHaveBeenCalledTimes(1);

    watchdog.arm();
    vi.advanceTimersByTime(999);
    expect(onFire).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(onFire).toHaveBeenCalledTimes(2);
  });

  it('超时前再次 arm 重置计时窗口（每帧续期语义）', () => {
    const onFire = vi.fn();
    const watchdog = new IdleWatchdog(1000, onFire);

    watchdog.arm();
    vi.advanceTimersByTime(900);
    watchdog.arm(); // 第 900ms 收到新帧
    vi.advanceTimersByTime(900);
    expect(onFire).not.toHaveBeenCalled(); // 距新帧仅 900ms

    vi.advanceTimersByTime(100);
    expect(onFire).toHaveBeenCalledTimes(1); // 距新帧满 1000ms
  });

  it('cancel 后不再触发；cancel 幂等', () => {
    const onFire = vi.fn();
    const watchdog = new IdleWatchdog(1000, onFire);
    watchdog.arm();
    watchdog.cancel();
    watchdog.cancel();
    vi.advanceTimersByTime(5000);
    expect(onFire).not.toHaveBeenCalled();
  });

  it('未 arm 过的 watchdog 直接 cancel 不抛错', () => {
    expect(() => new IdleWatchdog(1000, vi.fn()).cancel()).not.toThrow();
  });

  it('产品超时为 30s：高于审批/首 token 的正常帧间隔，又不过分漫长', () => {
    expect(STREAM_IDLE_TIMEOUT_MS).toBe(30_000);
  });
});
