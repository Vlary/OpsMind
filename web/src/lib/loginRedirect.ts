// 会话失效统一跳登录页的 URL 构造（RBAC 走查 sprint 1）。
//
// 两条 401 路径必须同源：RequireAuth（/api/auth/me 401）与 queryClient 的
// 全局 QueryCache/MutationCache 兜底（在途业务请求 401）。此前只有前者带
// `reason=session_expired`，后者把用户丢到普通登录页、不解释会话已失效。
// 抽成纯函数既保证口径一致，也让 node 环境单测直接断言。

export const SESSION_EXPIRED_REASON = 'session_expired';

/** 登录页回跳地址：保留原路径（登录成功后回跳）并携带失效原因。 */
export function loginRedirectPath(pathname: string, search = ''): string {
  const next = `${pathname}${search}`;
  return `/login?next=${encodeURIComponent(next)}&reason=${SESSION_EXPIRED_REASON}`;
}
