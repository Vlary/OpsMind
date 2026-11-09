import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { makeFailure } from '../../../components/admin/apiError';
import { buildCreatePayload, buildUpdatePayload, resolvePort } from '../DatabaseFormDrawer';
import {
  consoleSpecFor,
  consoleTarget,
  describeQueryFailure,
  mongoCommandHints,
  validateMongoDocument,
} from '../SqlConsole';
import { databaseAddress } from '../DatabasesPage';

type Database = components['schemas']['Database'];

function databaseOf(overrides: Partial<Database> = {}): Database {
  return {
    id: 'db-1',
    name: 'orders',
    engine: 'postgres',
    host: '10.0.0.7',
    port: 5432,
    database_name: 'app',
    username: 'readonly',
    sqlite_path: '',
    instance_name: '',
    redis_db: 0,
    auth_db: '',
    ssl_mode: 'prefer',
    has_password: true,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

// 二期 F2：数据库表单 mssql / redis 引擎扩展（契约 2.6 / 4.4 / 8.7）。

describe('resolvePort mssql 端口与命名实例互斥', () => {
  it('非 mssql 引擎：直接返回表单端口或默认端口', () => {
    expect(resolvePort('postgres', undefined, '')).toBe(5432);
    expect(resolvePort('redis', 6380, 'ignored')).toBe(6380);
    expect(resolvePort('redis', undefined, '')).toBe(6379);
  });

  it('mssql + 命名实例：端口强制 0（经 SQL Server Browser 解析）', () => {
    expect(resolvePort('mssql', 1433, 'SQLEXPRESS')).toBe(0);
    expect(resolvePort('mssql', undefined, ' SQLEXPRESS ')).toBe(0);
  });

  it('mssql 无实例：端口直连，未填时默认 1433', () => {
    expect(resolvePort('mssql', 14330, '')).toBe(14330);
    expect(resolvePort('mssql', undefined, '  ')).toBe(1433);
  });
});

describe('buildCreatePayload 按引擎组装', () => {
  it('redis：host/port/username 可选密码/redis_db，不含 database_name 与 instance_name', () => {
    const payload = buildCreatePayload({
      name: 'cache',
      engine: 'redis',
      host: '10.0.0.9',
      port: 6379,
      username: 'app',
      password: 'secret',
      redis_db: 3,
      ssl_mode: 'require',
    });
    expect(payload).toEqual({
      name: 'cache',
      engine: 'redis',
      host: '10.0.0.9',
      port: 6379,
      username: 'app',
      password: 'secret',
      redis_db: 3,
      ssl_mode: 'require',
    });
  });

  it('redis：可选项缺省为空用户名 / db 0 / 无密码', () => {
    const payload = buildCreatePayload({ name: 'cache', engine: 'redis', host: 'h', port: 6379 });
    expect(payload.username).toBe('');
    expect(payload.redis_db).toBe(0);
    expect(payload.password).toBeUndefined();
  });

  it('mssql 端口直连：默认 1433，携带 database_name/username/password', () => {
    const payload = buildCreatePayload({
      name: 'erp',
      engine: 'mssql',
      host: '10.0.0.11',
      database_name: 'master',
      username: 'sa',
      password: 'pw',
      ssl_mode: 'require',
    });
    expect(payload.port).toBe(1433);
    expect(payload.instance_name).toBe('');
    expect(payload.password).toBe('pw');
  });

  it('mssql 命名实例：port 归零且 instance_name 透传（首尾空白裁剪）', () => {
    const payload = buildCreatePayload({
      name: 'erp',
      engine: 'mssql',
      host: '10.0.0.11',
      port: 1433,
      instance_name: ' SQLEXPRESS ',
      database_name: 'master',
      username: 'sa',
      password: 'pw',
    });
    expect(payload.port).toBe(0);
    expect(payload.instance_name).toBe('SQLEXPRESS');
  });

  it('postgres 沿用一期组装（不带二期字段）', () => {
    const payload = buildCreatePayload({
      name: 'pg',
      engine: 'postgres',
      host: 'h',
      port: 5432,
      database_name: 'app',
      username: 'u',
      ssl_mode: 'prefer',
    });
    expect(payload.engine).toBe('postgres');
    expect(payload).not.toHaveProperty('instance_name');
    expect(payload).not.toHaveProperty('redis_db');
  });
});

describe('buildUpdatePayload 按引擎组装', () => {
  it('redis：不发送 database_name，携带 redis_db/username', () => {
    const payload = buildUpdatePayload({
      name: 'cache',
      engine: 'redis',
      host: '10.0.0.9',
      port: 6380,
      username: '',
      redis_db: 7,
      ssl_mode: 'prefer',
    });
    expect(payload.host).toBe('10.0.0.9');
    expect(payload.port).toBe(6380);
    expect(payload.username).toBe('');
    expect(payload.redis_db).toBe(7);
    expect(payload).not.toHaveProperty('database_name');
    expect(payload).not.toHaveProperty('instance_name');
  });

  it('mssql：instance 非空时 update 同样 port=0', () => {
    const payload = buildUpdatePayload({
      name: 'erp',
      engine: 'mssql',
      host: 'h',
      port: 1433,
      instance_name: 'SQLEXPRESS',
      database_name: 'master',
      username: 'sa',
      ssl_mode: 'prefer',
    });
    expect(payload.port).toBe(0);
    expect(payload.instance_name).toBe('SQLEXPRESS');
  });

  it('redis 清除密码：clear_password 且无新密码时发送空字符串', () => {
    const payload = buildUpdatePayload({
      name: 'cache',
      engine: 'redis',
      host: 'h',
      port: 6379,
      clear_password: true,
    });
    expect(payload.password).toBe('');
  });
});

// 三期（N3）：engine=mongodb 分支（契约 2.6 / 4.4 / 7.6 / 8.7）。
describe('buildCreatePayload mongodb（三期）', () => {
  it('未填端口时默认 27017；auth_db 透传（空 = admin）', () => {
    const payload = buildCreatePayload({
      name: 'mongo',
      engine: 'mongodb',
      host: '10.0.0.20',
      database_name: 'app',
      username: '',
      auth_db: '',
      ssl_mode: 'prefer',
    });
    expect(payload.engine).toBe('mongodb');
    expect(payload.port).toBe(27017);
    expect(payload.auth_db).toBe('');
    expect(payload.password).toBeUndefined();
    expect(payload).not.toHaveProperty('instance_name');
    expect(payload).not.toHaveProperty('redis_db');
  });

  it('有认证：username/password 成对携带；auth_db 自定义', () => {
    const payload = buildCreatePayload({
      name: 'mongo',
      engine: 'mongodb',
      host: '10.0.0.20',
      port: 27018,
      database_name: 'app',
      username: 'readonly',
      password: 'pw',
      auth_db: 'users',
      ssl_mode: 'require',
    });
    expect(payload.port).toBe(27018);
    expect(payload.username).toBe('readonly');
    expect(payload.password).toBe('pw');
    expect(payload.auth_db).toBe('users');
  });
});

describe('buildUpdatePayload mongodb（三期）', () => {
  it('常规更新携带 host/port/database_name/auth_db/ssl_mode', () => {
    const payload = buildUpdatePayload({
      name: 'mongo',
      engine: 'mongodb',
      host: '10.0.0.21',
      port: 27017,
      database_name: 'app2',
      username: 'readonly',
      auth_db: 'admin2',
      ssl_mode: 'verify-ca',
    });
    expect(payload.host).toBe('10.0.0.21');
    expect(payload.port).toBe(27017);
    expect(payload.database_name).toBe('app2');
    expect(payload.auth_db).toBe('admin2');
    expect(payload.ssl_mode).toBe('verify-ca');
    expect(payload.password).toBeUndefined();
  });

  it('清除认证：username 与 password 同时清空（契约：清空须两者同时清）', () => {
    const payload = buildUpdatePayload({
      name: 'mongo',
      engine: 'mongodb',
      host: '10.0.0.21',
      database_name: 'app',
      username: '',
      auth_db: '',
      clear_password: true,
    });
    expect(payload.username).toBe('');
    expect(payload.password).toBe('');
  });
});

describe('validateMongoDocument 命令文档本地校验（三期）', () => {
  it('合法 find/aggregate 文档通过', () => {
    expect(validateMongoDocument('{"find":"users","filter":{},"limit":10}')).toBeNull();
    expect(
      validateMongoDocument('{"aggregate":"o","pipeline":[{"$group":{"_id":"$s"}}]}'),
    ).toBeNull();
  });

  it('JSON 语法错误返回中文错误（含原始错误信息）', () => {
    const error = validateMongoDocument('{"find":"users",}');
    expect(error).toContain('不是合法的 JSON 命令文档');
  });

  it('数组/字面量被拒绝：必须是 JSON 对象', () => {
    expect(validateMongoDocument('[{"find":"users"}]')).toContain('JSON 对象');
    expect(validateMongoDocument('"find"')).toContain('JSON 对象');
  });

  it('顶层命令键不在白名单或出现多个命令键时拒绝', () => {
    expect(validateMongoDocument('{"drop":"x"}')).toContain('顶层命令键');
    expect(validateMongoDocument('{"find":"a","count":"b"}')).toContain('一个顶层命令键');
  });

  it('空输入提示输入命令文档', () => {
    expect(validateMongoDocument('   ')).toContain('请输入');
  });
});

describe('consoleSpecFor / consoleTarget / mongoCommandHints mongodb（三期）', () => {
  it('JSON 查询模式：标题/初始文档/占位符/白名单文案（契约 8.7）', () => {
    const spec = consoleSpecFor('mongodb');
    expect(spec.title).toBe('JSON 查询');
    expect(spec.initial).toBe('{"find":"users","filter":{},"limit":10}');
    expect(spec.placeholder).toContain('"aggregate"');
    expect(spec.allowedRule).toContain(
      'find / aggregate / count / distinct / estimatedDocumentCount',
    );
    expect(spec.emptyHint).toContain('JSON 命令文档');
  });

  it('连接描述：host:port/db（AuthSource 缺省 admin，自定义透传）', () => {
    expect(
      consoleTarget(
        databaseOf({ engine: 'mongodb', host: 'm', port: 27017, database_name: 'd', auth_db: '' }),
      ),
    ).toBe('m:27017/d（AuthSource admin）');
    expect(
      consoleTarget(
        databaseOf({
          engine: 'mongodb',
          host: 'm',
          port: 27017,
          database_name: 'd',
          auth_db: 'users',
        }),
      ),
    ).toBe('m:27017/d（AuthSource users）');
  });

  it('schema 提示覆盖五个命令与限制说明', () => {
    const hints = mongoCommandHints();
    expect(hints.join('\n')).toContain('find');
    expect(hints.join('\n')).toContain('aggregate');
    expect(hints.join('\n')).toContain('count');
    expect(hints.join('\n')).toContain('distinct');
    expect(hints.join('\n')).toContain('estimatedDocumentCount');
    expect(hints.join('\n')).toContain('maxTimeMS');
  });
});

describe('describeQueryFailure mongodb 失败文案（三期）', () => {
  it('SQL_QUERY_FAILED 标题去 SQL 化为「查询执行失败」，原因透出', () => {
    expect(
      describeQueryFailure(makeFailure('SQL_QUERY_FAILED', 'server selection error'), 'mongodb'),
    ).toBe('查询执行失败（server selection error）');
  });

  it('SQL_NOT_READONLY 保持通用映射（守卫拒绝展示）', () => {
    expect(
      describeQueryFailure(makeFailure('SQL_NOT_READONLY', 'top-level insert'), 'mongodb'),
    ).toBe('仅支持只读查询，写操作不会执行（top-level insert）');
  });

  // p3 sprint 3：后端查询失败 message 固定前置「查询失败：」（databases.go
  // describeDatabaseFailure）。实测缺陷形态（request_id 28dc56401103a088）：
  // 「查询执行失败（查询失败：无法连接 127.0.0.1:27017：连接被拒绝…）」
  // 双重「查询失败」——剥离前缀后括号内直接从分类短语开始。
  it('SQL_QUERY_FAILED 剥离后端「查询失败：」前缀，不再双重拼接', () => {
    expect(
      describeQueryFailure(
        makeFailure(
          'SQL_QUERY_FAILED',
          '查询失败：无法连接 127.0.0.1:27017：连接被拒绝（端口未开放或被防火墙拦截）；原因：dbproxy: mongo ping: server selection error',
        ),
        'mongodb',
      ),
    ).toBe(
      '查询执行失败（无法连接 127.0.0.1:27017：连接被拒绝（端口未开放或被防火墙拦截）；原因：dbproxy: mongo ping: server selection error）',
    );
  });

  it('message 不以「查询失败：」开头时不做剥离（如英文底层原因）', () => {
    expect(
      describeQueryFailure(makeFailure('SQL_QUERY_FAILED', 'server selection error'), 'mongodb'),
    ).toBe('查询执行失败（server selection error）');
  });

  it('redis 口径不受影响', () => {
    expect(describeQueryFailure(makeFailure('SQL_QUERY_FAILED', 'boom'), 'redis')).toBe(
      '命令执行失败（boom）',
    );
  });
});

describe('databaseAddress 连接列文本', () => {
  it('redis：host:port + db 序号（username 缺省不显示）', () => {
    expect(
      databaseAddress(
        databaseOf({ engine: 'redis', host: 'r', port: 6379, redis_db: 2, username: '' }),
      ),
    ).toEqual({
      main: 'r:6379',
      sub: 'db 2',
    });
  });

  it('redis：有 ACL 用户名时副行拼接', () => {
    expect(
      databaseAddress(databaseOf({ engine: 'redis', host: 'r', port: 6379, username: 'app' })),
    ).toEqual({ main: 'r:6379', sub: 'db 0 · app' });
  });

  it('mssql 命名实例：host\\instance', () => {
    expect(
      databaseAddress(
        databaseOf({
          engine: 'mssql',
          host: 'm',
          port: 0,
          instance_name: 'SQLEXPRESS',
          database_name: 'd',
          username: 'sa',
        }),
      ),
    ).toEqual({ main: 'm\\SQLEXPRESS', sub: 'd · sa' });
  });

  it('mssql 端口直连：host:port', () => {
    expect(
      databaseAddress(
        databaseOf({ engine: 'mssql', host: 'm', port: 1433, database_name: 'd', username: 'sa' }),
      ),
    ).toEqual({ main: 'm:1433', sub: 'd · sa' });
  });

  it('postgres/sqlite 沿用一期渲染', () => {
    expect(databaseAddress(databaseOf())).toEqual({ main: '10.0.0.7:5432', sub: 'app · readonly' });
    expect(
      databaseAddress(databaseOf({ engine: 'sqlite', sqlite_path: '/data/db.sqlite' })),
    ).toEqual({
      main: '/data/db.sqlite',
      sub: '',
    });
  });
});

describe('consoleSpecFor 查询抽屉按引擎适配（契约 8.7）', () => {
  it('redis：命令模式——标题/初始语句/占位符/白名单文案', () => {
    const spec = consoleSpecFor('redis');
    expect(spec.title).toBe('命令查询');
    expect(spec.initial).toBe('PING');
    expect(spec.placeholder).toContain('GET');
    expect(spec.placeholder).toContain('ZRANGE key 0 -1 WITHSCORES');
    expect(spec.allowedRule).toContain('白名单');
    expect(spec.emptyHint).toBe('请输入要执行的命令');
  });

  it('mssql：SELECT 单语句语义（无 EXPLAIN/EXEC）', () => {
    const spec = consoleSpecFor('mssql');
    expect(spec.title).toBe('SQL 查询');
    expect(spec.allowedRule).toContain('SELECT');
    expect(spec.allowedRule).not.toContain('EXPLAIN');
    expect(spec.placeholder).toContain('TOP');
  });

  it('postgres/mysql/sqlite 沿用一期文案（含 EXPLAIN）', () => {
    for (const engine of ['postgres', 'mysql', 'sqlite'] as const) {
      const spec = consoleSpecFor(engine);
      expect(spec.title).toBe('SQL 查询');
      expect(spec.initial).toBe('SELECT 1;');
      expect(spec.allowedRule).toContain('EXPLAIN');
    }
  });
});

describe('consoleTarget 连接描述', () => {
  it('redis：host:port/db N', () => {
    expect(consoleTarget(databaseOf({ engine: 'redis', host: 'r', port: 6379, redis_db: 5 }))).toBe(
      'r:6379/db 5',
    );
  });

  it('mssql 命名实例：host\\instance/database', () => {
    expect(
      consoleTarget(
        databaseOf({ engine: 'mssql', host: 'm', instance_name: 'SQLEXPRESS', database_name: 'd' }),
      ),
    ).toBe('m\\SQLEXPRESS/d');
  });

  it('postgres：host:port/database', () => {
    expect(consoleTarget(databaseOf())).toBe('10.0.0.7:5432/app');
  });
});

// p2 sprint 3：redis 命令抽屉的失败标题与命令口径一致（不再弹出
// 「SQL 执行失败」）；底层原因照常拼入，其他引擎/错误码走通用映射。
describe('describeQueryFailure 失败文案按引擎适配（p2 sprint 3）', () => {
  it('redis + SQL_QUERY_FAILED：标题「命令执行失败」，底层原因透出', () => {
    expect(
      describeQueryFailure(
        makeFailure(
          'SQL_QUERY_FAILED',
          '查询失败：dial tcp 127.0.0.1:16379: connect: connection refused',
        ),
        'redis',
      ),
    ).toBe('命令执行失败（查询失败：dial tcp 127.0.0.1:16379: connect: connection refused）');
  });

  it('redis + SQL_QUERY_FAILED + 空原因：只显示标题', () => {
    expect(describeQueryFailure(makeFailure('SQL_QUERY_FAILED', ''), 'redis')).toBe('命令执行失败');
  });

  it('redis + 其他错误码（SQL_NOT_READONLY）：保持通用映射', () => {
    expect(
      describeQueryFailure(makeFailure('SQL_NOT_READONLY', 'write command rejected'), 'redis'),
    ).toBe('仅支持只读查询，写操作不会执行（write command rejected）');
  });

  it('非 redis 引擎 + SQL_QUERY_FAILED：保持通用「SQL 执行失败」', () => {
    expect(
      describeQueryFailure(makeFailure('SQL_QUERY_FAILED', 'query failed: boom'), 'postgres'),
    ).toBe('SQL 执行失败（query failed: boom）');
  });
});
