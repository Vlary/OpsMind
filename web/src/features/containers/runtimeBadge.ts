// C2 四期：容器运行时徽标与容器状态徽标的纯展示元数据（契约 8.7 容器页
// 图例 / 2.9 约定 2）。独立成纯函数模块供页面、下拉与单测共用，不引入
// antd 依赖——色值是 antd Tag preset 名，由 RuntimeBadge 组件装配。

/** 契约 3.1：''|none|docker|podman|containerd；''=未探测，network_device 恒 ''。 */
export type ContainerRuntime = '' | 'none' | 'docker' | 'podman' | 'containerd';

export interface RuntimeBadgeMeta {
  label: string;
  /** antd Tag preset 色名；docker/podman/containerd 用品牌近似色。 */
  color: string;
}

const RUNTIME_BADGES: Record<ContainerRuntime, RuntimeBadgeMeta> = {
  '': { label: '未探测', color: 'default' },
  none: { label: '无容器', color: 'default' },
  docker: { label: 'Docker', color: 'blue' },
  podman: { label: 'Podman', color: 'geekblue' },
  containerd: { label: 'containerd', color: 'cyan' },
};

/** 运行时徽标元数据；未知值（防御）回落 '' 的「未探测」。 */
export function runtimeBadgeOf(runtime: string): RuntimeBadgeMeta {
  return RUNTIME_BADGES[runtime as ContainerRuntime] ?? RUNTIME_BADGES[''];
}

/**
 * 契约 2.9 约定 2：容器管理面仅 docker/podman；''/none/containerd 的
 * 一切容器接口一律 400 CONTAINER_UNSUPPORTED（containerd 只探测展示）。
 */
export function isManagedRuntime(runtime: string): boolean {
  return runtime === 'docker' || runtime === 'podman';
}

/** 运行时徽标的悬浮说明（图例口径，容器页与服务器列表页共用）。 */
export function runtimeBadgeTip(runtime: string): string {
  switch (runtime) {
    case '':
      return '尚未探测容器运行时；打开容器页会自动探测一次';
    case 'none':
      return '已探测：该服务器没有可管理的容器运行时（docker/podman）';
    case 'containerd':
      return '仅探测到 containerd：调试用途，不支持容器管理面';
    default:
      return `容器运行时：${runtime}`;
  }
}

/**
 * 容器 state 徽标色（契约 8.7）：running 绿、exited 灰、paused 黄、
 * 其他（created/restarting/dead/…运行时原值）默认色。label 用运行时原值。
 */
export function containerStateColor(state: string): string {
  switch (state) {
    case 'running':
      return 'green';
    case 'exited':
      return 'default';
    case 'paused':
      return 'gold';
    default:
      return 'default';
  }
}
