import { describe, expect, it } from 'vitest';

import { loginRedirectPath } from './loginRedirect';

// RBAC 走查 sprint 1（low）：会话被吊销后，queryClient 的全局 401 兜底与
// RequireAuth 必须给出同一个登录页地址（带 reason=session_expired），
// 否则用户只看到普通登录页、不知道会话已失效。
describe('loginRedirectPath 会话失效回跳', () => {
  it('携带原路径与 reason=session_expired', () => {
    expect(loginRedirectPath('/chat')).toBe('/login?next=%2Fchat&reason=session_expired');
  });

  it('保留查询串（next 编码为一个参数）', () => {
    expect(loginRedirectPath('/alerts', '?diagnosis=abc')).toBe(
      '/login?next=%2Falerts%3Fdiagnosis%3Dabc&reason=session_expired',
    );
  });

  it('根路径与空 search 也生成合法 next', () => {
    expect(loginRedirectPath('/', '')).toBe('/login?next=%2F&reason=session_expired');
  });

  it('reason 与登录页读取的常量一致', () => {
    expect(loginRedirectPath('/replays')).toContain('reason=session_expired');
  });
});
