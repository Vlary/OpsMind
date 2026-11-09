// 左侧导航的单一来源（CONTRACT.md 8.3.10）：`NAV_ITEMS` 具名导出，供
// AppShell 渲染菜单，也供 `lib/pageTitleNavAlign.test.ts` 断言「导航 label /
// 页面标题 / 文档标题」三层一致——此前 nav 数组是 AppShell 模块内 const，
// 外部不可见，任何单测都无法与 pageTitle.ts 的映射比对（六期 /term
// /recordings /rdp /containers 四条漏映射正是因此漏网）。
//
// 图标用 createElement 而非 JSX：契约把文件路径写死为 `navItems.ts`（不是
// .tsx），这样 node 环境的单测也能直接 import 本模块而不需要额外 loader。

import {
  AlertOutlined,
  AppstoreOutlined,
  CloudServerOutlined,
  CodeOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DesktopOutlined,
  FileSearchOutlined,
  MessageOutlined,
  RobotOutlined,
  ScheduleOutlined,
  SettingOutlined,
  ThunderboltOutlined,
  VideoCameraOutlined,
} from '@ant-design/icons';
import { createElement, type ReactNode } from 'react';

export interface NavItem {
  /** 路由前缀，同时是 Menu 的 item key。 */
  key: string;
  icon: ReactNode;
  /** 导航文案：必须与 PageHeader 标题、document.title 逐字相同。 */
  label: string;
}

export const NAV_ITEMS: readonly NavItem[] = [
  { key: '/chat', icon: createElement(MessageOutlined), label: '对话' },
  { key: '/servers', icon: createElement(CloudServerOutlined), label: '服务器' },
  { key: '/databases', icon: createElement(DatabaseOutlined), label: '数据库' },
  { key: '/providers', icon: createElement(RobotOutlined), label: '模型' },
  { key: '/audit', icon: createElement(FileSearchOutlined), label: '审计' },
  // F1 二期：终端 / 回放 / RDP 桌面导航（CONTRACT.md 8.1）。
  { key: '/term', icon: createElement(CodeOutlined), label: '终端' },
  { key: '/recordings', icon: createElement(VideoCameraOutlined), label: '回放' },
  { key: '/rdp', icon: createElement(DesktopOutlined), label: '桌面' },
  // C2 四期：容器管理导航（CONTRACT.md 8.2 AppShell 菜单含「容器」）。
  { key: '/containers', icon: createElement(AppstoreOutlined), label: '容器' },
  // M4 五期导航（CONTRACT.md 8.2 AppShell 扩展：监控/告警/巡检/批量）。
  { key: '/monitor', icon: createElement(DashboardOutlined), label: '监控' },
  { key: '/alerts', icon: createElement(AlertOutlined), label: '告警' },
  { key: '/inspections', icon: createElement(ScheduleOutlined), label: '巡检' },
  { key: '/batch', icon: createElement(ThunderboltOutlined), label: '批量' },
  // 六期 FE-P1：设置（全局审批门限，CONTRACT.md 8.1 /settings）。
  { key: '/settings', icon: createElement(SettingOutlined), label: '设置' },
];
