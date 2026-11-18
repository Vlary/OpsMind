import { describe, expect, it } from 'vitest';

import { sessionExpiredNotice } from './loginNotice';

describe('sessionExpiredNotice', () => {
  it('会话过期/角色变更跳回时给出明确原因', () => {
    expect(sessionExpiredNotice('session_expired')).toContain('登录已失效');
  });

  it('无 reason 或未知 reason 不显示提示', () => {
    expect(sessionExpiredNotice(null)).toBeNull();
    expect(sessionExpiredNotice('other')).toBeNull();
  });
});
