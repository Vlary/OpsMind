import { describe, expect, it } from 'vitest';

import { isValidUUID, normalizeUUID } from './uuidFilter';

// UUID 轻校验矩阵（audit_handlers.go 同款校验的前端镜像）：
// 与 uuidFilter.test.ts 互补，逐位边界覆盖。

describe('isValidUUID 合法值矩阵', () => {
  it('标准 v4 形态', () => {
    expect(isValidUUID('550e8400-e29b-41d4-a716-446655440000')).toBe(true);
  });

  it('全大写与大小写混合', () => {
    expect(isValidUUID('550E8400-E29B-41D4-A716-446655440000')).toBe(true);
    expect(isValidUUID('550e8400-E29B-41d4-a716-446655440000')).toBe(true);
  });

  it('全 0 与全 f 的边界形态', () => {
    expect(isValidUUID('00000000-0000-0000-0000-000000000000')).toBe(true);
    expect(isValidUUID('ffffffff-ffff-ffff-ffff-ffffffffffff')).toBe(true);
  });

  it('版本位与变体位任意 hex 均合法（不校验 UUID 版本语义）', () => {
    expect(isValidUUID('12345678-90ab-cdef-0123-456789abcdef')).toBe(true);
    expect(isValidUUID('99999999-9999-9999-9999-999999999999')).toBe(true);
  });
});

describe('isValidUUID 非法值矩阵', () => {
  it('空串与空白', () => {
    expect(isValidUUID('')).toBe(false);
    expect(isValidUUID(' ')).toBe(false);
    expect(isValidUUID('   ')).toBe(false);
  });

  it('大括号 / urn 前缀等规范变体不被接受（后端同样拒绝）', () => {
    expect(isValidUUID('{550e8400-e29b-41d4-a716-446655440000}')).toBe(false);
    expect(isValidUUID('urn:uuid:550e8400-e29b-41d4-a716-446655440000')).toBe(false);
  });

  it('末尾少一位 / 多一位', () => {
    expect(isValidUUID('550e8400-e29b-41d4-a716-44665544000')).toBe(false);
    expect(isValidUUID('550e8400-e29b-41d4-a716-4466554400000')).toBe(false);
  });

  it('hex 之外的字母（g/G）', () => {
    expect(isValidUUID('550e8400-e29b-41d4-a716-44665544000g')).toBe(false);
    expect(isValidUUID('g50e8400-e29b-41d4-a716-446655440000')).toBe(false);
  });

  it('横线位置错位', () => {
    expect(isValidUUID('550e8400e29b-41d4-a716-446655440000')).toBe(false);
    expect(isValidUUID('550e8400-e29b4-1d4a-7164-46655440000')).toBe(false);
  });

  it('分隔符替换为下划线 / 空格', () => {
    expect(isValidUUID('550e8400_e29b_41d4_a716_446655440000')).toBe(false);
    expect(isValidUUID('550e8400 e29b 41d4 a716 446655440000')).toBe(false);
  });

  it('十六进制数值不带横线（32 连号）', () => {
    expect(isValidUUID('550e8400e29b41d4a716446655440000')).toBe(false);
  });

  it('含前导或尾随空白', () => {
    expect(isValidUUID(' 550e8400-e29b-41d4-a716-446655440000')).toBe(false);
    expect(isValidUUID('550e8400-e29b-41d4-a716-446655440000 ')).toBe(false);
  });

  it('随机日常输入', () => {
    expect(isValidUUID('abc')).toBe(false);
    expect(isValidUUID('conversation-id')).toBe(false);
    expect(isValidUUID('550e8400-e29b-41d4-a716-446655440000\n')).toBe(false);
  });
});

// 第 4 轮打磨：走查复现（/tmp/walk4/walk6c.mjs）——粘贴全大写 UUID 过校验
// 但筛选 0 行且无提示；后端资源 ID 恒小写，提交前必须归一化。
describe('normalizeUUID 提交前归一化', () => {
  it('全大写 → 小写（与库内小写记录同值命中）', () => {
    expect(normalizeUUID('550E8400-E29B-41D4-A716-446655440000')).toBe(
      '550e8400-e29b-41d4-a716-446655440000',
    );
  });

  it('大小写混合 → 小写', () => {
    expect(normalizeUUID('550e8400-E29B-41d4-a716-446655440000')).toBe(
      '550e8400-e29b-41d4-a716-446655440000',
    );
  });

  it('去首尾空白（粘贴带入的换行/空格）', () => {
    expect(normalizeUUID('  550e8400-e29b-41d4-a716-446655440000\n')).toBe(
      '550e8400-e29b-41d4-a716-446655440000',
    );
  });

  it('已是小写的值不变（幂等）', () => {
    const value = '550e8400-e29b-41d4-a716-446655440000';
    expect(normalizeUUID(normalizeUUID(value))).toBe(value);
  });

  it('空串与纯空白归一为空串（走清除筛选路径）', () => {
    expect(normalizeUUID('')).toBe('');
    expect(normalizeUUID('   ')).toBe('');
  });

  it('归一化产物仍过校验：大写输入归一后 isValidUUID 恒真', () => {
    expect(isValidUUID(normalizeUUID('550E8400-E29B-41D4-A716-446655440000'))).toBe(true);
    // 非法输入归一后依旧非法（校验在归一化之后仍兜底）。
    expect(isValidUUID(normalizeUUID('NOT-A-UUID'))).toBe(false);
  });
});
