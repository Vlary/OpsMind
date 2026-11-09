import { useEffect } from 'react';
import { Outlet, useLocation } from 'react-router-dom';

import { pageTitle } from '../lib/pageTitle';

// 成品标准「页面标题与 favicon」：title 随路由更新的无 path layout 路由。
// 挂载方式见 App.tsx——包住全部路由（登录页与 404 同样覆盖），各页面
// 无需各自 useEffect。路由 → 页面名的映射在 lib/pageTitle.ts。
export default function PageTitleRoute() {
  const location = useLocation();
  useEffect(() => {
    document.title = pageTitle(location.pathname);
  }, [location.pathname]);
  return <Outlet />;
}
