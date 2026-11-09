import { lazy, Suspense, type ReactNode } from 'react';
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router-dom';

import RequireAuth from './features/auth/RequireAuth';
import PageTitleRoute from './components/PageTitle';
import RouteErrorBoundary from './components/RouteErrorBoundary';
import Spinner from './components/Spinner';

// CONTRACT.md 8.1：createBrowserRouter + 路由级 React.lazy。
// AppShell（Layout/Menu/Drawer 等壳层组件）与登录页互斥使用，
// 同样拆为动态 import：入口只需 router + 认证门 + 各懒加载路由，
// 壳层 antd 组件与图标不进首屏链，登录首屏不再下载整套导航壳。
// RouteFallback/RequireAuth 的加载态用纯 CSS Spinner（不引 antd），
// 否则 antd Spin 会把 icons-vendor 拉回首屏 chunk，分包目标落空。
//
// AntdAppRoot（ConfigProvider 壳 + antd/locale/zh_CN + toast 主题桥）
// 同样下沉到懒加载边界（第 3 轮打磨，README 8.7）：它静态引入的
// antd ConfigProvider/App 会连带 config-provider/skeleton/Button/
// useClosable/useZIndex 等壳 chunk（合计 gzip ~75KB）与 icons-vendor，
// 让首屏 JS gzip 从 ~116.5 KiB 涨到 224 KiB、超 150KB 预算。改懒加载
// 后首屏入口只剩 index + react-vendor + rolldown-runtime。
// 挂起期间的 fallback 仍是纯 CSS Spinner；页面各自的 ConfigProvider
// （AppShell/LoginPage/NotFoundPage）与本层同主题，行为不变。
const AntdAppRoot = lazy(() => import('./components/AntdAppRoot'));
const AppShell = lazy(() => import('./components/AppShell'));
const LoginPage = lazy(() => import('./features/auth/LoginPage'));
const NotFoundPage = lazy(() => import('./features/NotFoundPage'));
const ChatPage = lazy(() => import('./features/chat/ChatPage'));
const ServersPage = lazy(() => import('./features/servers/ServersPage'));
const DatabasesPage = lazy(() => import('./features/databases/DatabasesPage'));
const ProvidersPage = lazy(() => import('./features/providers/ProvidersPage'));
const AuditPage = lazy(() => import('./features/audit/AuditPage'));
// F1 二期（CONTRACT.md 8.1 路由表）：终端 / 回放 / RDP 桌面三个懒加载路由。
const TermPage = lazy(() => import('./features/term/TermPage'));
const RecordingsPage = lazy(() => import('./features/recordings/RecordingsPage'));
const RdpPage = lazy(() => import('./features/rdp/RdpPage'));
// C2 四期（CONTRACT.md 8.1 路由表）：容器管理页。
const ContainersPage = lazy(() => import('./features/containers/ContainersPage'));
// M4 五期（CONTRACT.md 8.1 路由表）：监控 / 告警 / 巡检 / 批量四页。
const MonitorPage = lazy(() => import('./features/monitor/MonitorPage'));
const AlertsPage = lazy(() => import('./features/alerts/AlertsPage'));
const InspectionsPage = lazy(() => import('./features/inspections/InspectionsPage'));
const BatchPage = lazy(() => import('./features/batch/BatchPage'));
// 六期 FE-P1（CONTRACT.md 8.1 路由表）：/settings 全局审批门限。
const SettingsPage = lazy(() => import('./features/settings/SettingsPage'));

// AntdAppRoot 是必然挂载的根壳：模块求值即预取，router 首帧渲染 lazy 时
// 命中已在途的同一 import()（Vite 模块缓存），把「多一跳串行」压缩成
// 与 index 解析并行的下载（RequireAuth 对 AppShell/ChatPage 同此手法）。
void import('./components/AntdAppRoot');

function RouteFallback() {
  return (
    <div className="app-center">
      <Spinner />
    </div>
  );
}

function lazyPage(element: ReactNode): ReactNode {
  return <Suspense fallback={<RouteFallback />}>{element}</Suspense>;
}

const router = createBrowserRouter([
  // PageTitleRoute 是无 path 的 layout 路由：title 随路由更新（成品标准
  // 「页面标题与 favicon」），登录页与 404 一并覆盖，见 components/PageTitle.tsx。
  {
    element: <PageTitleRoute />,
    children: [
      {
        path: '/login',
        element: lazyPage(<LoginPage />),
      },
      {
        element: <RequireAuth />,
        children: [
          {
            element: lazyPage(<AppShell />),
            children: [
              { index: true, element: <Navigate to="/chat" replace /> },
              { path: '/chat', element: lazyPage(<ChatPage />) },
              { path: '/c/:conversationId', element: lazyPage(<ChatPage />) },
              { path: '/servers', element: lazyPage(<ServersPage />) },
              { path: '/databases', element: lazyPage(<DatabasesPage />) },
              { path: '/providers', element: lazyPage(<ProvidersPage />) },
              { path: '/audit', element: lazyPage(<AuditPage />) },
              // F1 二期：/term 终端、/recordings 回放、/rdp 桌面（受保护）。
              { path: '/term', element: lazyPage(<TermPage />) },
              { path: '/recordings', element: lazyPage(<RecordingsPage />) },
              { path: '/rdp', element: lazyPage(<RdpPage />) },
              // C2 四期：/containers 容器管理（受保护）。
              { path: '/containers', element: lazyPage(<ContainersPage />) },
              // M4 五期：/monitor /alerts /inspections /batch（受保护）。
              { path: '/monitor', element: lazyPage(<MonitorPage />) },
              { path: '/alerts', element: lazyPage(<AlertsPage />) },
              { path: '/inspections', element: lazyPage(<InspectionsPage />) },
              { path: '/batch', element: lazyPage(<BatchPage />) },
              // 六期 FE-P1：/settings 全局审批门限（受保护）。
              { path: '/settings', element: lazyPage(<SettingsPage />) },
            ],
          },
        ],
      },
      // 未知路径展示 404 页而非静默重定向（CONTRACT.md 8.1）：输错地址的
      // 用户需要感知，并提供「返回对话」出口。
      { path: '*', element: lazyPage(<NotFoundPage />) },
    ],
  },
]);

export default function App() {
  // AntdAppRoot 让全站 message toast 走主题化实例（契约 8.3：主题固定）；
  // 挂起期间整树停在纯 CSS Spinner（AntdAppRoot chunk gzip ~45KB，
  // 且已随模块求值并行预取，见上方 void import 注释）。
  // RouteErrorBoundary 兜底 React.lazy 的 chunk 加载失败等渲染期错误，
  // 否则整树卸载白屏且 lazy 缓存的 rejected promise 让每次导航重抛。
  return (
    <Suspense fallback={<RouteFallback />}>
      <AntdAppRoot>
        <RouteErrorBoundary>
          <RouterProvider router={router} />
        </RouteErrorBoundary>
      </AntdAppRoot>
    </Suspense>
  );
}
