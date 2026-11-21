// 告警事件分页合并顺序回归（走查第 2 轮 medium）。
import { describe, expect, it } from 'vitest';

import { mergeEventPageHistory } from '../eventPages';

interface Row {
  id: number;
  fired_at: string;
}

// 后端返回：fired_at DESC, id DESC。
const PAGE_NEWEST: Row[] = [
  { id: 100, fired_at: '2026-11-19T10:40:00Z' },
  { id: 99, fired_at: '2026-11-19T09:40:00Z' },
];
const PAGE_OLDER: Row[] = [
  { id: 98, fired_at: '2026-11-19T08:40:00Z' },
  { id: 97, fired_at: '2026-11-19T07:40:00Z' },
];
const PAGE_OLDEST: Row[] = [
  { id: 96, fired_at: '2026-11-19T06:40:00Z' },
  { id: 95, fired_at: '2026-11-19T05:40:00Z' },
];

function assertDescending(rows: readonly Row[]): void {
  for (let index = 1; index < rows.length; index += 1) {
    const previous = rows[index - 1] as Row;
    const current = rows[index] as Row;
    const previousTime = Date.parse(previous.fired_at);
    const currentTime = Date.parse(current.fired_at);
    expect(previousTime).toBeGreaterThanOrEqual(currentTime);
    if (previousTime === currentTime) {
      expect(previous.id).toBeGreaterThanOrEqual(current.id);
    }
  }
}

describe('mergeEventPageHistory', () => {
  it('加载更早事件后保持全局降序（旧页排在原页之后，不双重反转）', () => {
    const merged = mergeEventPageHistory([PAGE_NEWEST, PAGE_OLDER], PAGE_OLDEST);
    expect(merged.map((row) => row.id)).toEqual([100, 99, 98, 97, 96, 95]);
    assertDescending(merged);
  });

  it('没有更早页时原样返回当前页；返回较新事件后顺序仍正确', () => {
    expect(mergeEventPageHistory([], PAGE_NEWEST).map((row) => row.id)).toEqual([100, 99]);
    const backOnePage = mergeEventPageHistory([PAGE_NEWEST], PAGE_OLDER);
    expect(backOnePage.map((row) => row.id)).toEqual([100, 99, 98, 97]);
    assertDescending(backOnePage);
  });

  it('不修改入参页数组', () => {
    const pages = [PAGE_NEWEST, PAGE_OLDER];
    mergeEventPageHistory(pages, PAGE_OLDEST);
    expect(pages).toEqual([PAGE_NEWEST, PAGE_OLDER]);
  });
});
