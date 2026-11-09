import {
  DesktopOutlined,
  KeyOutlined,
  LogoutOutlined,
  MenuOutlined,
  MoonOutlined,
  SunOutlined,
  UserOutlined,
} from '@ant-design/icons';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Avatar,
  Button,
  ConfigProvider,
  Drawer,
  Dropdown,
  Grid,
  Layout,
  Menu,
  Segmented,
  Space,
  Typography,
} from 'antd';
import { useCallback, useState } from 'react';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';

import { themeConfig } from '../styles/theme';
import { useTheme, type ThemePreference } from '../styles/themePreference';
import { NAV_ITEMS } from './navItems';

import { api } from '../api/client';
import { qk } from '../api/queryKeys';
import { message } from './antdMessage';
import ChangePasswordModal from '../features/auth/ChangePasswordModal';
import { useUIStore } from '../stores/uiStore';

function selectedKey(pathname: string): string {
  // 契约 8.3.10：导航项单一来源 NAV_ITEMS（components/navItems.ts）。
  const hit = NAV_ITEMS.find(
    (item) => pathname === item.key || pathname.startsWith(`${item.key}/`),
  );
  return hit?.key ?? '/chat';
}

function isChatPath(pathname: string): boolean {
  return pathname === '/chat' || pathname.startsWith('/c/');
}

/**
 * 侧栏折叠条高度（antd Layout.Sider 的 `.ant-layout-sider-trigger`）。
 *
 * antd token：`triggerHeight = controlHeightLG(45) + marginXXS(8)*2 = 53`。
 * 该值为 antd 内部派生 token，写死 53 是为了给菜单留出底部内边距——实测
 * 1366×768 下折叠条 top=715、高 53，正好压住滚到底的末菜单项（六期打磨
 * sprint 4 走查）。若升级 antd 改变了 trigger 高度，这里需要同步。
 */
const SIDER_TRIGGER_HEIGHT = 53;

/**
 * 顶栏主题三态切换（CONTRACT.md 8.3.12）。
 *
 * 位置：`flex:1` 占位与用户菜单 `Dropdown` 之间。控件本身不带色值——
 * Segmented 的底/边/文字全部来自 `themeConfig(mode)` 的 token。
 * `aria-label` 供读屏识别整组控件，每项 `title` 给出悬停说明。
 *
 * 窄屏（< md）只保留图标：三态带文字时实测 360/375/390 下顶栏被挤爆
 * ——品牌名折成单字竖排、header `scrollHeight` 216 vs `clientHeight` 47、
 * 375 下用户名按钮右溢 6px（七期走查）。图标态宽度约 96px，配合
 * `compact` 下的品牌文字隐藏，顶栏回到单行。可访问名由 `title` 与
 * `aria-label` 双份保证（图标本身无文字）。
 */
function ThemeSwitcher({ compact = false }: { compact?: boolean }) {
  const { preference, setMode } = useTheme();
  if (compact) {
    return (
      <Segmented<ThemePreference>
        aria-label="主题"
        size="small"
        value={preference}
        onChange={(value) => setMode(value)}
        options={[
          { value: 'light', icon: <SunOutlined aria-label="浅色" />, title: '浅色主题' },
          { value: 'dark', icon: <MoonOutlined aria-label="深色" />, title: '深色主题' },
          {
            value: 'system',
            icon: <DesktopOutlined aria-label="跟随系统" />,
            title: '跟随系统明暗设置',
          },
        ]}
      />
    );
  }
  return (
    <Segmented<ThemePreference>
      aria-label="主题"
      size="small"
      value={preference}
      onChange={(value) => setMode(value)}
      options={[
        { value: 'light', label: '浅色', title: '浅色主题' },
        { value: 'dark', label: '深色', title: '深色主题' },
        { value: 'system', label: '跟随系统', title: '跟随系统明暗设置' },
      ]}
    />
  );
}

export default function AppShell() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const screens = Grid.useBreakpoint();
  const isMobile = screens.md === false;
  const collapsed = useUIStore((state) => state.sidebarCollapsed);
  const setSidebarCollapsed = useUIStore((state) => state.setSidebarCollapsed);
  const mobileNavOpen = useUIStore((state) => state.mobileNavOpen);
  const setMobileNavOpen = useUIStore((state) => state.setMobileNavOpen);
  // 契约 8.3.12：生效主题 → antd ThemeConfig / Menu theme / Drawer 背景同源。
  const { resolved: theme } = useTheme();
  const [loggingOut, setLoggingOut] = useState(false);
  const [passwordModalOpen, setPasswordModalOpen] = useState(false);

  const meQuery = useQuery({
    queryKey: qk.me(),
    queryFn: async () => {
      const { data, error } = await api.GET('/api/auth/me');
      if (error) {
        throw error;
      }
      return data.user;
    },
    retry: false,
  });

  const handleLogout = useCallback(async () => {
    setLoggingOut(true);
    try {
      await api.POST('/api/auth/logout', {
        params: { header: { Origin: window.location.origin } },
      });
    } finally {
      queryClient.clear();
      setLoggingOut(false);
      navigate('/login', { replace: true });
    }
  }, [navigate, queryClient]);

  // 改密成功：服务端已吊销全部会话，清空缓存并回到登录页。
  const handlePasswordChanged = useCallback(() => {
    setPasswordModalOpen(false);
    queryClient.clear();
    message.success('密码已修改，请使用新密码重新登录');
    navigate('/login', { replace: true });
  }, [navigate, queryClient]);

  const menu = (
    <Menu
      // 契约 8.3.8：Menu 的 theme 必须跟随模式，不得写死 "dark"。
      theme={theme}
      mode="inline"
      selectedKeys={[selectedKey(location.pathname)]}
      // 契约 8.3.10：导航项单一来源 NAV_ITEMS（components/navItems.ts）；
      // 这里只做形状映射（antd Menu 的 ItemType 要求）。
      items={NAV_ITEMS.map(({ key, icon, label }) => ({ key, icon, label }))}
      onClick={({ key }) => {
        setMobileNavOpen(false);
        navigate(key);
      }}
    />
  );

  const brand = (
    <div className="app-brand">
      <Typography.Text strong style={{ fontSize: 16 }}>
        {collapsed && !isMobile ? 'OM' : '云枢 OpsMind'}
      </Typography.Text>
    </div>
  );

  return (
    <ConfigProvider theme={themeConfig(theme)}>
      <Layout style={{ height: '100vh', overflow: 'hidden' }}>
        {!isMobile ? (
          <Layout.Sider
            collapsible
            collapsed={collapsed}
            onCollapse={(value) => setSidebarCollapsed(value)}
            width={220}
            style={{ height: '100vh', overflow: 'auto' }}
          >
            {brand}
            {/*
              六期打磨 sprint 4（走查 medium）：折叠条 `.ant-layout-sider-trigger`
              是 `position: fixed; bottom: 0`（高 triggerHeight = controlHeightLG
              45 + marginXXS×2 = 53px，本主题实测 53），而 sider 自身
              `overflow: auto` 的滚动区不把固定定位的折叠条算进底部留白——
              1366×768 下「设置」项被压住 26px（1280×720 下「批量」「设置」
              两项都被吞），点该项文字中部命中的是折叠条，侧栏被折叠而不是
              跳转。菜单外再包一层底部内边距，末两项可滚到折叠条上方。
            */}
            <div style={{ paddingBottom: SIDER_TRIGGER_HEIGHT }}>{menu}</div>
          </Layout.Sider>
        ) : null}

        <Layout style={{ height: '100vh', overflow: 'hidden' }}>
          <Layout.Header
            className="app-header"
            style={{
              height: 48,
              lineHeight: '48px',
              padding: '0 16px',
              display: 'flex',
              alignItems: 'center',
              gap: 12,
            }}
          >
            <Space size={12} style={{ minWidth: 0 }}>
              {isMobile ? (
                <Button
                  type="text"
                  icon={<MenuOutlined />}
                  aria-label="打开导航"
                  onClick={() => setMobileNavOpen(true)}
                />
              ) : (
                <Typography.Text type="secondary" ellipsis>
                  {NAV_ITEMS.find((item) => item.key === selectedKey(location.pathname))?.label}
                </Typography.Text>
              )}
            </Space>

            <div style={{ flex: 1 }} />

            <ThemeSwitcher compact={isMobile} />

            <Dropdown
              placement="bottomRight"
              menu={{
                items: [
                  { key: 'password', icon: <KeyOutlined />, label: '修改密码' },
                  { type: 'divider' },
                  { key: 'logout', icon: <LogoutOutlined />, label: '退出登录' },
                ],
                onClick: ({ key }) => {
                  if (key === 'logout') {
                    void handleLogout();
                  }
                  if (key === 'password') {
                    setPasswordModalOpen(true);
                  }
                },
              }}
            >
              <Button type="text" loading={loggingOut}>
                <Space size={8}>
                  <Avatar size="small" icon={<UserOutlined />} />
                  <span>{meQuery.data?.username ?? 'admin'}</span>
                </Space>
              </Button>
            </Dropdown>

            <ChangePasswordModal
              open={passwordModalOpen}
              onClose={() => setPasswordModalOpen(false)}
              onSuccess={handlePasswordChanged}
            />
          </Layout.Header>

          <Layout.Content
            className={
              isChatPath(location.pathname) ? 'app-content app-content--chat' : 'app-content'
            }
          >
            <Outlet />
          </Layout.Content>
        </Layout>

        <Drawer
          open={isMobile && mobileNavOpen}
          placement="left"
          width={240}
          onClose={() => setMobileNavOpen(false)}
          styles={{ body: { padding: 0, background: 'var(--color-bg-shell)' } }}
        >
          {brand}
          {menu}
        </Drawer>
      </Layout>
    </ConfigProvider>
  );
}
