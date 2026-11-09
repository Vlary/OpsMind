import { describe, expect, it } from 'vitest';

import { liveReasoningDurationMs, type LiveTurn } from '../features/chat/useChatStream';
import {
  applyStreamingToExpanded,
  reasoningCopyTarget,
  reasoningDurationSeconds,
  reasoningSummaryLabel,
} from './reasoningView';

// 深度思考块纯逻辑（六期 FE-P1，契约 8.6 第 13 条 / 8.7 清单⑪）。

describe('reasoningDurationSeconds 耗时秒数', () => {
  it('毫秒差四舍五入到整秒', () => {
    expect(reasoningDurationSeconds(0)).toBe(0);
    expect(reasoningDurationSeconds(499)).toBe(0);
    expect(reasoningDurationSeconds(500)).toBe(1);
    expect(reasoningDurationSeconds(12_340)).toBe(12);
    expect(reasoningDurationSeconds(90_000)).toBe(90);
  });

  it('null/undefined/负数/NaN → null（无计时数据）', () => {
    expect(reasoningDurationSeconds(null)).toBeNull();
    expect(reasoningDurationSeconds(undefined)).toBeNull();
    expect(reasoningDurationSeconds(-5)).toBeNull();
    expect(reasoningDurationSeconds(Number.NaN)).toBeNull();
  });
});

describe('reasoningSummaryLabel 摘要行文案', () => {
  it('streaming → 「思考中…」（实时展开态）', () => {
    expect(reasoningSummaryLabel({ streaming: true })).toBe('思考中…');
    expect(reasoningSummaryLabel({ streaming: true, durationMs: 5000 })).toBe('思考中…');
  });

  it('结束后有计时 → 「深度思考 · 耗时 Ns」', () => {
    expect(reasoningSummaryLabel({ streaming: false, durationMs: 8200 })).toBe(
      '深度思考 · 耗时 8s',
    );
    expect(reasoningSummaryLabel({ streaming: false, durationMs: 0 })).toBe('深度思考 · 耗时 0s');
  });

  it('无计时数据 → 「深度思考 · 耗时 —」（不静默省略耗时）', () => {
    expect(reasoningSummaryLabel({ streaming: false })).toBe('深度思考 · 耗时 —');
    expect(reasoningSummaryLabel({ streaming: false, durationMs: null })).toBe('深度思考 · 耗时 —');
  });
});

describe('applyStreamingToExpanded 折叠状态机（streaming 边沿）', () => {
  it('首个 reasoning delta（streaming=true）→ 强制展开（覆盖用户手动折叠）', () => {
    expect(applyStreamingToExpanded(false, true)).toBe(true);
    expect(applyStreamingToExpanded(true, true)).toBe(true);
  });

  it('首个正文 token 到达（streaming true→false）→ 回默认收起', () => {
    expect(applyStreamingToExpanded(true, false)).toBe(false);
    expect(applyStreamingToExpanded(false, false)).toBe(false);
  });

  it('历史消息（streaming=undefined）不改变用户手动展开态', () => {
    expect(applyStreamingToExpanded(true, undefined)).toBe(true);
    expect(applyStreamingToExpanded(false, undefined)).toBe(false);
  });
});

describe('reasoningCopyTarget 复制入口（8.7 清单⑪：ReasoningBlock 全文）', () => {
  it('展开态 → 复制内容为完整全文', () => {
    const text = '先检查服务状态，再决定是否重启；需要核对磁盘水位。';
    expect(reasoningCopyTarget(text, true)).toBe(text);
  });

  it('折叠态 → null（不渲染复制按钮；正文仍保留在 DOM，折叠不丢内容）', () => {
    expect(reasoningCopyTarget('推理内容', false)).toBeNull();
  });

  it('空 reasoning 展开态 → null（组件对空内容整体不渲染）', () => {
    expect(reasoningCopyTarget('', true)).toBeNull();
  });
});

// useChatStream 的计时测量：LiveTurn 首/尾 reasoning delta 本地时间差。
function liveOf(startedAt: number | null, endedAt: number | null): LiveTurn {
  return {
    userContent: 'q',
    text: '',
    reasoning: 'r',
    reasoningStartedAt: startedAt,
    reasoningEndedAt: endedAt,
    tools: [],
    approvals: [],
    error: null,
    done: null,
    stopped: false,
    settled: false,
    persisted: false,
    startedAt: 0,
  };
}

describe('liveReasoningDurationMs live 轮计时（useChatStream 测量）', () => {
  it('首尾差值', () => {
    expect(liveReasoningDurationMs(liveOf(1_000, 4_600))).toBe(3_600);
  });

  it('无 reasoning（首/尾为 null）→ null', () => {
    expect(liveReasoningDurationMs(liveOf(null, null))).toBeNull();
    expect(liveReasoningDurationMs(liveOf(1_000, null))).toBeNull();
    expect(liveReasoningDurationMs(liveOf(null, 1_000))).toBeNull();
  });

  it('负差值夹到 0（时钟回拨防御）', () => {
    expect(liveReasoningDurationMs(liveOf(2_000, 1_000))).toBe(0);
  });
});
