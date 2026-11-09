// C2 四期：容器页服务器选择的纯逻辑（契约 8.7 容器页 / 2.9 约定 1-2）。
// 抽成纯函数供单测与页面共用（同 term/serverOptions.ts 的先例），不引 antd。

import type { components } from '../../api/schema';

type Server = components['schemas']['Server'];

export interface ContainerServerOption {
  value: string;
  /** 下拉主文案：名称（host:port）。 */
  label: string;
  /** 运行时原值，页面据此在下拉项内渲染徽标。 */
  runtime: string;
  disabled: boolean;
  /** 禁用原因（disabled 时非空，antd Select option 的 title 提示）。 */
  title: string | null;
}

/**
 * 容器页服务器下拉选项（契约 8.7）：仅 device_kind=server 的资产——
 * network_device 的容器接口一律 400 CONTAINER_UNSUPPORTED（2.9），不进
 * 下拉。runtime 为 none/containerd 的服务器禁用并给提示；''（未探测）
 * 可选——选中后自动触发一次容器列表请求，后端同步探测并写回
 * container_runtime（2.9 约定 1），这是 '' 服务器唯一的探测入口。
 */
export function containerServerOptions(servers: Server[]): ContainerServerOption[] {
  return servers
    .filter((server) => server.device_kind === 'server')
    .map((server) => {
      const base = {
        value: server.id,
        label: `${server.name}（${server.host}:${server.port}）`,
        runtime: server.container_runtime,
      };
      if (server.container_runtime === 'none') {
        return {
          ...base,
          disabled: true,
          title: '未探测到受支持的容器运行时（docker/podman），无法管理容器',
        };
      }
      if (server.container_runtime === 'containerd') {
        return {
          ...base,
          disabled: true,
          title: '仅探测到 containerd：调试用途，不支持容器管理面',
        };
      }
      return { ...base, disabled: false, title: null };
    });
}

/**
 * 未手动选择时的默认服务器（七期走查修复）。
 *
 * 只挑「可管理或可探测」的 server 类资产，优先级：docker/podman > ''（未
 * 探测）> 不选。修复前取列表第一台（按 created_at 升序），种子/演示数据的
 * 第一台常是 runtime=none——进页即 `GET /containers` 400
 * CONTAINER_UNSUPPORTED，页面顶部挂一整条红色错误横幅、console 同步报 400。
 *
 * - `none` / `containerd`：跳过（管理面确定不可用，5.2C / 2.9 约定 2）。
 * - `''`（未探测）：可作兜底——选中即触发一次探测写回（2.9 约定 1），
 *   这是未探测服务器唯一的自动探测入口；但只在没有任何已确认运行时时
 *   兜底，避免有 docker 服务器时仍先进错误态。
 * - 全部为 none/containerd（或没有 server 类资产）：返回 null——页面显示
 *   「请先选择支持容器运行时的服务器」空态（仍保留「重新探测」入口），
 *   不再替用户发一次注定 400 的请求。
 */
export function pickDefaultServer(servers: Server[]): string | null {
  const candidates = servers.filter((server) => server.device_kind === 'server');
  const manageable = candidates.find(
    (server) => server.container_runtime === 'docker' || server.container_runtime === 'podman',
  );
  if (manageable) {
    return manageable.id;
  }
  const unprobed = candidates.find((server) => server.container_runtime === '');
  return unprobed?.id ?? null;
}
