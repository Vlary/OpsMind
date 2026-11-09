import { describe, expect, it } from 'vitest';

import {
  computeLiveSettled,
  liveToolsFallbackTurnId,
  type LiveTurn,
  type SettledMessageView,
} from '../useChatStream';

// 第 3 轮打磨回归：快上游（无 token 间隔的 OpenAI 兼容 mock LLM）走查
// 实测 13 轮中 3 轮 done 后工具卡从页面消失、重进会话才恢复。根因是
// done 处理里 messages 与 toolCalls 的 invalidate 为并发 refetch，
// 落地存在毫秒级间隙——撤除 live（正文切换）与 toolCalls 分组上屏
// 之间存在窗口。
//
// 修复拆成两个互补判定（均为纯函数，本文件矩阵回归）：
//
// 1. computeLiveSettled（ChatPage liveSettled 本体）：done 分支只看
//    messages。若还要等 toolCalls，间隙内会同时渲染 live 条目与持久化
//    assistant 消息两份正文——审批超时 E2E 全量并行下 getByText 撞进
//    间隙，strict mode 双元素失败（本轮实测复现）。
// 2. liveToolsFallbackTurnId（MessageList 回退渲染）：live 条目撤除后
//    该 turn 的 toolCalls 分组未落地时，用内存中的 live.tools 补渲染
//    工具卡，卡片不消失；分组落地后回退自动失效。

function liveTurn(overrides: Partial<LiveTurn> = {}): LiveTurn {
  return {
    userContent: '查一下磁盘',
    text: '磁盘正常',
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

const doneFrame: LiveTurn['done'] = {
  turn_id: 'turn-1',
  message_id: 'msg-done-1',
  finish_reason: 'stop',
  usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
};

function dbQueryTool(toolCallId: string): LiveTurn['tools'][number] {
  return {
    id: toolCallId,
    providerCallId: `call_${toolCallId}`,
    name: 'db_query',
    arguments: { sql: 'select 1' },
    result: { status: 'ok' },
    error: null,
    status: 'succeeded',
    riskLevel: 'low',
    durationMs: 3,
    approvalId: '',
      approvalMode: '',
    startedAt: Date.now(),
  };
}

const persistedMessages = (extra: SettledMessageView[] = []): SettledMessageView[] => [
  { id: 'msg-user-1', role: 'user', content: '查一下磁盘' },
  { id: 'msg-done-1', role: 'assistant', content: '磁盘正常' },
  ...extra,
];

// 带 turn_id 的消息视图（liveToolsFallbackTurnId 的输入）。
const turnMessages = [
  { id: 'msg-user-1', turn_id: 'turn-0' },
  { id: 'msg-done-1', turn_id: 'turn-1' },
];

describe('computeLiveSettled 快流（0 延迟）回归', () => {
  it('done 后只看 messages：落地即撤除，不等 toolCalls（正文无双份窗口）', () => {
    const live = liveTurn({ done: doneFrame, tools: [dbQueryTool('tc-1')] });
    // messages 已含 done 消息、toolCalls refetch 仍在途（分组为空）：
    // live 必须立即撤除，否则间隙内 live 条目与持久化消息渲染两份正文。
    expect(computeLiveSettled(live, persistedMessages())).toBe(true);
  });

  it('messages 尚未含 done 消息（refetch 在途）时不撤除', () => {
    const live = liveTurn({ done: doneFrame, tools: [] });
    expect(
      computeLiveSettled(live, [{ id: 'msg-user-1', role: 'user', content: '查一下磁盘' }]),
    ).toBe(false);
  });

  it('无工具轮同样只看 messages', () => {
    expect(computeLiveSettled(liveTurn({ done: doneFrame }), persistedMessages())).toBe(true);
  });
});

describe('computeLiveSettled 既有分支语义不回归（从 ChatPage 原地搬迁）', () => {
  it('null live 与 HTTP 层拒绝（settled）恒为已撤除', () => {
    expect(computeLiveSettled(null, [])).toBe(true);
    expect(computeLiveSettled(liveTurn({ settled: true }), [])).toBe(true);
  });

  it('停止/断流轮：尾部 user 未等到 assistant error 落库时不撤除', () => {
    const live = liveTurn({ stopped: true, text: '部分回答' });
    const messages = [{ id: 'msg-user-1', role: 'user', content: '查一下磁盘' }];
    expect(computeLiveSettled(live, messages)).toBe(false);
  });

  it('停止/断流轮：尾部 assistant error 前缀匹配 live.text 时撤除', () => {
    const live = liveTurn({ error: null, stopped: true, text: '部分回答' });
    const messages = [
      { id: 'msg-user-1', role: 'user', content: '查一下磁盘' },
      { id: 'msg-err-1', role: 'assistant', status: 'error', content: '部分回答，但被截断' },
    ];
    expect(computeLiveSettled(live, messages)).toBe(true);
  });

  it('流式中（无 done/error/stopped）不撤除', () => {
    expect(computeLiveSettled(liveTurn(), persistedMessages())).toBe(false);
  });
});

describe('liveToolsFallbackTurnId done 轮工具卡回退（快流卡片不消失）', () => {
  it('done 消息已落地且分组为空：返回该 turn（用 live.tools 补渲染，回归主用例）', () => {
    const live = liveTurn({ done: doneFrame, tools: [dbQueryTool('tc-1')] });
    expect(liveToolsFallbackTurnId(live, turnMessages, new Set())).toBe('turn-1');
  });

  it('toolCalls 分组落地后回退失效（持久化分组接管）', () => {
    const live = liveTurn({ done: doneFrame, tools: [dbQueryTool('tc-1')] });
    expect(liveToolsFallbackTurnId(live, turnMessages, new Set(['turn-1']))).toBeNull();
  });

  it('live 无工具（无工具轮）或无 done（流式中/失败轮）不回退', () => {
    expect(
      liveToolsFallbackTurnId(liveTurn({ done: doneFrame }), turnMessages, new Set()),
    ).toBeNull();
    expect(
      liveToolsFallbackTurnId(liveTurn({ tools: [dbQueryTool('tc-1')] }), turnMessages, new Set()),
    ).toBeNull();
    expect(liveToolsFallbackTurnId(null, turnMessages, new Set())).toBeNull();
  });

  it('done 消息未落地（live 条目仍在渲染工具卡）不回退', () => {
    const live = liveTurn({ done: doneFrame, tools: [dbQueryTool('tc-1')] });
    const onlyUser = [{ id: 'msg-user-1', turn_id: 'turn-0' }];
    expect(liveToolsFallbackTurnId(live, onlyUser, new Set())).toBeNull();
  });

  it('多工具轮：该 turn 的 toolCalls 分组落地即由持久化接管（分组按 turn 粒度）', () => {
    const live = liveTurn({
      done: doneFrame,
      tools: [dbQueryTool('tc-1'), dbQueryTool('tc-2')],
    });
    expect(liveToolsFallbackTurnId(live, turnMessages, new Set(['turn-1']))).toBeNull();
  });
});
