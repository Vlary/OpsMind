// 服务器选项的通道判定与展示文案（纯函数，三期 p3 sprint 3 从 TermPage
// 抽出共享）：ChatPage 的「服务器上下文」下拉与终端页共用同一口径，
// 避免终端页「name（host:port · 品牌 · 协议）」而对话页只有裸名称的
// 不一致（同名网络设备与服务器无法区分，实测走查 sprint 3）。独立成
// 模块（而非互相 import 页面）是为不破坏路由级 React.lazy 分割——
// ChatPage 若直接 import TermPage 会把 xterm 终端链拉进对话 chunk。
import type { components } from '../../api/schema';
import { netdevBrandLabel } from '../servers/ServerFormDrawer';

type Server = components['schemas']['Server'];

/**
 * 终端通道判定（契约 2.7/2.8 定稿路由）：SSH 通道（server 类 linux 全部、
 * windows 的 password/private_key；网络设备 access_proto=ssh）可开终端；
 * WinRM 认证（password_ntlm/password_basic）无 PTY 语义、Telnet 接入的
 * 网络设备 v1 无终端桥，服务端均 400 TERM_UNSUPPORTED，前端直接禁用。
 */
export function termChannelOf(server: Server): {
  ssh: boolean;
  windows: boolean;
  netdev: boolean;
} {
  const netdev = server.device_kind === 'network_device';
  const ssh = netdev
    ? server.access_proto === 'ssh'
    : server.auth_method === 'password' || server.auth_method === 'private_key';
  const windows = !netdev && server.os === 'windows';
  return { ssh, windows, netdev };
}

/**
 * 服务器下拉的展示文案（走查 C2 修复）：端口跟随真实通道——SSH 通道
 * 显示 server.port；WinRM 认证机器的 server.port 是与其通道无关的
 * SSH 端口（实测显示 127.0.0.1:22 误导用户），改显 winrm_port
 *（缺省 5985）。三期（N3）：网络设备追加品牌与接入协议标识。
 */
export function termServerOptionLabel(server: Server): string {
  if (server.device_kind === 'network_device') {
    return `${server.name}（${server.host}:${server.port} · ${netdevBrandLabel(server.netdev_brand)} · ${server.access_proto.toUpperCase()}）`;
  }
  const channel = termChannelOf(server);
  const port = channel.ssh ? server.port : server.winrm_port || 5985;
  return `${server.name}（${server.host}:${port}）`;
}

/**
 * 终端卡片是否需要常驻「Telnet 网络设备不可开终端」说明（p3 sprint 5）：
 * Telnet 接入的网络设备在下拉中是 disabled 项，点击零反馈，「为什么不能
 * 开终端」此前只能靠 hover 原生 title——且专为该场景写的选中态警告 Alert
 * 因 disabled 不可选而不可达（死代码）。存在 Telnet 接入的网络设备时返回
 * 常驻 info 提示文案，否则 null（普通部署不打扰）。
 */
export function telnetNetdevNotice(servers: Server[] | undefined | null): string | null {
  if (!servers) {
    return null;
  }
  return servers.some(
    (server) => server.device_kind === 'network_device' && server.access_proto === 'telnet',
  )
    ? '网络设备经 SSH 通道提供交互式 CLI 终端（提示符驱动的行协议），全程录制；Telnet 接入的网络设备暂不支持终端（v1 无 telnet 终端桥），命令执行请走对话或命令预览。'
    : null;
}
