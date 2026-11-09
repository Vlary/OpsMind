import { describe, expect, it } from 'vitest';

import { showsDuration, type ToolCallView } from '../useChatStream';

// 走查 B3 回归：denied/expired 终态的命令从未执行（审批被拒/超时/策略禁止），
// 45s 级审批等待不得作为「执行耗时」展示在工具卡上。
function view(overrides: Partial<ToolCallView>): ToolCallView {
  return {
    id: 'tc-1',
    name: 'ssh_exec',
    arguments: {},
    result: null,
    status: 'succeeded',
    riskLevel: '',
    errorCode: '',
    errorMessage: '',
    durationMs: 0,
    approvalMode: '',
    ...overrides,
  };
}

describe('showsDuration 工具卡耗时展示', () => {
  it('expired：审批超时（命令从未执行）不展示 45s 级等待耗时', () => {
    expect(
      showsDuration(view({ status: 'expired', durationMs: 45_018, errorCode: 'APPROVAL_TIMEOUT' })),
    ).toBe(false);
  });

  it('denied：审批被拒不展示耗时（无论数字大小）', () => {
    expect(
      showsDuration(view({ status: 'denied', durationMs: 45_018, errorCode: 'APPROVAL_DENIED' })),
    ).toBe(false);
  });

  it('denied：策略禁止（COMMAND_FORBIDDEN）同样不展示', () => {
    expect(
      showsDuration(view({ status: 'denied', durationMs: 12, errorCode: 'COMMAND_FORBIDDEN' })),
    ).toBe(false);
  });

  it('succeeded：真实执行耗时正常展示', () => {
    expect(showsDuration(view({ status: 'succeeded', durationMs: 170 }))).toBe(true);
  });

  it('failed：真实执行过但失败（如 SSH_EXEC_FAILED）保留耗时', () => {
    expect(
      showsDuration(view({ status: 'failed', durationMs: 3_200, errorCode: 'SSH_EXEC_FAILED' })),
    ).toBe(true);
  });

  it('零耗时不展示（沿用原 durationMs > 0 门槛）', () => {
    expect(showsDuration(view({ status: 'succeeded', durationMs: 0 }))).toBe(false);
  });
});
