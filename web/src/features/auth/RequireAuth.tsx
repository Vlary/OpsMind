import { useEffect } from 'react';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';

import Spinner from '../../components/Spinner';
import { loginRedirectPath } from '../../lib/loginRedirect';
import { describeError, isUnauthorizedError, toApiErrorInfo } from '../chat/useChatStream';
import { useMe } from './useMe';

export default function RequireAuth() {
  const navigate = useNavigate();
  const location = useLocation();

  // qk.me() 缓存完整 MeResponse（user + permissions，00012 RBAC）；
  // 认证门只关心 user/错误码，权限由内层 PermissionGate 消费。
  const meQuery = useMe();

  const unauthorized = isUnauthorizedError(meQuery.error);

  useEffect(() => {
    // 首跳串行瀑布消除：受保护页首跳原本是 /api/auth/me 往返 → AppShell
    // chunk → ChatPage chunk 三段串行；me 期间并行预取壳层与默认首页
    // chunk，认证通过后 lazy 直接命中已在途的请求（同一 import() 缓存）。
    void import('../../components/AppShell');
    void import('../chat/ChatPage');
  }, []);

  useEffect(() => {
    if (!unauthorized) {
      return;
    }
    navigate(loginRedirectPath(location.pathname, location.search), {
      replace: true,
    });
  }, [location.pathname, location.search, navigate, unauthorized]);

  if (meQuery.isLoading) {
    return (
      <div className="app-center">
        <Spinner />
      </div>
    );
  }

  if (unauthorized) {
    return null;
  }

  if (meQuery.isError) {
    const info = toApiErrorInfo(meQuery.error);
    // 认证门在首屏链上：错误态用原生元素展示（antd Alert/图标会把
    // icons-vendor 拉回登录首屏 chunk），保留重试入口。
    return (
      <div className="app-center">
        <div style={{ textAlign: 'center' }}>
          <p style={{ marginBottom: 12 }}>
            {describeError(info.code, info.message)}（{info.code}）
          </p>
          <button
            type="button"
            onClick={() => void meQuery.refetch()}
            style={{ padding: '4px 16px', cursor: 'pointer' }}
          >
            重试
          </button>
        </div>
      </div>
    );
  }

  return <Outlet />;
}
