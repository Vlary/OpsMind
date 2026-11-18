import { describe, expect, it } from 'vitest';

import {
  hasEarlierUnverified,
  parseVerifySummary,
  verifyBrokenText,
  verifyCoverageText,
  verifyOkText,
} from './verifyResult';

const hash = 'a'.repeat(64);

describe('parseVerifySummary', () => {
  it('解析成功响应（含 legacy 计数与覆盖区间）', () => {
    expect(
      parseVerifySummary({
        ok: true,
        checked: 161,
        legacy: 3,
        head_id: 161,
        head_hash: hash,
        first_id: 1,
        window: 200,
      }),
    ).toEqual({
      checked: 161,
      legacy: 3,
      headId: 161,
      headHash: hash,
      brokenAt: null,
      firstId: 1,
      window: 200,
    });
  });

  it('解析 409 error.details，并保留断点 id 与区间', () => {
    expect(
      parseVerifySummary({
        checked: 7,
        legacy: 0,
        head_id: 8,
        head_hash: hash,
        broken_at: 8,
        first_id: 2,
        window: 200,
      }),
    ).toEqual({
      checked: 7,
      legacy: 0,
      headId: 8,
      headHash: hash,
      brokenAt: 8,
      firstId: 2,
      window: 200,
    });
  });

  // 旧后端 / 契约漂移时不臆造 0：缺关键字段即视为无法解析。
  it('缺 checked/head_id/head_hash 或非对象时返回 null', () => {
    expect(parseVerifySummary(null)).toBeNull();
    expect(parseVerifySummary('oops')).toBeNull();
    expect(parseVerifySummary({ checked: 1 })).toBeNull();
    expect(parseVerifySummary({ checked: 1, head_id: 1, head_hash: '' })).toBeNull();
  });

  it('legacy 缺省按 0 处理；first_id/window 缺省为 null（不臆造区间）', () => {
    const summary = parseVerifySummary({ checked: 2, head_id: 2, head_hash: hash });
    expect(summary?.legacy).toBe(0);
    expect(summary?.firstId).toBeNull();
    expect(summary?.window).toBeNull();
  });

  it('空区间 first_id=0 保留为 0（与字段缺失区分），且不产生覆盖文案', () => {
    const summary = parseVerifySummary({
      checked: 0,
      legacy: 0,
      head_id: 0,
      head_hash: hash,
      first_id: 0,
      window: 200,
    });
    expect(summary?.firstId).toBe(0);
    expect(summary && verifyCoverageText(summary)).toBeNull();
  });
});

describe('校验结论文案', () => {
  const base = { legacy: 0, headHash: hash, brokenAt: null };

  it('链完整时展示 checked 与链头，legacy 行如实说明只校验链接', () => {
    expect(verifyOkText({ ...base, checked: 5, headId: 9, firstId: null, window: null })).toBe(
      '哈希链校验通过：已校验 5 条，链头 #9',
    );
    expect(
      verifyOkText({ ...base, checked: 5, headId: 9, firstId: null, window: null, legacy: 2 }),
    ).toContain('其中 2 条为迁移前历史记录，按 legacy 规则仅校验链接');
  });

  // 第 2 轮走查 low：499 行的库只校验最近 200 条时，文案必须写出覆盖区间
  // 与「更早记录未校验」，不能让人读成全链通过。
  it('窗口有界时写出覆盖区间与未覆盖区间', () => {
    const text = verifyOkText({ ...base, checked: 200, headId: 499, firstId: 300, window: 200 });
    expect(text).toContain('已校验 200 条（#300–#499）');
    expect(text).toContain('更早记录（#1–#299）未在本次校验范围内');
    expect(text).toContain('链头 #499');
  });

  it('覆盖从 #1 起时不说「更早记录未校验」', () => {
    const text = verifyOkText({ ...base, checked: 499, headId: 499, firstId: 1, window: 200 });
    expect(text).toContain('（#1–#499）');
    expect(text).not.toContain('未在本次校验范围内');
  });

  it('旧后端（无 first_id）保持原文案，不臆造区间', () => {
    const text = verifyOkText({ ...base, checked: 200, headId: 499, firstId: null, window: null });
    expect(text).toBe('哈希链校验通过：已校验 200 条，链头 #499');
  });

  it('链断裂时露出断点 id；无法解析时明确「未知位置」', () => {
    expect(
      verifyBrokenText({
        ...base,
        checked: 5,
        headId: 9,
        brokenAt: 7,
        firstId: null,
        window: null,
      }),
    ).toBe('哈希链已破坏：断点 #7，已校验 5 条');
    expect(verifyBrokenText(null)).toContain('未返回断点信息');
  });

  it('链断裂且带区间时写出校验范围', () => {
    expect(
      verifyBrokenText({
        ...base,
        checked: 12,
        headId: 490,
        brokenAt: 485,
        firstId: 479,
        window: 200,
      }),
    ).toBe('哈希链已破坏：断点 #485，已校验 12 条（校验范围 #479–#490）');
  });
});

describe('覆盖区间判定', () => {
  const base = { legacy: 0, headHash: hash, brokenAt: null, window: 200 };

  it('firstId>1 表示存在未校验的更早记录', () => {
    expect(hasEarlierUnverified({ ...base, checked: 1, headId: 9, firstId: 2 })).toBe(true);
    expect(hasEarlierUnverified({ ...base, checked: 1, headId: 9, firstId: 1 })).toBe(false);
    // 旧后端没有区间信息：不声称任何一侧。
    expect(hasEarlierUnverified({ ...base, checked: 1, headId: 9, firstId: null })).toBeNull();
  });

  it('verifyCoverageText 只在 firstId>0 时给出区间', () => {
    expect(verifyCoverageText({ ...base, checked: 1, headId: 9, firstId: 8 })).toBe('#8–#9');
    expect(verifyCoverageText({ ...base, checked: 0, headId: 0, firstId: 0 })).toBeNull();
    expect(verifyCoverageText({ ...base, checked: 0, headId: 0, firstId: null })).toBeNull();
  });
});
