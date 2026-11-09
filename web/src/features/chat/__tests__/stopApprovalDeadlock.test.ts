import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { cancelPendingApprovals, type LiveTurn } from '../useChatStream';
import { mergeApprovalsByTool } from '../MessageList';

type Approval = components['schemas']['Approval'];

// 第 4 轮打磨回归：走查复现（/tmp/walk4/dbg3.mjs）——停止含未决审批的
// 回答后，审批卡在 TTL 内死锁显示「待处理」并继续倒计时，批准按钮可点
// 但必返回 409，本地倒计时归零后又误显「已超时」。
//
// 根因有二，修复拆成两个互补纯函数（本文件矩阵回归）：
//
// 1. cancelPendingApprovals（useChatStream.stop() 调用）：stop() 只置
//    live.stopped，live.approvals 里残留 pending 快照；服务端此时已把
//    审批 Cancel 为 expired + stream_cancelled（internal/agent/runner.go），
//    但前端 SSE 已断收不到帧。stop() 同步写入服务端同款终态，卡片立即
//    显示「已取消（回答流已中断，审批未生效）」。
// 2. mergeApprovalsByTool（MessageList mergedApprovalsByTool 本体）：
//    live 快照按同 id 无条件覆盖落库状态。多窗口场景下另一窗口停止
//    回答，本窗口 live 里仍是 pending，会压住落库的 expired 终态——
//    终态优先规则堵住该路径；live 终态（决策回填 / approval_resolved
//    帧）仍可覆盖落库任意状态，即时反馈不受影响。

const NOW = '2026-10-25T12:00:00Z';

function approvalOf(overrides: Partial<Approval> = {}): Approval {
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

describe('cancelPendingApprovals 停止时的终态写入', () => {
  it('pending 置为 expired + stream_cancelled + decided_at（服务端 Cancel 同款终态）', () => {
    const [first] = cancelPendingApprovals([approvalOf()], NOW);
    expect(first).toMatchObject({ status: 'expired', note: 'stream_cancelled', decided_at: NOW });
  });

  it('pending 之外的字段原样保留（命令、服务器、规则等不丢）', () => {
    const source = approvalOf({ id: 'ap-keep', command: 'df -h', server_name: 'db-9' });
    const kept = cancelPendingApprovals([source], NOW).find((item) => item.id === 'ap-keep');
    expect(kept).toMatchObject({ command: 'df -h', server_name: 'db-9' });
    expect(kept?.expires_at).toBe(source.expires_at);
  });

  it('非 pending 审批不动（approved/denied/expired 均保持原终态）', () => {
    const approvals = [
      approvalOf({
        id: 'a-ok',
        status: 'approved',
        decided_at: '2026-01-01T00:01:00Z',
        decided_by: 'u1',
      }),
      approvalOf({ id: 'a-no', status: 'denied' }),
      approvalOf({ id: 'a-ttl', status: 'expired', note: '' }),
    ];
    expect(cancelPendingApprovals(approvals, NOW)).toEqual(approvals);
  });

  it('混合列表：只改 pending 项，终态项逐字段相等', () => {
    const pending = approvalOf({ id: 'a-pending' });
    const approved = approvalOf({ id: 'a-approved', status: 'approved' });
    const result = cancelPendingApprovals([pending, approved], NOW);
    const gotPending = result.find((item) => item.id === 'a-pending');
    const gotApproved = result.find((item) => item.id === 'a-approved');
    expect(gotPending).toMatchObject({ status: 'expired', note: 'stream_cancelled' });
    expect(gotApproved).toBe(approved);
  });

  it('写入终态后 ApprovalCard 判定为「已取消」（isCancelledApproval 联动）', async () => {
    const { isCancelledApproval } = await import('../../../components/ApprovalCard');
    const cancelled = cancelPendingApprovals([approvalOf()], NOW).find(() => true);
    expect(cancelled && isCancelledApproval(cancelled)).toBe(true);
  });

  it('空列表返回空列表', () => {
    expect(cancelPendingApprovals([], NOW)).toEqual([]);
  });
});

describe('mergeApprovalsByTool 终态优先合并矩阵', () => {
  const toolA = 'tc-a';
  const toolB = 'tc-b';

  function byTool(entries: Array<[string, Approval]>): Map<string, Approval[]> {
    const map = new Map<string, Approval[]>();
    for (const [toolId, approval] of entries) {
      const list = map.get(toolId);
      if (list) {
        list.push(approval);
      } else {
        map.set(toolId, [approval]);
      }
    }
    return map;
  }

  it('走查死锁场景：live pending 不覆盖落库 expired 终态', () => {
    const persisted = byTool([
      [toolA, approvalOf({ id: 'ap1', status: 'expired', note: 'stream_cancelled' })],
    ]);
    const live = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending' })]]);
    const merged = mergeApprovalsByTool(persisted, live);
    expect(merged.get(toolA)).toEqual([
      approvalOf({ id: 'ap1', status: 'expired', note: 'stream_cancelled' }),
    ]);
  });

  it('落库 approved/denied 同样不被 live pending 压回', () => {
    for (const status of ['approved', 'denied'] as const) {
      const persisted = byTool([
        [toolA, approvalOf({ id: 'ap1', status, decided_at: '2026-01-01T00:01:00Z' })],
      ]);
      const live = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending' })]]);
      expect(mergeApprovalsByTool(persisted, live).get(toolA)?.[0]?.status).toBe(status);
    }
  });

  it('live 终态仍覆盖落库 pending（本窗口决策的即时反馈）', () => {
    const persisted = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending' })]]);
    const live = byTool([
      [toolA, approvalOf({ id: 'ap1', status: 'approved', decided_at: '2026-01-01T00:01:00Z' })],
    ]);
    expect(mergeApprovalsByTool(persisted, live).get(toolA)?.[0]?.status).toBe('approved');
  });

  it('live 终态覆盖落库终态（approval_resolved 帧先于 refetch 到达）', () => {
    const persisted = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending' })]]);
    const live = byTool([
      [toolA, approvalOf({ id: 'ap1', status: 'denied', note: 'stream_cancelled' })],
    ]);
    expect(mergeApprovalsByTool(persisted, live).get(toolA)?.[0]?.status).toBe('denied');
  });

  it('live pending 覆盖落库 pending（同状态，维持原语义）', () => {
    const persisted = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending', note: 'old' })]]);
    const live = byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending', note: 'new' })]]);
    expect(mergeApprovalsByTool(persisted, live).get(toolA)?.[0]?.note).toBe('new');
  });

  it('live 独有的审批追加（SSE 帧先到、落库 refetch 未落地）', () => {
    const persisted = byTool([]);
    const live = byTool([[toolA, approvalOf({ id: 'ap-live', status: 'pending' })]]);
    const merged = mergeApprovalsByTool(persisted, live);
    expect(merged.get(toolA)?.map((item) => item.id)).toEqual(['ap-live']);
  });

  it('落库独有的审批保留（live 已清空或未覆盖的工具组）', () => {
    const persisted = byTool([[toolB, approvalOf({ id: 'ap-db', status: 'expired' })]]);
    const merged = mergeApprovalsByTool(persisted, new Map());
    expect(merged.get(toolB)?.[0]?.id).toBe('ap-db');
  });

  it('同工具组多审批按 id 合并、不同工具组互不干扰', () => {
    const persisted = byTool([
      [toolA, approvalOf({ id: 'ap1', status: 'expired', note: 'stream_cancelled' })],
      [toolB, approvalOf({ id: 'ap2', status: 'approved' })],
    ]);
    const live = byTool([
      [toolA, approvalOf({ id: 'ap1', status: 'pending' })],
      [toolA, approvalOf({ id: 'ap3', status: 'pending' })],
    ]);
    const merged = mergeApprovalsByTool(persisted, live);
    const groupA = merged.get(toolA) ?? [];
    expect(groupA.map((item) => [item.id, item.status])).toEqual([
      ['ap1', 'expired'],
      ['ap3', 'pending'],
    ]);
    expect(merged.get(toolB)?.[0]?.status).toBe('approved');
  });

  it('原入参映射不被修改（纯函数）', () => {
    const persisted = byTool([[toolA, approvalOf({ id: 'ap1', status: 'expired' })]]);
    const snapshot = structuredClone(persisted.get(toolA));
    mergeApprovalsByTool(
      persisted,
      byTool([[toolA, approvalOf({ id: 'ap1', status: 'pending' })]]),
    );
    expect(persisted.get(toolA)).toEqual(snapshot);
  });
});

// LiveTurn 类型仅用于确认 stop() 路径不引入新字段依赖（结构类型占位）。
describe('stop() 后 live.approvals 的完整回路（类型与数据流契约）', () => {
  it('cancelPendingApprovals 输出可直接作为 LiveTurn.approvals 元素', () => {
    const live: LiveTurn = {
      userContent: '问',
      text: '',
      reasoning: '',
    reasoningStartedAt: null,
    reasoningEndedAt: null,
      tools: [],
      approvals: cancelPendingApprovals(
        [approvalOf({ status: 'pending' }), approvalOf({ id: 'a2', status: 'approved' })],
        NOW,
      ),
      error: null,
      done: null,
      stopped: true,
      settled: false,
      persisted: true,
      startedAt: Date.parse(NOW),
    };
    expect(live.approvals.map((item) => item.status)).toEqual(['expired', 'approved']);
    expect(live.approvals.every((item) => item.status !== 'pending')).toBe(true);
  });
});
