import { describe, expect, it } from 'vitest';

import type { components } from '../../api/schema';
import { approvalOutcome } from './approvalOutcome';

type Approval = components['schemas']['Approval'];
type ApprovalExecution = components['schemas']['ApprovalExecution'];

function approvalOf(overrides: Partial<Approval> = {}): Approval {
  return {
    id: '11111111-1111-4111-8111-111111111111',
    conversation_id: null,
    tool_call_id: null,
    source: 'console',
    server_id: '22222222-2222-4222-8222-222222222222',
    server_name: 'web-1',
    command: 'uptime',
    risk_level: 'needs_approval',
    rule_id: '',
    reason: 'rev8 console exec ui',
    status: 'approved',
    approval_mode: 'standard',
    requested_at: '2026-01-01T00:00:00Z',
    expires_at: '2026-01-01T00:02:00Z',
    decided_at: '2026-01-01T00:00:10Z',
    decided_by: '33333333-3333-4333-8333-333333333333',
    note: '',
    edited_command: '',
    edited_risk_level: '',
    edited_rule_id: '',
    requested_by: '44444444-4444-4444-8444-444444444444',
    requested_by_name: 'admin',
    break_glass: false,
    legacy: false,
    ...overrides,
  };
}

function executionOf(overrides: Partial<ApprovalExecution> = {}): ApprovalExecution {
  return {
    exit_code: 0,
    output: '',
    truncated: false,
    duration_ms: 12,
    ...overrides,
  };
}

describe('approvalOutcome（第 8 轮：decide 响应的执行结果）', () => {
  it('execution=null（chat/policy/terminal 审批或拒绝）只给状态文案，无执行明细', () => {
    expect(approvalOutcome(approvalOf({ source: 'chat' }), null)).toEqual({
      kind: 'success',
      message: '已批准',
      detail: '',
    });
    expect(approvalOutcome(approvalOf({ status: 'denied' }), null)).toEqual({
      kind: 'denied',
      message: '已拒绝，命令未执行',
      detail: '',
    });
  });

  it('console 批准执行成功：exit 0 的人话文案 + exit_code 明细', () => {
    const outcome = approvalOutcome(approvalOf(), executionOf({ exit_code: 0, duration_ms: 12 }));
    expect(outcome.kind).toBe('success');
    expect(outcome.message).toContain('执行成功（exit 0');
    expect(outcome.detail).toBe('exit_code=0');
  });

  it('console 批准执行失败（通道失败 -1）：展示 error_code，明细含 exit_code/error_code', () => {
    const outcome = approvalOutcome(
      approvalOf(),
      executionOf({ exit_code: -1, duration_ms: 0, error_code: 'SSH_EXEC_FAILED' }),
    );
    expect(outcome.kind).toBe('failed');
    expect(outcome.message).toContain('执行失败');
    expect(outcome.message).toContain('SSH_EXEC_FAILED');
    expect(outcome.detail).toBe('exit_code=-1 · error_code=SSH_EXEC_FAILED');
  });

  it('console 批准执行非 0 退出码：明细同时带 exit_code 与错误码', () => {
    const outcome = approvalOutcome(
      approvalOf(),
      executionOf({ exit_code: 2, duration_ms: 40, error_code: 'CONTAINER_CLI_FAILED' }),
    );
    expect(outcome.kind).toBe('failed');
    expect(outcome.message).toContain('exit 2');
    expect(outcome.detail).toBe('exit_code=2 · error_code=CONTAINER_CLI_FAILED');
  });
});
