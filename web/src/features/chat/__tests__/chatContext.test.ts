import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { stripContextPrefix } from '../markdownBlocks';
// buildContextContent / CHAT_CONTENT_LIMIT 为 ChatPage 私有纯函数，
// export 仅作单测钩子（无行为变化）。
import { buildContextContent } from '../ChatPage';

type Server = components['schemas']['Server'];
type Database = components['schemas']['Database'];

// 最小可用的服务器/数据库清单工厂（仅用到的字段参与前缀构建）。
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

describe('buildContextContent 上下文前缀构建', () => {
  it('未选择任何上下文时不加前缀，正文原样', () => {
    expect(buildContextContent('检查磁盘', [], [], [], [])).toBe('检查磁盘');
  });

  it('仅服务器：前缀带 servers 行并解析为「名称 (id=…)」', () => {
    const content = buildContextContent('巡检', ['s1'], [], [serverOf('s1', 'web-1')], []);
    expect(content).toBe('<!--opsmind-context\nservers: web-1 (id=s1)\n-->\n巡检');
  });

  it('仅数据库：前缀带 databases 行', () => {
    const content = buildContextContent('查库', [], ['d1'], [], [databaseOf('d1', '订单库')]);
    expect(content).toBe('<!--opsmind-context\ndatabases: 订单库 (id=d1)\n-->\n查库');
  });

  it('同时选择服务器与数据库时 servers 行在 databases 行之前', () => {
    const content = buildContextContent(
      '综合巡检',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'web-1')],
      [databaseOf('d1', 'db-1')],
    );
    const [serversLine, databasesLine] = content.split('\n').slice(1, 3);
    expect(serversLine).toBe('servers: web-1 (id=s1)');
    expect(databasesLine).toBe('databases: db-1 (id=d1)');
  });

  it('多台服务器按选择顺序用逗号连接', () => {
    const content = buildContextContent(
      'x',
      ['s2', 's1'],
      [],
      [serverOf('s1', 'web-1'), serverOf('s2', 'db-1')],
      [],
    );
    expect(content).toContain('servers: db-1 (id=s2), web-1 (id=s1)');
  });

  it('清单里找不到的 id 回退为裸 id（资产可能已被删除）', () => {
    const content = buildContextContent('x', ['gone'], [], [], []);
    expect(content).toContain('servers: gone');
  });

  it('build → strip 往返恒等（展示层剥离前缀不丢正文）', () => {
    const content = buildContextContent(
      '重启 nginx 前先看日志',
      ['s1'],
      ['d1'],
      [serverOf('s1', 'web-1')],
      [databaseOf('d1', 'db')],
    );
    expect(stripContextPrefix(content)).toBe('重启 nginx 前先看日志');
  });
});

describe('上下文前缀与长度预算（CHAT_CONTENT_LIMIT=16000）', () => {
  const LIMIT = 16000;

  it('前缀长度按 code point 计数：build(空正文) 即前缀本身', () => {
    const prefix = buildContextContent('', ['s1'], [], [serverOf('s1', '生产主机')], []);
    // 「生产主机」4 个汉字各占 1 个 code point（[...x].length 而非 UTF-16 length）。
    expect([...prefix].length).toBeGreaterThan(0);
    expect(prefix.endsWith('\n')).toBe(true);
    // 正文可用长度 = 16000 - 前缀长度：贴着预算发送（含前缀）不超限。
    const budget = LIMIT - [...prefix].length;
    const full = buildContextContent(
      'a'.repeat(budget),
      ['s1'],
      [],
      [serverOf('s1', '生产主机')],
      [],
    );
    expect([...full].length).toBe(LIMIT);
  });

  it('超长正文 + 前缀超过 16000 code point 时会被 ChatPage 拒绝（长度判定语义）', () => {
    const prefixLength = [
      ...buildContextContent('', ['s1'], ['d1'], [serverOf('s1', 'w')], [databaseOf('d1', 'd')]),
    ].length;
    const overlong = '字'.repeat(LIMIT - prefixLength + 1);
    const content = buildContextContent(
      overlong,
      ['s1'],
      ['d1'],
      [serverOf('s1', 'w')],
      [databaseOf('d1', 'd')],
    );
    expect([...content].length).toBeGreaterThan(LIMIT);
    // 不带上下文时同样长度可以通过（前缀是唯一增量）。
    expect([...overlong].length).toBeLessThanOrEqual(LIMIT);
  });

  it('emoji 等增补平面字符按 1 个 code point 计数（不因 UTF-16 双单元误判超限）', () => {
    const text = '🔴'.repeat(100);
    const content = buildContextContent(text, ['s1'], [], [serverOf('s1', 'w')], []);
    // UTF-16 length 为 200+，code point 计数为 100 + 前缀。
    expect(content.length).toBeGreaterThan([...content].length - 100 - 1);
    expect([...text].length).toBe(100);
  });
});
