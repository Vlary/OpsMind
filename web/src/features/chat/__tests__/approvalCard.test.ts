import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
// formatRemaining / isCancelledApproval 为 ApprovalCard 私有纯函数，
// export 仅作单测钩子（审批状态机展示逻辑，无行为变化）。
import {
  approvalApproveBlockedByDegradedMode,
  approvalBodyKind,
  formatRemaining,
  isCancelledApproval,
} from '../../../components/ApprovalCard';
import { APPROVAL_STATUS_LABELS } from '../../../components/approvalStatus';
// 审批操作区收敛判定已抽到 lib（三处 console/batch 入口共用）。
import { approvalDecisionState } from '../../../lib/approvalDecision';
import { PERMS } from '../../../lib/rbacPermissions';
import { describeError } from '../useChatStream';

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
    requested_by: 'u1',
    requested_by_name: 'admin',
    break_glass: false,
    legacy: false,
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

// RBAC 走查 sprint 1（high）：运维执行者/自审发起人曾能看到批准/拒绝，
// 点击后才拿 403。操作区必须同时满足「有 approvals:decide」与
// 「不是发起人」，否则给出等待/自审说明而不是可点按钮。
describe('approvalDecisionState 审批操作区收敛', () => {
  const decide = [PERMS.approvalsDecide];

  it('无审批权限：等待审批者处理（不渲染按钮）', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: [PERMS.chat, PERMS.execWrite],
        requestedBy: 'u1',
        currentUserId: 'u1',
      }),
    ).toBe('waiting');
  });

  it('只有 approvals:read 也不足以决策', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: [PERMS.approvalsRead],
        requestedBy: 'u1',
        currentUserId: 'u2',
      }),
    ).toBe('waiting');
  });

  it('有审批权限且不是发起人：渲染批准/拒绝', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: decide,
        requestedBy: 'u1',
        currentUserId: 'u2',
      }),
    ).toBe('actions');
  });

  it('有审批权限但就是发起人：不能自审', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: decide,
        requestedBy: 'u1',
        currentUserId: 'u1',
      }),
    ).toBe('self');
  });

  it('身份未就绪（currentUserId 为空）失败关闭为 self', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: decide,
        requestedBy: 'u1',
        currentUserId: '',
      }),
    ).toBe('self');
  });

  it('历史审批 requested_by 为空时按非自审处理（不阻断审批者）', () => {
    expect(
      approvalDecisionState({
        status: 'pending',
        permissions: decide,
        requestedBy: '',
        currentUserId: 'u2',
      }),
    ).toBe('actions');
  });

  it('非 pending 状态一律 resolved（不再显示任何操作入口）', () => {
    for (const status of ['approved', 'denied', 'expired'] as const) {
      expect(
        approvalDecisionState({
          status,
          permissions: decide,
          requestedBy: 'u1',
          currentUserId: 'u2',
        }),
      ).toBe('resolved');
    }
  });
});

// 第 2 轮走查（medium）：policy 审批（命令恒空串）此前也渲染「待执行命令」
// + 空代码块，审批者以为漏了命令。正文分区必须按 source 收敛。
describe('approvalBodyKind 审批正文分区（第 2 轮走查）', () => {
  it('policy 只渲染策略，不渲染命令区', () => {
    expect(approvalBodyKind('policy')).toBe('policy');
  });

  it('terminal 只渲染目标与授权时长，不渲染命令区', () => {
    expect(approvalBodyKind('terminal')).toBe('terminal');
  });

  it('chat/console/batch 仍渲染命令区（命令非空）', () => {
    expect(approvalBodyKind('chat')).toBe('command');
    expect(approvalBodyKind('console')).toBe('command');
    expect(approvalBodyKind('batch')).toBe('command');
  });
});

// 走查第 3 轮 medium：单管理员降级时「批准」必须在点之前就置灰并给出中文
// 说明（此前只能撞一次英文 409 NO_SECOND_APPROVER）。未知降级态不得禁用。
describe('approvalApproveBlockedByDegradedMode 降级禁用判定', () => {
  it('pending + 明确降级 → 禁用批准', () => {
    expect(approvalApproveBlockedByDegradedMode('pending', true)).toBe(true);
  });

  it('非 pending 状态不涉及批准按钮', () => {
    expect(approvalApproveBlockedByDegradedMode('approved', true)).toBe(false);
    expect(approvalApproveBlockedByDegradedMode('expired', true)).toBe(false);
  });

  it('未降级或降级态未知（settings 未加载/存储不可用）→ 不禁用，权威判定留在服务端', () => {
    expect(approvalApproveBlockedByDegradedMode('pending', false)).toBe(false);
    expect(approvalApproveBlockedByDegradedMode('pending', undefined)).toBe(false);
  });
});

// NO_SECOND_APPROVER 等 RBAC 错误码必须有中文文案（describeError 被审批卡
// 与聊天链路共用，缺映射就会把后端英文 message 直接上屏）。
describe('describeError RBAC 错误码中文映射', () => {
  it('NO_SECOND_APPROVER 中文说明含恢复动作', () => {
    const text = describeError('NO_SECOND_APPROVER');
    // 第 11 轮走查 low：口径必须与 CountEnabledApprovers（admin+approver
    // 合计）同源，不能写成「仅一个启用管理员」。
    expect(text).toContain('管理员');
    expect(text).toContain('审批者');
    expect(text).toContain('不足 2 个');
    expect(text).toContain('break-glass');
  });

  it('DELIVERY_UNAVAILABLE / 自审 / 最后管理员等均映射中文', () => {
    expect(describeError('DELIVERY_UNAVAILABLE')).toContain('SMTP');
    expect(describeError('APPROVAL_SELF_DECISION')).toContain('自己发起');
    expect(describeError('LAST_ADMIN')).toContain('最后一个管理员');
  });
});
