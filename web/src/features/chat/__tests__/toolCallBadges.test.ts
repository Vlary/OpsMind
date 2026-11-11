import { describe, expect, it } from 'vitest';

import {
  EXEMPT_EXECUTION_LABEL,
  badgeInputOf,
  isCommandExecTool,
  isExemptExecution,
} from '../toolCallBadges';
import { approvedToolCallIds, isReplayExemptExecution } from '../../replay/replayPlayer';

// 六期打磨 sprint 1（契约 8.7 / 2.11 约定 2）：工具卡风险/门限徽标互斥。
//
// 实机缺陷：none 档流式首帧 Tag = ["成功","需审批","免审批","高风险"]——
// RiskBadge 显示命令分类级别 needs_approval、ApprovalModeTag 显示 none、
// 「高风险」Tag 与之并列，同一张卡两种相反提示。修法：免审批档下实际
// 直执行的 needs_approval 命令隐藏 RiskBadge「需审批」，并把「高风险」
// 改为「高风险 · 免审批执行」。

const base = {
  name: 'ssh_exec',
  riskLevel: 'needs_approval',
  status: 'succeeded',
  approvalMode: 'none',
  approvalCount: 0,
};

describe('isExemptExecution', () => {
  it('none 档 + needs_approval + 未建审批 + 已执行 → 免审批执行', () => {
    expect(isExemptExecution(base)).toBe(true);
  });

  it('high 档同样适用（warning 级 needs_approval 免审）', () => {
    expect(isExemptExecution({ ...base, approvalMode: 'high' })).toBe(true);
  });

  it('standard / all 档不适用（这些档 needs_approval 必走审批）', () => {
    expect(isExemptExecution({ ...base, approvalMode: 'standard' })).toBe(false);
    expect(isExemptExecution({ ...base, approvalMode: 'all' })).toBe(false);
  });

  it('门限未知（空串，00010 之前的旧行）不适用', () => {
    expect(isExemptExecution({ ...base, approvalMode: '' })).toBe(false);
  });

  it('确实建了审批或仍在等待审批时不适用（规则照旧走审批）', () => {
    expect(isExemptExecution({ ...base, approvalCount: 1 })).toBe(false);
    expect(isExemptExecution({ ...base, status: 'awaiting_approval' })).toBe(false);
  });

  it('forbidden 不适用（命令被禁止、从未执行，仍需红色禁止提示）', () => {
    expect(isExemptExecution({ ...base, riskLevel: 'forbidden' })).toBe(false);
  });

  it('safe 命令不适用（本就不需要审批，无冲突）', () => {
    expect(isExemptExecution({ ...base, riskLevel: 'safe' })).toBe(false);
  });

  it('非命令执行类工具不适用', () => {
    expect(isExemptExecution({ ...base, name: 'list_servers' })).toBe(false);
    expect(isExemptExecution({ ...base, name: 'db_query' })).toBe(false);
  });

  it('batch_execute / container_action 同在命令执行类集合内', () => {
    expect(isExemptExecution({ ...base, name: 'batch_execute' })).toBe(true);
    expect(isExemptExecution({ ...base, name: 'container_action' })).toBe(true);
  });
});

describe('isCommandExecTool', () => {
  it('与后端 isCommandExecTool 同集合', () => {
    expect(isCommandExecTool('ssh_exec')).toBe(true);
    expect(isCommandExecTool('container_action')).toBe(true);
    expect(isCommandExecTool('batch_execute')).toBe(true);
    expect(isCommandExecTool('run_diagnosis')).toBe(false);
    expect(isCommandExecTool('get_metrics')).toBe(false);
  });
});

describe('badgeInputOf', () => {
  it('从视图与审批列表装配判定输入', () => {
    const input = badgeInputOf(
      { name: 'ssh_exec', riskLevel: 'needs_approval', status: 'succeeded', approvalMode: 'none' },
      [],
    );
    expect(input).toEqual({
      name: 'ssh_exec',
      riskLevel: 'needs_approval',
      status: 'succeeded',
      approvalMode: 'none',
      approvalCount: 0,
    });
  });
});

describe('提示文案', () => {
  it('替代文案明确「免审批执行」', () => {
    expect(EXEMPT_EXECUTION_LABEL).toBe('高风险 · 免审批执行');
  });
});

// 六期打磨 sprint 2（走查 medium）：回放侧复用同一判定——回放 tool_call 步
// 此前无论门限如何都渲染 RiskBadge「需审批」，与聊天工具卡的
// 「高风险 · 免审批执行」说法相反（实测会话 710e05eb…：none 档 + auto_exec
// 直执行的 ssh_exec 在回放里显示「需审批」，approval_id=null 且无 approval 步）。
describe('isReplayExemptExecution（回放侧等价判定）', () => {
  const toolCallStep = {
    seq: 3,
    timestamp: '2026-10-30T10:00:00Z',
    kind: 'tool_call' as const,
    name: 'ssh_exec',
    risk_level: 'needs_approval' as const,
    status: 'succeeded',
    approval_mode: 'none' as const,
  };

  it('none/high 档 + 无审批步 → 免审批执行', () => {
    expect(isReplayExemptExecution(toolCallStep, false)).toBe(true);
    expect(isReplayExemptExecution({ ...toolCallStep, approval_mode: 'high' }, false)).toBe(true);
  });

  it('存在 approval 步（真走了审批）→ 不判免审批，RiskBadge 保持「需审批」', () => {
    expect(isReplayExemptExecution(toolCallStep, true)).toBe(false);
  });

  it('standard/all 档、旧行门限为空、非命令执行类工具 → 不判免审批', () => {
    expect(isReplayExemptExecution({ ...toolCallStep, approval_mode: 'standard' }, false)).toBe(
      false,
    );
    expect(isReplayExemptExecution({ ...toolCallStep, approval_mode: 'all' }, false)).toBe(false);
    expect(isReplayExemptExecution({ ...toolCallStep, approval_mode: null }, false)).toBe(false);
    expect(isReplayExemptExecution({ ...toolCallStep, name: 'list_servers' }, false)).toBe(false);
  });

  it('仍等待人工（awaiting_approval）不判免审批', () => {
    expect(isReplayExemptExecution({ ...toolCallStep, status: 'awaiting_approval' }, false)).toBe(
      false,
    );
  });

  it('非 tool_call 步恒 false', () => {
    expect(
      isReplayExemptExecution(
        { seq: 1, timestamp: '2026-10-30T10:00:00Z', kind: 'approval' },
        false,
      ),
    ).toBe(false);
  });

  it('approvedToolCallIds 只收集 approval 步关联的调用', () => {
    const ids = approvedToolCallIds([
      { seq: 1, timestamp: 't', kind: 'tool_call', tool_call_id: 'tc-1' },
      { seq: 2, timestamp: 't', kind: 'approval', tool_call_id: 'tc-1' },
      { seq: 3, timestamp: 't', kind: 'tool_call', tool_call_id: 'tc-2' },
      { seq: 4, timestamp: 't', kind: 'approval', tool_call_id: null },
    ]);
    expect([...ids]).toEqual(['tc-1']);
  });
});
