import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
// formatRemaining / isCancelledApproval 为 ApprovalCard 私有纯函数，
// export 仅作单测钩子（审批状态机展示逻辑，无行为变化）。
import { formatRemaining, isCancelledApproval } from '../../../components/ApprovalCard';
import { APPROVAL_STATUS_LABELS } from '../../../components/approvalStatus';

type Approval = components['schemas']['Approval'];

// 最小可用 Approval 工厂：仅 status/note 参与状态机判定。
function approvalOf(overrides: Partial<Pick<Approval, 'status' | 'note'>>): Approval {
  return {
    id: 'ap1',
    conversation_id: 'c1',
    tool_call_id: 'tc1',
    source: 'chat',
    server_id: 's1',
    server_name: 'web-1',
    command: 'systemctl restart nginx',
    risk_level: 'needs_approval',
    rule_id: 'RM_ANY',
    reason: 'risk',
    status: 'pending',
    requested_at: '2026-01-01T00:00:00Z',
    expires_at: '2026-01-01T00:02:00Z',
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

// formatRemaining：审批卡倒计时（剩余时间）的展示格式。
describe('formatRemaining 审批倒计时格式', () => {
  it('整分钟：90s → 1:30', () => {
    expect(formatRemaining(90_000)).toBe('1:30');
  });

  it('整分钟：60s → 1:00（进位到分钟）', () => {
    expect(formatRemaining(60_000)).toBe('1:00');
  });

  it('不足一分钟：59s → 0:59；秒数补零', () => {
    expect(formatRemaining(59_000)).toBe('0:59');
    expect(formatRemaining(5_000)).toBe('0:05');
  });

  it('毫秒向上取整到秒：4999ms → 0:05', () => {
    expect(formatRemaining(4999)).toBe('0:05');
    expect(formatRemaining(1)).toBe('0:01');
  });

  it('0 与负数夹到 0:00（已到/超过截止时间）', () => {
    expect(formatRemaining(0)).toBe('0:00');
    expect(formatRemaining(-1234)).toBe('0:00');
  });

  it('大值正常展示分钟：120s → 2:00；61s → 1:01', () => {
    expect(formatRemaining(120_000)).toBe('2:00');
    expect(formatRemaining(61_000)).toBe('1:01');
  });
});

// isCancelledApproval：expired 的两个来源区分（TTL 超时 vs 流断开/服务关闭连带取消）。
describe('isCancelledApproval 审批取消判定矩阵', () => {
  it('expired + stream_cancelled → 取消（流断开连带取消）', () => {
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: 'stream_cancelled' }))).toBe(
      true,
    );
  });

  it('expired + server_shutting_down → 取消（服务关闭连带取消）', () => {
    expect(
      isCancelledApproval(approvalOf({ status: 'expired', note: 'server_shutting_down' })),
    ).toBe(true);
  });

  it('expired + null note → 不是取消（TTL 到期的正常超时）', () => {
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: '' }))).toBe(false);
  });

  it('expired + 未知 note → 保守地不判为取消（原样保留排查信息）', () => {
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: 'something_new' }))).toBe(
      false,
    );
  });

  it('非 expired 状态即使带取消 note 也不判为取消', () => {
    expect(isCancelledApproval(approvalOf({ status: 'pending', note: 'stream_cancelled' }))).toBe(
      false,
    );
    expect(
      isCancelledApproval(approvalOf({ status: 'approved', note: 'server_shutting_down' })),
    ).toBe(false);
    expect(isCancelledApproval(approvalOf({ status: 'denied', note: '' }))).toBe(false);
  });
});

// 六期打磨 sprint 4（走查 low）：同一张审批卡上 Tag 与正文必须同一措辞。
// 走查实测 TTL 到期后工具卡 Tag 集合为「已过期」而审批卡正文写「已超时」
// （StatusTag 与 replayPlayer 早已统一为「已过期」，只有 ApprovalCard 的
// 私有表漏改）。此处直接断言表内容与一致性来源。
describe('APPROVAL_STATUS_LABELS 审批状态措辞（p6 sprint 4）', () => {
  it('expired 用中性「已过期」，与 StatusTag / 回放同源', () => {
    expect(APPROVAL_STATUS_LABELS.expired).toBe('已过期');
    // 不得再出现「已超时」——approvals.expired 也承载流断开取消
    // （note=stream_cancelled），说「超时」与卡上的「已取消」矛盾。
    expect(Object.values(APPROVAL_STATUS_LABELS)).not.toContain('已超时');
  });

  it('四态文案齐全且无 ASCII 残留', () => {
    expect(APPROVAL_STATUS_LABELS).toEqual({
      pending: '等待处理',
      approved: '已批准',
      denied: '已拒绝',
      expired: '已过期',
    });
    for (const label of Object.values(APPROVAL_STATUS_LABELS)) {
      expect(label).not.toMatch(/[a-z]/i);
    }
  });
});
