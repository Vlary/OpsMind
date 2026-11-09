import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { formatRemaining, isCancelledApproval } from '../../../components/ApprovalCard';
import { describeRiskRule, RISK_RULE_LABELS } from '../../../components/riskRules';
import { shouldHoldIdleWatchdog, type LiveTurn } from '../useChatStream';

type Approval = components['schemas']['Approval'];

// 审批状态机的展示与判定矩阵：倒计时格式化、取消判定（TTL 超时 vs 流断开
// 连带取消）、风险规则中文映射与看门狗续期的审批维度补充。

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

describe('formatRemaining 审批倒计时全矩阵', () => {
  it('0 与负数夹到 0:00', () => {
    expect(formatRemaining(0)).toBe('0:00');
    expect(formatRemaining(-1)).toBe('0:00');
    expect(formatRemaining(-60_000)).toBe('0:00');
  });

  it('1-999ms 向上取整到 0:01', () => {
    expect(formatRemaining(1)).toBe('0:01');
    expect(formatRemaining(500)).toBe('0:01');
    expect(formatRemaining(999)).toBe('0:01');
  });

  it('整秒区间：[n*1000, n*1000+999] 都显示 n 秒', () => {
    expect(formatRemaining(1000)).toBe('0:01');
    expect(formatRemaining(1999)).toBe('0:02');
    expect(formatRemaining(2000)).toBe('0:02');
    expect(formatRemaining(59_000)).toBe('0:59');
  });

  it('59.999s 进位到 1:00（ceil 语义）', () => {
    expect(formatRemaining(59_999)).toBe('1:00');
  });

  it('60s 边界：1:00 起', () => {
    expect(formatRemaining(60_000)).toBe('1:00');
    expect(formatRemaining(60_001)).toBe('1:01');
    expect(formatRemaining(61_000)).toBe('1:01');
  });

  it('90s（E2E TTL=45s 的两倍）→ 1:30', () => {
    expect(formatRemaining(90_000)).toBe('1:30');
  });

  it('45s（E2E 收紧 TTL）→ 0:45', () => {
    expect(formatRemaining(45_000)).toBe('0:45');
  });

  it('120s（生产默认 TTL）→ 2:00', () => {
    expect(formatRemaining(120_000)).toBe('2:00');
    expect(formatRemaining(119_999)).toBe('2:00');
  });

  it('超过 10 分钟继续按分钟展示', () => {
    expect(formatRemaining(600_000)).toBe('10:00');
    expect(formatRemaining(601_000)).toBe('10:01');
  });

  it('超过 1 小时不折叠为小时（M:SS 一直加分钟）', () => {
    expect(formatRemaining(3_600_000)).toBe('60:00');
    expect(formatRemaining(3_661_000)).toBe('61:01');
  });
});

describe('isCancelledApproval 全状态 × note 矩阵', () => {
  const notes = ['', 'stream_cancelled', 'server_shutting_down', 'other_unknown'];

  it('status=pending：任何 note 都不是取消', () => {
    for (const note of notes) {
      expect(isCancelledApproval(approvalOf({ status: 'pending', note }))).toBe(false);
    }
  });

  it('status=approved：任何 note 都不是取消', () => {
    for (const note of notes) {
      expect(isCancelledApproval(approvalOf({ status: 'approved', note }))).toBe(false);
    }
  });

  it('status=denied：任何 note 都不是取消', () => {
    for (const note of notes) {
      expect(isCancelledApproval(approvalOf({ status: 'denied', note }))).toBe(false);
    }
  });

  it('status=expired：仅 stream_cancelled / server_shutting_down 是取消', () => {
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: 'stream_cancelled' }))).toBe(
      true,
    );
    expect(
      isCancelledApproval(approvalOf({ status: 'expired', note: 'server_shutting_down' })),
    ).toBe(true);
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: '' }))).toBe(false);
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: 'other_unknown' }))).toBe(
      false,
    );
  });

  it('已决策（decided_at 有值）的 expired 审批仍按 note 判定取消', () => {
    const cancelled = approvalOf({
      status: 'expired',
      note: 'stream_cancelled',
      decided_at: '2026-01-01T00:01:30Z',
    });
    expect(isCancelledApproval(cancelled)).toBe(true);
  });

  it('note 前后空白不被修剪（精确匹配内部标识）', () => {
    expect(isCancelledApproval(approvalOf({ status: 'expired', note: ' stream_cancelled ' }))).toBe(
      false,
    );
  });
});

describe('describeRiskRule 风险规则中文映射全量', () => {
  const ruleIds = Object.keys(RISK_RULE_LABELS);

  it('规则表覆盖 OpsMind 全部内置规则（≥36 条）', () => {
    expect(ruleIds.length).toBeGreaterThanOrEqual(36);
  });

  it.each(ruleIds.map((id) => [id] as const))('规则 %s 返回非空中文说明', (id) => {
    const label = describeRiskRule(id);
    expect(label).not.toBeNull();
    expect(label!.length).toBeGreaterThan(2);
  });

  it('高风险规则返回中文而非英文标识', () => {
    expect(describeRiskRule('RM_PROTECTED')).toBe('删除受保护的系统路径（递归/强制删除）');
    expect(describeRiskRule('FORK_BOMB')).toBe('检测到 fork 炸弹模式');
    expect(describeRiskRule('MKFS')).toBe('格式化文件系统');
  });

  it('安全只读规则同样映射（审批卡不展示时也不误报）', () => {
    expect(describeRiskRule('SAFE_READ')).toBe('只读命令');
    expect(describeRiskRule('SAFE_PIPE')).toBe('只读命令的常规管道');
  });

  it('SQL 类规则映射中文', () => {
    expect(describeRiskRule('SQL_DESTRUCTIVE')).toBe(
      '破坏性 SQL（DROP、TRUNCATE、全表 DELETE 等）',
    );
    expect(describeRiskRule('SQL_WRITE')).toBe('写入型 SQL（INSERT、UPDATE、ALTER 等）');
  });

  it('未知规则 ID 返回 null（调用方回退原始 reason）', () => {
    expect(describeRiskRule('NOT_A_RULE')).toBeNull();
  });

  it('空值输入返回 null', () => {
    expect(describeRiskRule(null)).toBeNull();
    expect(describeRiskRule(undefined)).toBeNull();
    expect(describeRiskRule('')).toBeNull();
  });

  it('匹配大小写敏感（后端 rule_id 恒大写）', () => {
    expect(describeRiskRule('rm_any')).toBeNull();
    expect(describeRiskRule('Rm_Any')).toBeNull();
  });
});

describe('shouldHoldIdleWatchdog 审批维度补充矩阵', () => {
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

  it('expires_at 非法日期：不按审批续期（保守判挂死）', () => {
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: 'not-a-date' })],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('多个审批中只要一个未过期即续期', () => {
    const live = liveOf({
      approvals: [
        approvalOf({ id: 'ap-old', status: 'expired' }),
        approvalOf({ id: 'ap-live', status: 'pending', expires_at: '2026-01-01T00:12:00Z' }),
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('多个审批全部过期超缓冲：不续期', () => {
    const live = liveOf({
      approvals: [
        approvalOf({ id: 'ap1', status: 'pending', expires_at: '2026-01-01T00:05:00Z' }),
        approvalOf({ id: 'ap2', status: 'pending', expires_at: '2026-01-01T00:05:30Z' }),
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('pending 审批过期前 1ms 仍在续期窗口', () => {
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: '2026-01-01T00:09:59.999Z' })],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('pending 审批与已决策审批混合：以 pending 为准', () => {
    const live = liveOf({
      approvals: [
        approvalOf({ id: 'a1', status: 'approved' }),
        approvalOf({ id: 'a2', status: 'pending', expires_at: '2026-01-01T00:11:00Z' }),
        approvalOf({ id: 'a3', status: 'denied' }),
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('审批 pending 续期优先于首帧宽限缺失（receivedAnyFrame=true 也续）', () => {
    // 即使已收到过帧（token 间隙），pending 审批仍代表后端在等用户决策。
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: '2026-01-01T00:12:00Z' })],
      text: '部分回答',
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(true);
  });

  it('工具 started 超 SSH 上限余量：审批也过期时不续期', () => {
    const live = liveOf({
      approvals: [approvalOf({ status: 'pending', expires_at: '2026-01-01T00:05:00Z' })],
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
          startedAt: NOW - 400_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('awaiting_approval 工具超余量：不续期（连接判挂死）', () => {
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
          startedAt: NOW - 331_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('已结束工具（succeeded/failed/denied/expired）不构成续期理由', () => {
    for (const status of ['succeeded', 'failed', 'denied', 'expired'] as const) {
      const live = liveOf({
        tools: [
          {
            id: 'tc1',
            providerCallId: '',
            name: 'ssh_exec',
            arguments: {},
            result: null,
            error: null,
            status,
            riskLevel: '',
            durationMs: 0,
            approvalId: '',
            approvalMode: '',
            startedAt: NOW - 60_000,
          },
        ],
      });
      expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
    }
  });

  it('awaiting_user 工具状态不在续期集合内（等待用户但无审批卡场景）', () => {
    const live = liveOf({
      tools: [
        {
          id: 'tc1',
          providerCallId: '',
          name: 'ask_user',
          arguments: {},
          result: null,
          error: null,
          status: 'awaiting_user',
          riskLevel: '',
          durationMs: 0,
          approvalId: '',
          approvalMode: '',
          startedAt: NOW - 60_000,
        },
      ],
    });
    expect(shouldHoldIdleWatchdog(live, NOW, true)).toBe(false);
  });

  it('startedAt 在未来（时钟回拨）：首帧宽限按负差值恒成立', () => {
    const live = liveOf({ startedAt: NOW + 5_000 });
    expect(shouldHoldIdleWatchdog(live, NOW, false)).toBe(true);
  });
});
