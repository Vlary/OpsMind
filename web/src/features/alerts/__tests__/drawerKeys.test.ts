// 告警抽屉 key 唯一性回归（走查第 2 轮 low）。
import { describe, expect, it } from 'vitest';

import { formDrawerKey } from '../drawerKeys';

describe('formDrawerKey', () => {
  it('新建态规则/渠道 key 不重复（此前同为 new）', () => {
    expect(formDrawerKey('rule', null)).toBe('rule-new');
    expect(formDrawerKey('channel', null)).toBe('channel-new');
    expect(formDrawerKey('rule', null)).not.toBe(formDrawerKey('channel', null));
  });

  it('编辑态按类型+ID 唯一，普通 id 不会撞前缀', () => {
    expect(formDrawerKey('rule', 'abc')).toBe('rule-abc');
    expect(formDrawerKey('channel', 'abc')).toBe('channel-abc');
    expect(formDrawerKey('rule', undefined)).toBe('rule-new');
  });
});
