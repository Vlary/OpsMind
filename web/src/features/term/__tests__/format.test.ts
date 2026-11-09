import { describe, expect, it } from 'vitest';

import { formatBytes } from '../format';

// formatBytes 的进制矩阵：默认 1024（KiB，终端/回放自产数据口径），
// 容器镜像传 1000（SI，与 docker CLI human size / 后端 parseHumanSize
// 的解析口径一致——四期走查 T6：412000000B 曾被渲染成 393 MB）。

describe('formatBytes 默认 1024 进制（既有口径不回归）', () => {
  it.each([
    [0, '0 B'],
    [1023, '1023 B'],
    [1024, '1.00 KB'],
    [1048576, '1.00 MB'],
    [1073741824, '1.00 GB'],
    [1099511627776, '1.00 TB'],
    // units 封顶 TB：1024 TiB 不进位到 PB（既有行为，PB 分支不可达）。
    [1125899906842624, '1024 TB'],
  ])('%d → %s', (bytes, text) => {
    expect(formatBytes(bytes)).toBe(text);
  });

  it('走查 T6 的 412000000B 保持 1024 进制渲染（393 MB）', () => {
    expect(formatBytes(412000000)).toBe('393 MB');
  });

  it('有效位数：≥100 取整、10-99 一位、<10 两位', () => {
    expect(formatBytes(100 * 1024)).toBe('100 KB');
    expect(formatBytes(10 * 1024)).toBe('10.0 KB');
    expect(formatBytes(1024)).toBe('1.00 KB');
  });

  it('非法输入：null/undefined/负数/NaN/Infinity → —', () => {
    expect(formatBytes(null)).toBe('—');
    expect(formatBytes(undefined)).toBe('—');
    expect(formatBytes(-1)).toBe('—');
    expect(formatBytes(Number.NaN)).toBe('—');
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('—');
  });
});

describe('formatBytes 1000 进制（容器镜像列，与 docker CLI 对齐）', () => {
  it.each([
    [999, '999 B'],
    [1000, '1.00 KB'],
    [412000000, '412 MB'],
    [67.5 * 1e6, '67.5 MB'],
    [1.2 * 1e9, '1.20 GB'],
    [2.3 * 1e12, '2.30 TB'],
  ])('%d → %s', (bytes, text) => {
    expect(formatBytes(bytes, 1000)).toBe(text);
  });

  it('走查 T6 修复：412000000B 渲染 412 MB（docker CLI 同数字）', () => {
    expect(formatBytes(412000000, 1000)).toBe('412 MB');
  });
});
