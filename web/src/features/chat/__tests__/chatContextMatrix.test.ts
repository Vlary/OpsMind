import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { stripContextPrefix } from '../markdownBlocks';
import { buildContextContent } from '../ChatPage';

type Server = components['schemas']['Server'];
type Database = components['schemas']['Database'];

// 上下文前缀构建与长度预算的补充矩阵：
// 与 chatContext.test.ts 互补，覆盖结构断言、组合矩阵、名称回退、
// 往返剥离与 code point 预算的边界。

const CHAT_CONTENT_LIMIT = 16000;

function serverOf(id: string, name: string): Server {
  return {
    id,
    name,
    // 三期（N3 代改，归属 FE1）：schema Server 新增字段补契约默认值。
    device_kind: 'server',
    os: 'linux',
    netdev_brand: 'generic',
    access_proto: 'ssh',
    host: '127.0.0.1',
    port: 22,
    winrm_port: 5985,
    username: 'root',
    auth_method: 'password',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: '',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  };
}

function databaseOf(id: string, name: string): Database {
  return {
    id,
    name,
    engine: 'postgres',
    host: '127.0.0.1',
    port: 5432,
    database_name: 'main',
    username: 'aiops',
    sqlite_path: '',
    instance_name: '',
    redis_db: 0,
    // 三期（N3 代改，归属 FE1）：schema Database 新增字段补契约默认值。
    auth_db: '',
    ssl_mode: 'disable',
    has_password: true,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  };
}

describe('buildContextContent 前缀结构', () => {
  it('前缀首行固定为 <!--opsmind-context，尾行为 -->', () => {
    const content = buildContextContent('x', ['s1'], [], [serverOf('s1', 'web-1')], []);
    const lines = content.split('\n');
    expect(lines[0]).toBe('<!--opsmind-context');
    expect(lines[2]).toBe('-->');
    expect(lines[3]).toBe('x');
  });

  it('仅数据库选择时不含 servers 行', () => {
    const content = buildContextContent('x', [], ['d1'], [], [databaseOf('d1', 'db')]);
    expect(content).not.toContain('servers:');
    expect(content).toContain('databases: db (id=d1)');
  });

  it('仅服务器选择时不含 databases 行', () => {
    const content = buildContextContent('x', ['s1'], [], [serverOf('s1', 'web-1')], []);
    expect(content).toContain('servers: web-1 (id=s1)');
    expect(content).not.toContain('databases:');
  });

  it('双选择时总行数为 5（开头/servers/databases/闭合/正文）', () => {
    const content = buildContextContent(
      'x',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'web-1')],
      [databaseOf('d1', 'db')],
    );
    expect(content.split('\n')).toHaveLength(5);
  });

  it('正文为空：前缀后恰好一个换行', () => {
    const content = buildContextContent('', ['s1'], [], [serverOf('s1', 'w')], []);
    expect(content.endsWith('\n')).toBe(true);
    expect(content.split('\n')).toHaveLength(4);
  });

  it('正文多行原样保留（含空行）', () => {
    const content = buildContextContent('第一行\n\n第三行', [], [], [], []);
    expect(content).toBe('第一行\n\n第三行');
  });
});

describe('buildContextContent 组合矩阵', () => {
  it('多台服务器按选中顺序输出，与清单顺序无关', () => {
    const content = buildContextContent(
      'x',
      ['s3', 's1'],
      [],
      [serverOf('s1', 'alpha'), serverOf('s2', 'beta'), serverOf('s3', 'gamma')],
      [],
    );
    expect(content).toContain('servers: gamma (id=s3), alpha (id=s1)');
    expect(content).not.toContain('beta');
  });

  it('多个数据库同理按选中顺序输出', () => {
    const content = buildContextContent(
      'x',
      [],
      ['d2', 'd1'],
      [],
      [databaseOf('d1', '订单库'), databaseOf('d2', '用户库')],
    );
    expect(content).toContain('databases: 用户库 (id=d2), 订单库 (id=d1)');
  });

  it('同一 id 重复选中会重复出现（Select multiple 去重前的容错）', () => {
    const content = buildContextContent('x', ['s1', 's1'], [], [serverOf('s1', 'w')], []);
    expect(content).toContain('servers: w (id=s1), w (id=s1)');
  });

  it('清单里同 id 的服务器与数据库互不干扰', () => {
    const content = buildContextContent(
      'x',
      ['shared'],
      ['shared'],
      [serverOf('shared', 'srv')],
      [databaseOf('shared', 'db')],
    );
    expect(content).toContain('servers: srv (id=shared)');
    expect(content).toContain('databases: db (id=shared)');
  });

  it('部分 id 在清单、部分不在：可解析的用名称，缺失的回退裸 id', () => {
    const content = buildContextContent('x', ['s1', 'gone'], [], [serverOf('s1', 'web-1')], []);
    expect(content).toContain('servers: web-1 (id=s1), gone');
  });

  it('清单为空数组时全部回退裸 id', () => {
    const content = buildContextContent('x', ['a', 'b'], ['c'], [], []);
    expect(content).toContain('servers: a, b');
    expect(content).toContain('databases: c');
  });

  it('名称包含逗号/括号等特殊字符原样输出', () => {
    const content = buildContextContent('x', ['s1'], [], [serverOf('s1', 'web,1 (prod)')], []);
    expect(content).toContain('servers: web,1 (prod) (id=s1)');
  });

  it('名称包含中文与 emoji 原样输出', () => {
    const content = buildContextContent('x', ['s1'], [], [serverOf('s1', '生产🔴')], []);
    expect(content).toContain('servers: 生产🔴 (id=s1)');
  });

  it('id 为空字符串时按“找不到”回退（不匹配未命名资产）', () => {
    const content = buildContextContent('x', [''], [], [serverOf('s1', 'w')], []);
    expect(content).toContain('servers: ');
  });
});

describe('buildContextContent 与展示层往返', () => {
  it('build → strip 往返：仅数据库选择', () => {
    const content = buildContextContent('查订单库', [], ['d1'], [], [databaseOf('d1', '订单')]);
    expect(stripContextPrefix(content)).toBe('查订单库');
  });

  it('build → strip 往返：双选择', () => {
    const content = buildContextContent(
      '巡检',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'web')],
      [databaseOf('d1', 'db')],
    );
    expect(stripContextPrefix(content)).toBe('巡检');
  });

  it('正文本身以 <!-- 开头不被误剥（前缀只剥 opsmind-context 标记）', () => {
    const content = buildContextContent('<!-- 普通 --> 正文', [], [], [], []);
    expect(content).toBe('<!-- 普通 --> 正文');
  });

  it('空选择的 build 是恒等函数（无前缀无换行）', () => {
    expect(buildContextContent('任何正文', [], [], [], [])).toBe('任何正文');
  });
});

describe('上下文前缀长度预算矩阵（CHAT_CONTENT_LIMIT=16000）', () => {
  const codePoints = (value: string): number => [...value].length;

  it('前缀长度 = build(空正文) 的 code point 数', () => {
    const prefix = buildContextContent(
      '',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'w')],
      [databaseOf('d1', 'd')],
    );
    expect(codePoints(prefix)).toBeGreaterThan(0);
    // build(正文) = 前缀 + 正文：总长恒等于前缀长 + 正文长。
    const withBody = buildContextContent(
      '正文',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'w')],
      [databaseOf('d1', 'd')],
    );
    expect(codePoints(withBody)).toBe(codePoints(prefix) + codePoints('正文'));
  });

  it('选中资产越多前缀越长（预算单调递减）', () => {
    const one = buildContextContent('', ['s1'], [], [serverOf('s1', 'a')], []);
    const two = buildContextContent(
      '',
      ['s1', 's2'],
      [],
      [serverOf('s1', 'a'), serverOf('s2', 'b')],
      [],
    );
    expect(codePoints(two)).toBeGreaterThan(codePoints(one));
  });

  it('名字更长的资产占用更多预算（长度差逐字符传导）', () => {
    const short = buildContextContent('', ['s1'], [], [serverOf('s1', 'a')], []);
    const long = buildContextContent('', ['s1'], [], [serverOf('s1', 'a'.repeat(200))], []);
    // 名字 200 字符比 1 字符多 199 个 code point。
    expect(codePoints(long) - codePoints(short)).toBe(199);
  });

  it('正文贴满剩余预算：总长恰好 16000', () => {
    const prefix = buildContextContent('', ['s1'], [], [serverOf('s1', 'w')], []);
    const budget = CHAT_CONTENT_LIMIT - codePoints(prefix);
    const content = buildContextContent('a'.repeat(budget), ['s1'], [], [serverOf('s1', 'w')], []);
    expect(codePoints(content)).toBe(CHAT_CONTENT_LIMIT);
  });

  it('正文超出剩余预算 1 个字符：总长 16001（ChatPage 拒绝）', () => {
    const prefix = buildContextContent('', ['s1'], [], [serverOf('s1', 'w')], []);
    const budget = CHAT_CONTENT_LIMIT - codePoints(prefix);
    const content = buildContextContent(
      'a'.repeat(budget + 1),
      ['s1'],
      [],
      [serverOf('s1', 'w')],
      [],
    );
    expect(codePoints(content)).toBe(CHAT_CONTENT_LIMIT + 1);
  });

  it('无上下文时正文可用满 16000', () => {
    expect(codePoints(buildContextContent('a'.repeat(CHAT_CONTENT_LIMIT), [], [], [], []))).toBe(
      CHAT_CONTENT_LIMIT,
    );
  });

  it('emoji 正文按 code point 计数（双代理对不双计）', () => {
    const prefix = buildContextContent('', ['s1'], [], [serverOf('s1', 'w')], []);
    const budget = CHAT_CONTENT_LIMIT - codePoints(prefix);
    const emojiBody = '🚀'.repeat(budget);
    const content = buildContextContent(emojiBody, ['s1'], [], [serverOf('s1', 'w')], []);
    expect(codePoints(content)).toBe(CHAT_CONTENT_LIMIT);
    // UTF-16 length 因代理对远大于 code point 计数。
    expect(content.length).toBeGreaterThan(CHAT_CONTENT_LIMIT);
  });

  it('中文字符与 ASCII 同样按 1 个 code point 计数', () => {
    const prefix = buildContextContent('', ['s1'], [], [serverOf('s1', 'w')], []);
    const budget = CHAT_CONTENT_LIMIT - codePoints(prefix);
    const content = buildContextContent('汉'.repeat(budget), ['s1'], [], [serverOf('s1', 'w')], []);
    expect(codePoints(content)).toBe(CHAT_CONTENT_LIMIT);
  });

  it('composerMaxLength 语义：至少为 1（前缀吃满预算的极端兜底）', () => {
    // ChatPage: Math.max(1, CHAT_CONTENT_LIMIT - contextPrefixLength)。
    const hugePrefix = buildContextContent(
      '',
      ['s1'],
      [],
      [serverOf('s1', 'x'.repeat(CHAT_CONTENT_LIMIT))],
      [],
    );
    const contextPrefixLength = codePoints(hugePrefix);
    const composerMaxLength = Math.max(1, CHAT_CONTENT_LIMIT - contextPrefixLength);
    expect(composerMaxLength).toBe(1);
  });

  it('正常选择下 composerMaxLength = 16000 - 前缀长', () => {
    const prefix = buildContextContent(
      '',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'w')],
      [databaseOf('d1', 'd')],
    );
    const composerMaxLength = Math.max(1, CHAT_CONTENT_LIMIT - codePoints(prefix));
    expect(composerMaxLength).toBeGreaterThan(0);
    expect(composerMaxLength).toBeLessThan(CHAT_CONTENT_LIMIT);
    // 以预算正文发送恰好不超限。
    const content = buildContextContent(
      'a'.repeat(composerMaxLength),
      ['s1'],
      ['d1'],
      [serverOf('s1', 'w')],
      [databaseOf('d1', 'd')],
    );
    expect(codePoints(content)).toBeLessThanOrEqual(CHAT_CONTENT_LIMIT);
  });
});
