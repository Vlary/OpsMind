// C2 四期：容器运行时徽标组件（契约 8.7 容器页图例 / 服务器列表页徽标）。
// ''（未探测）默认不渲染——服务器列表页避免未探测噪音；容器页下拉与
// 选中态图例传 showUnknown 显式展示灰色「未探测」。

import { Tag, Tooltip } from 'antd';

import { runtimeBadgeOf, runtimeBadgeTip } from './runtimeBadge';

export interface RuntimeBadgeProps {
  runtime: string;
  /** 容器页传 true：'' 也显示「未探测」灰徽标；服务器列表页默认隐藏。 */
  showUnknown?: boolean;
}

export default function RuntimeBadge({ runtime, showUnknown = false }: RuntimeBadgeProps) {
  if (runtime === '' && !showUnknown) {
    return null;
  }
  const meta = runtimeBadgeOf(runtime);
  return (
    <Tooltip title={runtimeBadgeTip(runtime)}>
      <Tag color={meta.color}>{meta.label}</Tag>
    </Tooltip>
  );
}
