import { describe, expect, it } from 'vitest';

import { isStreamActive, type LiveTurn, type StreamPhase } from '../useChatStream';

// 走查第 3 轮 medium：回答正文已完整上屏后「流式进行中」滞留 30–45ms
// （负载下可达秒级），期间点「新建会话」被弹「当前回答仍在进行…」。
// isStreamActive 是页面守卫/Composer/离开 blocker 的共用判定：收到
// done/error/stopped 帧即视为回答结束，不再等 phase 在 finally 回落。

function turn(overrides: Partial<LiveTurn> = {}): LiveTurn {
  return {
    userContent: '问题',
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
    persisted: true,
    startedAt: Date.now(),
    ...overrides,
  };
}

const donePayload = { message_id: 'm-1', turn_id: 't-1' } as LiveTurn['done'];

describe('isStreamActive（done 帧即结束，不等 phase 回落）', () => {
  it('connecting/streaming 且 live 未终态 → 仍算进行中', () => {
    expect(isStreamActive('connecting', turn())).toBe(true);
    expect(isStreamActive('streaming', turn())).toBe(true);
  });

  it('phase 仍是 streaming 但已收到 done 帧 → 已结束（核心回归）', () => {
    expect(isStreamActive('streaming', turn({ done: donePayload }))).toBe(false);
  });

  it('已收到 error 帧或已停止 → 已结束', () => {
    expect(
      isStreamActive(
        'streaming',
        turn({
          error: {
            code: 'SSH_EXEC_FAILED',
            message: '',
            detail: '',
            requestId: '',
            retryable: false,
          },
        }),
      ),
    ).toBe(false);
    expect(isStreamActive('streaming', turn({ stopped: true }))).toBe(false);
  });

  it('phase 已回落 idle → 无论 live 如何都已结束', () => {
    for (const phase of ['idle'] as StreamPhase[]) {
      expect(isStreamActive(phase, turn())).toBe(false);
      expect(isStreamActive(phase, null)).toBe(false);
    }
  });

  it('connecting 阶段 live 为 null → 仍算进行中（请求已发出）', () => {
    expect(isStreamActive('connecting', null)).toBe(true);
  });
});
