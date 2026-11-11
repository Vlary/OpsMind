// FE-P2（六期）：回放播放器推进逻辑（契约 8.7——步进/自动播放/速度/进度条）。
import { describe, expect, it } from 'vitest';

import {
  REPLAY_SPEEDS,
  advanceTick,
  approvalSourceLabel,
  approvalStatusLabel,
  clampStepIndex,
  executionStatusLabel,
  isReplaySpeed,
  progressToIndex,
  replayProgress,
  stepBy,
  stepIntervalMs,
  stepSummary,
  toolCallStatusLabel,
} from '../replayPlayer';

describe('stepIntervalMs（速度换算）', () => {
  it('1x/2x/4x 对应 800/400/200ms', () => {
    expect(stepIntervalMs(1)).toBe(800);
    expect(stepIntervalMs(2)).toBe(400);
    expect(stepIntervalMs(4)).toBe(200);
  });

  it('速度档位恰为 1/2/4', () => {
    expect([...REPLAY_SPEEDS]).toEqual([1, 2, 4]);
    expect(isReplaySpeed(1)).toBe(true);
    expect(isReplaySpeed(4)).toBe(true);
    expect(isReplaySpeed(3)).toBe(false);
  });
});

describe('advanceTick（自动播放推进）', () => {
  it('逐步 +1 且保持播放', () => {
    expect(advanceTick({ index: 0, playing: true }, 3)).toEqual({ index: 1, playing: true });
    expect(advanceTick({ index: 1, playing: true }, 3)).toEqual({ index: 2, playing: true });
  });

  it('到达最后一步时停在末步并自动暂停（终止条件）', () => {
    expect(advanceTick({ index: 2, playing: true }, 3)).toEqual({ index: 2, playing: false });
  });

  it('空步骤列表直接归零并暂停', () => {
    expect(advanceTick({ index: 0, playing: true }, 0)).toEqual({ index: 0, playing: false });
    expect(advanceTick({ index: 5, playing: true }, 0)).toEqual({ index: 0, playing: false });
  });
});

describe('stepBy（单步）', () => {
  it('下一步/上一步移动且单步即暂停', () => {
    expect(stepBy({ index: 1, playing: true }, 1, 4)).toEqual({ index: 2, playing: false });
    expect(stepBy({ index: 2, playing: true }, -1, 4)).toEqual({ index: 1, playing: false });
  });

  it('越界钳制在首末步', () => {
    expect(stepBy({ index: 0, playing: false }, -1, 4)).toEqual({ index: 0, playing: false });
    expect(stepBy({ index: 3, playing: false }, 1, 4)).toEqual({ index: 3, playing: false });
  });

  it('空列表恒 0', () => {
    expect(stepBy({ index: 2, playing: false }, 1, 0)).toEqual({ index: 0, playing: false });
  });
});

describe('进度条换算', () => {
  it('首步 0%、末步 100%、中点 50%', () => {
    expect(replayProgress(0, 5)).toBe(0);
    expect(replayProgress(4, 5)).toBeCloseTo(100);
    expect(replayProgress(2, 5)).toBeCloseTo(50);
  });

  it('空列表恒 0%，越界 index 被钳制', () => {
    expect(replayProgress(3, 0)).toBe(0);
    expect(replayProgress(99, 5)).toBeCloseTo(100);
  });

  it('单步会话恒 0%（此前 0/0=NaN → Slider 无 aria-valuenow、轨道恒 0%）', () => {
    // 六期打磨 sprint 2 走查：`replayProgress(0, 1)` 曾返回 NaN。
    for (const index of [0, 1, 5, -1]) {
      const value = replayProgress(index, 1);
      expect(Number.isNaN(value), `index=${index}`).toBe(false);
      expect(value).toBe(0);
    }
  });

  it('percent → index 往返一致（四舍五入后钳制）', () => {
    expect(progressToIndex(0, 5)).toBe(0);
    expect(progressToIndex(100, 5)).toBe(4);
    expect(progressToIndex(50, 5)).toBe(2);
    expect(progressToIndex(-10, 5)).toBe(0);
    expect(progressToIndex(120, 5)).toBe(4);
    expect(progressToIndex(50, 0)).toBe(0);
  });

  it('clampStepIndex 基本钳制', () => {
    expect(clampStepIndex(-3, 4)).toBe(0);
    expect(clampStepIndex(9, 4)).toBe(3);
    expect(clampStepIndex(2, 4)).toBe(2);
    expect(clampStepIndex(2, -1)).toBe(0);
  });
});

describe('toolCallStatusLabel / approvalStatusLabel 中文化', () => {
  it('failed → 「失败」（此前走 approvalStatusLabel 的 default 原样返回英文）', () => {
    expect(toolCallStatusLabel('failed')).toBe('失败');
  });

  it('其余 ToolCallStatus 全中文（无中英混排）', () => {
    const expected: Record<string, string> = {
      started: '进行中',
      awaiting_approval: '待审批',
      succeeded: '成功',
      denied: '已拒绝',
      expired: '已过期',
      awaiting_user: '待用户回答',
    };
    for (const [status, label] of Object.entries(expected)) {
      expect(toolCallStatusLabel(status), status).toBe(label);
      // 断言不含 ASCII 字母：任何英文原样返回都会命中。
      expect(/[A-Za-z]/.test(label), `${status} 含英文`).toBe(false);
    }
  });

  it('未知名与空值兜底不变（— / 原样）', () => {
    expect(toolCallStatusLabel(null)).toBe('—');
    expect(toolCallStatusLabel(undefined)).toBe('—');
    expect(toolCallStatusLabel('weird')).toBe('weird');
    expect(approvalStatusLabel('failed')).toBe('failed');
  });

  it('approvalStatusLabel 四态中文不变（expired 用中性「已过期」）', () => {
    expect(approvalStatusLabel('approved')).toBe('已批准');
    expect(approvalStatusLabel('denied')).toBe('已拒绝');
    // 六期打磨 sprint 3（走查 medium）：expired 同时覆盖 TTL 超时与流断开
    // 取消（note=stream_cancelled，对话卡显示「已取消」）。回放拿不到 note，
    // 说「已超时」会与同会话审批卡直接矛盾 → 中性文案。
    expect(approvalStatusLabel('expired')).toBe('已过期');
    expect(approvalStatusLabel('pending')).toBe('待审批');
    expect(approvalStatusLabel(null)).toBe('—');
  });

  it('expired 文案不再断言「超时」（避免与对话卡的「已取消」矛盾回归）', () => {
    expect(approvalStatusLabel('expired')).not.toContain('超时');
  });

  it('executionStatusLabel 成功/失败中文（无 result 的失败步同样走「失败」）', () => {
    expect(executionStatusLabel('succeeded')).toBe('成功');
    expect(executionStatusLabel('failed')).toBe('失败');
    expect(executionStatusLabel(null)).toBe('—');
  });
});

// 六期打磨 sprint 2（走查 medium）：摘要行与正文同口径剥离上下文前缀。
// e2e 实测显示「用户 · <!--opsmind-context servers: walk-ssh …」——正文修好
// 了但摘要行没修，等于同一处泄漏换了个位置。
describe('stepSummary 摘要行', () => {
  const base = { seq: 1, timestamp: '2026-10-30T10:00:00Z' };

  it('user_message 剥离内部上下文前缀（与正文一致）', () => {
    expect(
      stepSummary({
        ...base,
        kind: 'user_message',
        content: '<!--opsmind-context\nservers: walk-ssh (id=abc)\n-->\n__walk_uptime__',
      }),
    ).toBe('__walk_uptime__');
  });

  it('普通 HTML 注释不误剥', () => {
    expect(stepSummary({ ...base, kind: 'user_message', content: '<!-- 普通注释 --> 正文' })).toBe(
      '<!-- 普通注释 --> 正文',
    );
  });

  it('assistant_text / reasoning 不做前缀处理（它们从不携带上下文）', () => {
    expect(
      stepSummary({ ...base, kind: 'assistant_text', content: '<!--opsmind-context x -->' }),
    ).toBe('<!--opsmind-context x -->');
  });

  it('空内容与各类步的兜底文案', () => {
    expect(stepSummary({ ...base, kind: 'user_message', content: '' })).toBe('（空）');
    expect(
      stepSummary({
        ...base,
        kind: 'user_message',
        content: '<!--opsmind-context\n-->\n',
      }),
    ).toBe('（空）');
    expect(stepSummary({ ...base, kind: 'tool_call', name: 'ssh_exec' })).toBe('ssh_exec');
    expect(stepSummary({ ...base, kind: 'approval', command: 'uptime' })).toBe('uptime');
    expect(stepSummary({ ...base, kind: 'execution' })).toBe('—');
  });
});

// 六期打磨 sprint 5（走查 low）：回放审批步头部曾直出 source 原码
// （chat / console / batch），是全站唯一的中英混排——同一步的「已批准」
// 「用户改写」、门限、批量状态全部是中文。
describe('approvalSourceLabel 审批来源中文化（p6 sprint 5）', () => {
  it('三种来源各自映射中文，且不含 ASCII 字母', () => {
    const expected: Record<string, string> = {
      chat: '对话',
      console: '控制台',
      batch: '批量',
    };
    for (const [source, label] of Object.entries(expected)) {
      expect(approvalSourceLabel(source), source).toBe(label);
      expect(/[A-Za-z]/.test(label), `${source} 含英文`).toBe(false);
    }
  });

  it('空值兜底「—」，未知来源原样返回（后端新增来源不渲染空白）', () => {
    expect(approvalSourceLabel(null)).toBe('—');
    expect(approvalSourceLabel(undefined)).toBe('—');
    expect(approvalSourceLabel('')).toBe('—');
    expect(approvalSourceLabel('webhook')).toBe('webhook');
  });
});
