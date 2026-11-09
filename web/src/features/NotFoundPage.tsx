import { FileSearchOutlined } from '@ant-design/icons';
import { Button, ConfigProvider, Result } from 'antd';
import { useNavigate } from 'react-router-dom';

import { themeConfig } from '../styles/theme';
import { useTheme } from '../styles/themePreference';

// 顶层兜底路由（CONTRACT.md 8.1）：未知路径明确告知 404，而不是静默
// 重定向到 /chat——输错地址的用户需要立刻感知。公开路由（与 /login
// 平级），未登录也能看到 404；「返回对话」进入受保护路由，未登录时由
// RequireAuth 跳登录并回跳。与 LoginPage 一致，独立于 AppShell 的页面
// 需自包 ConfigProvider，antd Result 才能吃到暗色主题。
export default function NotFoundPage() {
  const navigate = useNavigate();
  // 契约 8.3：404 同属公开路由，主题与全站同源。
  const { resolved: theme } = useTheme();
  return (
    <ConfigProvider theme={themeConfig(theme)}>
      <div className="app-center">
        <Result
          // 自定义 icon 覆盖 antd v6 404 内置插画：内置插画自带英文
          // 「No Found」（拼写还非规范英文 Not Found），与全中文 UI 不协调；
          // 这里换成主题色图标，title/subTitle 仍由页面自己提供中文文案。
          icon={<FileSearchOutlined style={{ fontSize: 72, color: 'var(--color-accent)' }} />}
          title="404"
          subTitle="页面不存在或已被移动，请检查地址是否输入正确。"
          extra={
            <Button type="primary" onClick={() => navigate('/chat')}>
              返回对话
            </Button>
          }
        />
      </div>
    </ConfigProvider>
  );
}
