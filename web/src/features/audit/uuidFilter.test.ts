import { describe, expect, it } from 'vitest';

import { isValidUUID } from './uuidFilter';

describe('isValidUUID', () => {
  it('接受标准 uuid（大小写不敏感）', () => {
    expect(isValidUUID('123e4567-e89b-12d3-a456-426614174000')).toBe(true);
    expect(isValidUUID('123E4567-E89B-12D3-A456-426614174000')).toBe(true);
  });

  it('拒绝非法值：后端会以 VALIDATION_FAILED 拒绝的输入前端也不放行', () => {
    expect(isValidUUID('not-a-uuid')).toBe(false);
    expect(isValidUUID('123e4567e89b12d3a456426614174000')).toBe(false); // 缺连字符
    expect(isValidUUID('123e4567-e89b-12d3-a456-42661417400')).toBe(false); // 少一位
    expect(isValidUUID('123e4567-e89b-12d3-a456-4266141740000')).toBe(false); // 多一位
    expect(isValidUUID('123e4567-e89b-12d3-a456-42661417400g')).toBe(false); // 非十六进制
    expect(isValidUUID(' 123e4567-e89b-12d3-a456-426614174000')).toBe(false); // 前导空格
  });

  it('空字符串返回 false（调用方先判空再校验，空值走清除筛选路径）', () => {
    expect(isValidUUID('')).toBe(false);
  });
});
