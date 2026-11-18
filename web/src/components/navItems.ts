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
  CheckCircleOutlined,
  CloudServerOutlined,
  CodeOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DesktopOutlined,
  FileSearchOutlined,
  HistoryOutlined,
  MessageOutlined,
  RobotOutlined,
  ScheduleOutlined,
  SettingOutlined,
  ThunderboltOutlined,
  UserOutlined,
  VideoCameraOutlined,
} from '@ant-design/icons';
import { createElement, type ReactNode } from 'react';

import { PERMS, type Permission } from '../lib/rbacPermissions';

export interface NavItem {
  /** 路由前缀，同时是 Menu 的 item key。 */
  key: string;
  icon: ReactNode;
  /** 导航文案：必须与 PageHeader 标题、document.title 逐字相同。 */
  label: string;
  /**
   * 可见/可访问所需权限（任一命中即可，CONTRACT.md 12.2 权限矩阵）。
   * 省略表示所有已登录用户可见；服务端仍是唯一授权点，这里只收敛入口。
   */
  permissions?: readonly Permission[];
}

export const NAV_ITEMS: readonly NavItem[] = [
  { key: '/chat', icon: createElement(MessageOutlined), label: '对话', permissions: [PERMS.chat] },
  // 00012：审批队列（审批者登录后的默认落地页；此前无任何审批入口）。
  {
    key: '/approvals',
    icon: createElement(CheckCircleOutlined),
    label: '审批',
    permissions: [PERMS.approvalsRead],
  },
  {
    key: '/servers',
    icon: createElement(CloudServerOutlined),
    label: '服务器',
    permissions: [PERMS.assetsRead],
  },
  {
    key: '/databases',
    icon: createElement(DatabaseOutlined),
    label: '数据库',
    permissions: [PERMS.assetsRead],
  },
  {
    key: '/providers',
    icon: createElement(RobotOutlined),
    label: '模型',
    permissions: [PERMS.providers],
  },
  {
    key: '/audit',
    icon: createElement(FileSearchOutlined),
    label: '审计',
    permissions: [PERMS.auditRead],
  },
  // F1 二期：终端 / 录制 / RDP 桌面导航（CONTRACT.md 8.1）。
  // 八期 FE-R：/recordings 文案「回放」→「录制」（2.12 约定 1）——它是终端/
  // RDP 会话录像；AI 会话的事件级回放在 /replays（「回放执行」）。
  {
    key: '/term',
    icon: createElement(CodeOutlined),
    label: '终端',
    permissions: [PERMS.terminal],
  },
  {
    key: '/recordings',
    icon: createElement(VideoCameraOutlined),
    label: '录制',
    // 本人录制需 terminal；审计员跨用户看录制需 audit:read（12.3 第 4 条）。
    permissions: [PERMS.terminal, PERMS.auditRead],
  },
  {
    key: '/rdp',
    icon: createElement(DesktopOutlined),
    label: '桌面',
    permissions: [PERMS.terminal],
  },
  // C2 四期：容器管理导航（CONTRACT.md 8.2 AppShell 菜单含「容器」）。
  {
    key: '/containers',
    icon: createElement(AppstoreOutlined),
    label: '容器',
    permissions: [PERMS.execRead],
  },
  // M4 五期导航（CONTRACT.md 8.2 AppShell 扩展：监控/告警/巡检/批量）。
  {
    key: '/monitor',
    icon: createElement(DashboardOutlined),
    label: '监控',
    permissions: [PERMS.metricsRead],
  },
  {
    key: '/alerts',
    icon: createElement(AlertOutlined),
    label: '告警',
    // 只读看事件用指标只读权限；配置规则/渠道需独立的配置权限。
    permissions: [PERMS.metricsRead, PERMS.alertsWrite],
  },
  {
    key: '/inspections',
    icon: createElement(ScheduleOutlined),
    label: '巡检',
    permissions: [PERMS.inspections],
  },
  {
    key: '/batch',
    icon: createElement(ThunderboltOutlined),
    label: '批量',
    permissions: [PERMS.execWrite],
  },
  // 八期 FE-R：独立回放页（AI 会话的事件级执行回放，契约 2.12 约定 1）。
  // 本人回放需发起对话权限；审计员跨用户回放需 audit:read（12.3 第 3 条）。
  {
    key: '/replays',
    icon: createElement(HistoryOutlined),
    label: '回放执行',
    permissions: [PERMS.chat, PERMS.auditRead],
  },
  // 六期 FE-P1：设置（全局审批门限，CONTRACT.md 8.1 /settings）。
  // 放宽门限需 policy:request 权限，矩阵中仅 admin 持有（12.2）。
  {
    key: '/settings',
    icon: createElement(SettingOutlined),
    label: '设置',
    permissions: [PERMS.policyWrite],
  },
  // 00012：用户管理（仅管理员；查看用户权限 users:read 矩阵中仅 admin）。
  {
    key: '/users',
    icon: createElement(UserOutlined),
    label: '用户',
    permissions: [PERMS.usersRead],
  },
];
