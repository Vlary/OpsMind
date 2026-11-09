import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { pendingRegenerateUserId } from '../MessageList';

type Message = components['schemas']['Message'];

// 最小可用 Message 工厂：pendingRegenerateUserId 只看末条 role/id。
function messageOf(
  id: string,
  role: 'user' | 'assistant',
  overrides: Partial<Message> = {},
): Message {
  return {
    id,
    seq: 1,
    conversation_id: 'c1',
    turn_id: 't1',
    role,
    content: '正文',
    reasoning: '',
    reasoning_duration_ms: null,
    status: 'complete',
    error_code: '',
    model: 'fake-model',
    prompt_tokens: 0,
    completion_tokens: 0,
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

// 走查 A2 附带项：后端崩溃经代理时 user 消息悬挂（无任何 assistant 回复），
// 刷新后必须能识别出这条悬挂 user，为其提供「重新生成」入口。
describe('pendingRegenerateUserId', () => {
  it('最后一条是 user（悬挂轮）时返回其 id', () => {
    const messages = [
      messageOf('u1', 'user'),
      messageOf('a1', 'assistant'),
      messageOf('u2', 'user'),
    ];
    expect(pendingRegenerateUserId(messages)).toBe('u2');
  });

  it('最后一条是 assistant（正常/失败完成轮）时不提供悬挂入口', () => {
    const messages = [
      messageOf('u1', 'user'),
      messageOf('a1', 'assistant', { status: 'error', error_code: 'CLIENT_DISCONNECTED' }),
    ];
    expect(pendingRegenerateUserId(messages)).toBeUndefined();
  });

  it('中间的 user 消息不算悬挂（只有末条生效）', () => {
    const messages = [
      messageOf('u1', 'user'),
      messageOf('u1x', 'user'),
      messageOf('a1', 'assistant'),
      messageOf('u2', 'user'),
      messageOf('a2', 'assistant'),
    ];
    expect(pendingRegenerateUserId(messages)).toBeUndefined();
  });

  it('空列表返回 undefined', () => {
    expect(pendingRegenerateUserId([])).toBeUndefined();
  });

  it('仅一条 user 消息（发送后即崩溃）也返回其 id', () => {
    expect(pendingRegenerateUserId([messageOf('u1', 'user')])).toBe('u1');
  });
});
