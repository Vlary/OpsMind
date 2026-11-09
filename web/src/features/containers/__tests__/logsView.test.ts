import { describe, expect, it } from 'vitest';

import { DEFAULT_TAIL, TAIL_CHOICES, normalizeTail, splitLogLines } from '../logsView';

describe('normalizeTail（契约 5.2C：tail 1..1000，0/非法视为缺省 200）', () => {
  it('四档取值原样通过', () => {
    for (const value of TAIL_CHOICES) {
      expect(normalizeTail(value)).toBe(value);
    }
  });

  it('非法值回落 200', () => {
    expect(normalizeTail(0)).toBe(DEFAULT_TAIL);
    expect(normalizeTail(-5)).toBe(DEFAULT_TAIL);
    expect(normalizeTail(1001)).toBe(DEFAULT_TAIL);
    expect(normalizeTail(Number.NaN)).toBe(DEFAULT_TAIL);
    expect(normalizeTail(null)).toBe(DEFAULT_TAIL);
    expect(normalizeTail(undefined)).toBe(DEFAULT_TAIL);
  });

  it('小数四舍五入到整数', () => {
    expect(normalizeTail(199.6)).toBe(200);
    expect(normalizeTail(100.4)).toBe(100);
  });
});

describe('splitLogLines（契约 8.7：输出按行渲染）', () => {
  it('空串为空数组（空态）', () => {
    expect(splitLogLines('')).toEqual([]);
  });

  it('按换行拆分；末尾换行不产生多余空行', () => {
    expect(splitLogLines('line1\nline2\n')).toEqual(['line1', 'line2']);
    expect(splitLogLines('line1\nline2')).toEqual(['line1', 'line2']);
  });

  it('中间空行保留（日志原文语义）', () => {
    expect(splitLogLines('a\n\nb')).toEqual(['a', '', 'b']);
  });
});
