import { App as AntdApp, ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { useEffect, type ReactNode } from 'react';

import { themeConfig, type ThemeMode } from '../styles/theme';
import { useTheme } from '../styles/themePreference';
import { bindMessage } from './antdMessage';

// 全站 toast 主题桥：antd 静态 message 不吃组件树 ConfigProvider 的主题，
// 根部包一层 ConfigProvider + <App component={false}>（不渲染额外 DOM），
// 并把 App.useApp() 的主题化 message 实例经 bindMessage 提供给
// antdMessage 模块转发。message 的 holder 渲染在本层 ConfigProvider 内，
// 因此**切换主题后新弹出的 toast 自动换色**；已弹出的也会随同重渲染。
//
// 主题来源（CONTRACT.md 8.3.12）：调用方可经 `mode` 传入已解析的
// light/dark；省略时由 `useTheme().resolved` 提供（读 `<html data-theme>`，
// 与 index.html 的首绘内联脚本同源）。`themeConfig(mode)` 是全站唯一
// ThemeConfig 来源——页面各自的 ConfigProvider 必须用同一函数，否则浮层
// 与 toast 会与主体不同主题。未显式传 locale 的内层继承本层 zh_CN，
// 分页器「N 条/页」、Modal 默认按钮等文案全站中文化。
function MessageBridge() {
  const { message } = AntdApp.useApp();
  useEffect(() => {
    bindMessage(message);
  }, [message]);
  return null;
}

export default function AntdAppRoot({
  children,
  mode,
}: {
  children: ReactNode;
  /** 已解析的生效主题；省略时跟随 `<html data-theme>`。 */
  mode?: ThemeMode;
}) {
  const { resolved } = useTheme();
  const activeMode = mode ?? resolved;

  return (
    <ConfigProvider theme={themeConfig(activeMode)} locale={zhCN}>
      <AntdApp component={false}>
        <MessageBridge />
        {children}
      </AntdApp>
    </ConfigProvider>
  );
}
