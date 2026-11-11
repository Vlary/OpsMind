// 成品标准「页面标题与 favicon」：title 随路由区分（「服务器 · 云枢 OpsMind」），
// 多标签页可辨识。路由匹配是纯函数（组件在 components/PageTitle.tsx），
// node 环境可直测（见 components/pageTitle.test.ts）。

// 路由前缀 → 页面名（与 AppShell navItems 的 label 保持一致）。
const PAGE_TITLES: ReadonlyArray<readonly [prefix: string, title: string]> = [
  ['/login', '登录'],
  ['/chat', '对话'],
  ['/servers', '服务器'],
  ['/databases', '数据库'],
  ['/providers', '模型'],
  ['/audit', '审计'],
  // 二至四期（与 AppShell navItems 的 label 保持一致；此前漏映射，
  // 这四个页面标签页曾错误显示「页面不存在」）。
  ['/term', '终端'],
  // 八期 FE-R（契约 2.12 约定 1 / 8.3.10）：/recordings 更名「录制」，
  // 新增 /replays「回放执行」；NAV_ITEMS 与 RecordingsPage 页头同一次提交改。
  ['/recordings', '录制'],
  ['/rdp', '桌面'],
  ['/containers', '容器'],
  // M4 五期（与 AppShell navItems 的 label 保持一致）。
  ['/monitor', '监控'],
  ['/alerts', '告警'],
  ['/inspections', '巡检'],
  ['/batch', '批量'],
  // 八期 FE-R：独立回放页（AI 会话执行回放）。
  ['/replays', '回放执行'],
  // 六期 FE-P1：设置页（全局审批门限）。
  ['/settings', '设置'],
];

const SITE_NAME = '云枢 OpsMind';

// pathname → 页面名。匹配不到的一律是 NotFoundPage，返回「页面不存在」。
export function pageTitleFor(pathname: string): string {
  // 根路径是 index 路由，立即重定向到 /chat（App.tsx），title 对齐目标；
  // /c/:conversationId 与 /chat 同页（会话详情），前缀以斜杠结尾需先判。
  if (pathname === '/' || pathname === '/chat' || pathname.startsWith('/c/')) {
    return '对话';
  }
  const hit = PAGE_TITLES.find(
    ([prefix]) => pathname === prefix || pathname.startsWith(`${prefix}/`),
  );
  return hit ? hit[1] : '页面不存在';
}

export function pageTitle(pathname: string): string {
  return `${pageTitleFor(pathname)} · ${SITE_NAME}`;
}
