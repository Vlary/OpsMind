import { describe, expect, it } from 'vitest';

import { qk } from './queryKeys';

// query key 唯一来源（CONTRACT.md 8.4）的结构矩阵：
// 参数化 key 必须按参数区分（缓存隔离），静态 key 保持字面量。

describe('静态 query key 结构', () => {
  it('auth / conversations / servers 等静态键', () => {
    expect(qk.auth).toEqual(['auth']);
    expect(qk.conversations).toEqual(['conversations']);
    expect(qk.servers).toEqual(['servers']);
    expect(qk.databases).toEqual(['databases']);
    expect(qk.providers).toEqual(['providers']);
    expect(qk.presets).toEqual(['provider-presets']);
    expect(qk.health).toEqual(['health']);
  });

  it('me() 是 auth 的子键（随登出一起失效）', () => {
    expect(qk.me()).toEqual(['auth', 'me']);
  });
});

describe('参数化 query key 隔离', () => {
  it('conversation(id) 按会话 id 区分', () => {
    expect(qk.conversation('c1')).toEqual(['conversation', 'c1']);
    expect(qk.conversation('c1')).not.toBe(qk.conversation('c2'));
    expect(qk.conversation('c2')[1]).toBe('c2');
  });

  it('messages / toolCalls / approvals 同样按会话区分', () => {
    expect(qk.messages('m')).toEqual(['messages', 'm']);
    expect(qk.toolCalls('t')).toEqual(['tool-calls', 't']);
    expect(qk.approvals('a')).toEqual(['approvals', 'a']);
    expect(qk.messages('a')).not.toEqual(qk.toolCalls('a'));
  });

  it('server / database / provider 详情键按 id 区分', () => {
    expect(qk.server('s1')).toEqual(['server', 's1']);
    expect(qk.database('d1')).toEqual(['database', 'd1']);
    expect(qk.provider('p1')).toEqual(['provider', 'p1']);
  });

  it('audit(filters) 把筛选对象作为第二个元素内联', () => {
    const filters = { action: 'server.create', limit: 50 };
    expect(qk.audit(filters)).toEqual(['audit', filters]);
  });

  it('audit 不同筛选组合产生不同键（react-query 按内容判重）', () => {
    expect(qk.audit({ action: 'a' })).not.toEqual(qk.audit({ action: 'b' }));
    expect(qk.audit({ limit: 20 })).not.toEqual(qk.audit({ limit: 50 }));
    // 对象键序不影响 toEqual 判等（react-query 的 hash 对键序敏感是另一层语义）。
    expect(qk.audit({ action: 'a', limit: 20 })).toEqual(qk.audit({ limit: 20, action: 'a' }));
  });

  it('键内容稳定：同参数两次调用 toEqual 相等（引用可不同）', () => {
    const first = qk.conversation('same');
    const second = qk.conversation('same');
    expect(first).toEqual(second);
    expect(first).not.toBe(second);
  });

  it('audit 空对象与带字段对象不同键', () => {
    expect(qk.audit({})).not.toEqual(qk.audit({ action: '' }));
  });
});

describe('八期 FE-R：回放目录 key', () => {
  it('replays(q, has_actions) 结构为 [replays, filters]', () => {
    expect(qk.replays({ q: '磁盘', has_actions: true })).toEqual([
      'replays',
      { q: '磁盘', has_actions: true },
    ]);
  });

  it('q / has_actions 变化产生不同键（切 key 而不失效旧 key）', () => {
    expect(qk.replays({ q: 'a' })).not.toEqual(qk.replays({ q: 'b' }));
    expect(qk.replays({ has_actions: true })).not.toEqual(qk.replays({ has_actions: false }));
  });

  it('目录 key 与单会话回放 key 前缀不同（目录失效不影响播放器）', () => {
    expect(qk.replays({})[0]).toBe('replays');
    expect(qk.replay('c1')[0]).toBe('replay');
  });
});
