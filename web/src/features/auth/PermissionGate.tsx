import { Outlet, useLocation } from 'react-router-dom';

import Spinner from '../../components/Spinner';
import { QueryErrorState } from '../../components/admin/AdminStates';
import { canAccessPath } from '../../lib/rbac';
import ForbiddenPage from '../ForbiddenPage';
import { useMe } from './useMe';

/**
 * 路由权限门（AppShell 内层 layout）。
 *
 * RequireAuth 只保证「已登录」；本组件按导航项声明的所需权限判定当前
 * 路径能否直达（CONTRACT.md 12.2）。无权限时渲染完整的 403 页，不重定向、
 * 不空白——直接输入 URL 的用户需要明确的解释。
 */
export default function PermissionGate() {
  const location = useLocation();
  const meQuery = useMe();

  if (meQuery.isPending) {
    return (
      <div className="app-center">
        <Spinner />
      </div>
    );
  }

  // 认证失败由 RequireAuth / queryClient 处理；这里只兜底「已登录但拉取
  // 权限失败」，给出可重试的错误提示而不是白屏。
  if (meQuery.isError) {
    return <QueryErrorState error={meQuery.error} onRetry={() => void meQuery.refetch()} />;
  }

  if (!canAccessPath(location.pathname, meQuery.data?.permissions ?? [])) {
    return <ForbiddenPage />;
  }

  return <Outlet />;
}
