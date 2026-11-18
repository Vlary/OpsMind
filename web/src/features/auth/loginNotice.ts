// 登录页提示（00012 评审 medium）：认证门因会话过期/角色变更吊销会话时
// 带 reason=session_expired 跳回，登录页需明确说明原因，而不是静默重定向。
// 被停用账号的登录失败仍是 AUTH_INVALID（契约 12.4 第 7 条，禁止账号枚举），
// 这里不区分、也不能区分。
export function sessionExpiredNotice(reason: string | null): string | null {
  if (reason !== 'session_expired') {
    return null;
  }
  return '登录已失效（会话过期或角色/权限已变更），请重新登录。';
}
