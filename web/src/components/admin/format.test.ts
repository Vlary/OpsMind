import { describe, expect, it } from 'vitest';

import {
  formatCell,
  formatDateTime,
  formatDuration,
  prettyJson,
  readBoolean,
  readNumber,
  readString,
  shortId,
  truncate,
} from './format';

// 资源管理页共用纯展示格式化函数的矩阵覆盖（此前无任何单测）。
// formatDateTime 的具体输出依赖本机时区，采用与实现同构的
// Intl.DateTimeFormat('zh-CN') 对照断言，避免环境差异导致的脆弱期望。

const SAME_FORMATTER = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

describe('formatDateTime', () => {
  it('null / undefined / 空串统一显示占位符 —', () => {
    expect(formatDateTime(null)).toBe('—');
    expect(formatDateTime(undefined)).toBe('—');
    expect(formatDateTime('')).toBe('—');
  });

  it('非法日期串原样返回（不吞错也不显示 Invalid Date）', () => {
    expect(formatDateTime('not-a-date')).toBe('not-a-date');
    expect(formatDateTime('2026-13-45')).toBe('2026-13-45');
  });

  it('合法 ISO 时间按 zh-CN 24 小时制格式化（与时区无关的同构断言）', () => {
    const iso = '2026-01-01T08:30:45Z';
    expect(formatDateTime(iso)).toBe(SAME_FORMATTER.format(new Date(iso)));
  });

  it('不同时间戳格式化结果随值变化', () => {
    const a = formatDateTime('2026-01-01T00:00:00Z');
    const b = formatDateTime('2026-06-15T12:00:00Z');
    expect(a).not.toBe(b);
    expect(a).toMatch(/\d/);
  });

  it('带毫秒与 Z 后缀的时间可解析', () => {
    const iso = '2026-10-25T12:00:00.123Z';
    expect(formatDateTime(iso)).toBe(SAME_FORMATTER.format(new Date(iso)));
  });
});

describe('formatDuration', () => {
  it('null / undefined 显示占位符 —', () => {
    expect(formatDuration(null)).toBe('—');
    expect(formatDuration(undefined)).toBe('—');
  });

  it('0 与小于 1000ms 显示毫秒', () => {
    expect(formatDuration(0)).toBe('0 ms');
    expect(formatDuration(1)).toBe('1 ms');
    expect(formatDuration(999)).toBe('999 ms');
  });

  it('恰好 1000ms 起显示秒（保留两位小数）', () => {
    expect(formatDuration(1000)).toBe('1.00 s');
    expect(formatDuration(1500)).toBe('1.50 s');
    expect(formatDuration(1234)).toBe('1.23 s');
  });

  it('分钟级时长按秒展示', () => {
    expect(formatDuration(60_000)).toBe('60.00 s');
    expect(formatDuration(299_999)).toBe('300.00 s');
  });

  it('毫秒到秒的进位舍入到两位小数', () => {
    expect(formatDuration(1999)).toBe('2.00 s');
    expect(formatDuration(105)).toBe('105 ms');
  });
});

describe('truncate', () => {
  it('短于上限原样返回', () => {
    expect(truncate('short')).toBe('short');
  });

  it('恰好等于上限不加省略号', () => {
    const text = 'a'.repeat(60);
    expect(truncate(text)).toBe(text);
  });

  it('超上限截断并追加省略号', () => {
    expect(truncate('a'.repeat(61))).toBe(`${'a'.repeat(60)}…`);
    expect(truncate('a'.repeat(100), 10)).toBe(`${'a'.repeat(10)}…`);
  });

  it('自定义上限 0：任何非空文本都截为省略号', () => {
    expect(truncate('abc', 0)).toBe('…');
  });

  it('空字符串原样', () => {
    expect(truncate('')).toBe('');
  });
});

describe('shortId', () => {
  it('null / undefined / 空串显示占位符 —', () => {
    expect(shortId(null)).toBe('—');
    expect(shortId(undefined)).toBe('—');
    expect(shortId('')).toBe('—');
  });

  it('短于等于长度上限原样返回', () => {
    expect(shortId('abc123')).toBe('abc123');
    expect(shortId('12345678')).toBe('12345678');
  });

  it('超长截断到默认 8 位', () => {
    expect(shortId('1234567890abcdef')).toBe('12345678');
  });

  it('自定义长度', () => {
    expect(shortId('abcdefghijklmnop', 4)).toBe('abcd');
    expect(shortId('abcdefghijklmnop', 20)).toBe('abcdefghijklmnop');
  });
});

describe('formatCell（SQL 结果单元格）', () => {
  it('null / undefined 显示 NULL', () => {
    expect(formatCell(null)).toBe('NULL');
    expect(formatCell(undefined)).toBe('NULL');
  });

  it('字符串原样（不加引号）', () => {
    expect(formatCell('hello')).toBe('hello');
    expect(formatCell('')).toBe('');
  });

  it('数字 / 布尔 / bigint 转 String', () => {
    expect(formatCell(42)).toBe('42');
    expect(formatCell(3.14)).toBe('3.14');
    expect(formatCell(true)).toBe('true');
    expect(formatCell(false)).toBe('false');
    expect(formatCell(10n)).toBe('10');
  });

  it('数组与对象 JSON 序列化', () => {
    expect(formatCell([1, 2])).toBe('[1,2]');
    expect(formatCell({ a: 1 })).toBe('{"a":1}');
  });

  it('循环引用对象序列化失败回退 String(value)', () => {
    const cyclic: Record<string, unknown> = {};
    cyclic['self'] = cyclic;
    // String(循环对象) 输出 [object Object] 而不抛错。
    expect(formatCell(cyclic)).toBe('[object Object]');
  });

  it('负数与 0 不被误判为空', () => {
    expect(formatCell(0)).toBe('0');
    expect(formatCell(-1)).toBe('-1');
  });
});

describe('prettyJson（审计原始 detail）', () => {
  it('对象按 2 空格缩进序列化', () => {
    expect(prettyJson({ a: 1, b: { c: 2 } })).toBe('{\n  "a": 1,\n  "b": {\n    "c": 2\n  }\n}');
  });

  it('数组同样缩进序列化', () => {
    expect(prettyJson([1, 'x'])).toBe('[\n  1,\n  "x"\n]');
  });

  it('原始值字符串化', () => {
    expect(prettyJson('text')).toBe('"text"');
    expect(prettyJson(7)).toBe('7');
  });

  it('undefined（JSON.stringify 返回 undefined）回退 String()', () => {
    expect(prettyJson(undefined)).toBe('undefined');
    expect(prettyJson(() => 1)).toContain('=>');
  });

  it('循环引用回退 String 而不抛错', () => {
    const cyclic: Record<string, unknown> = {};
    cyclic['self'] = cyclic;
    expect(prettyJson(cyclic)).toBe('[object Object]');
  });
});

describe('readString / readNumber / readBoolean 读取矩阵', () => {
  it('readString：source 为 null 返回 null', () => {
    expect(readString(null, ['a'])).toBeNull();
  });

  it('readString：按 key 顺序取第一个非空字符串', () => {
    expect(readString({ a: '', b: 'x' }, ['a', 'b'])).toBe('x');
    expect(readString({ a: 'first', b: 'second' }, ['a', 'b'])).toBe('first');
    expect(readString({ b: 'second' }, ['a', 'b'])).toBe('second');
  });

  it('readString：非字符串值跳过；全部未命中返回 null', () => {
    expect(readString({ a: 1, b: true }, ['a', 'b'])).toBeNull();
    expect(readString({}, ['a'])).toBeNull();
    expect(readString({ a: null }, ['a'])).toBeNull();
  });

  it('readNumber：source 为 null 返回 null', () => {
    expect(readNumber(null, ['n'])).toBeNull();
  });

  it('readNumber：有限数字命中', () => {
    expect(readNumber({ n: 42 }, ['n'])).toBe(42);
    expect(readNumber({ a: 'not-number', n: 3 }, ['a', 'n'])).toBe(3);
    expect(readNumber({ n: 0 }, ['n'])).toBe(0);
  });

  it('readNumber：NaN / Infinity / 字符串数字均不命中', () => {
    expect(readNumber({ n: Number.NaN }, ['n'])).toBeNull();
    expect(readNumber({ n: Number.POSITIVE_INFINITY }, ['n'])).toBeNull();
    expect(readNumber({ n: '42' }, ['n'])).toBeNull();
  });

  it('readBoolean：source 为 null 返回 null', () => {
    expect(readBoolean(null, ['ok'])).toBeNull();
  });

  it('readBoolean：布尔值命中（含 false）', () => {
    expect(readBoolean({ ok: true }, ['ok'])).toBe(true);
    expect(readBoolean({ ok: false }, ['ok'])).toBe(false);
    expect(readBoolean({ a: 'true', ok: false }, ['a', 'ok'])).toBe(false);
  });

  it('readBoolean：非布尔值（含 0/1/字符串）不命中', () => {
    expect(readBoolean({ ok: 1 }, ['ok'])).toBeNull();
    expect(readBoolean({ ok: 'true' }, ['ok'])).toBeNull();
    expect(readBoolean({ ok: null }, ['ok'])).toBeNull();
  });

  it('keys 为空数组：一律返回 null', () => {
    expect(readString({ a: 'x' }, [])).toBeNull();
    expect(readNumber({ a: 1 }, [])).toBeNull();
    expect(readBoolean({ a: true }, [])).toBeNull();
  });
});
