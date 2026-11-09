import { describe, expect, it } from 'vitest';

// 三期 p3 sprint 2 走查 W4 修复锁定：修复前 RdpPage 服务器下拉只按
// os!=='windows' 禁用，网络设备（device_kind=network_device，os 恒
// 'network'）以禁用项混入「远程桌面」选择器——实测 netdev-telnet-w1 /
// netdev-ssh-w2 都出现在下拉里，禁用提示「仅 Windows 服务器支持 RDP」
// 对交换机/路由器语义错位（它们不是桌面系统）。契约 3.2 约定 6「RDP
// 对一切 network_device 返回 400 RDP_UNSUPPORTED」服务端已拦
// （rdpx.Open 非 windows 即 ErrUnsupported，见 internal/rdpx/gateway.go），
// 前端对齐为直接过滤不渲染；Linux 服务器保留禁用项（提示语准确）。
import type { components } from '../../../api/schema';
import { buildRdpServerOptions } from '../RdpPage';

type Server = components['schemas']['Server'];

function serverOf(overrides: Partial<Server> = {}): Server {
  return {
    id: 'srv-1',
    name: 'walk-win',
    device_kind: 'server',
    os: 'windows',
    netdev_brand: 'generic',
    access_proto: 'ssh',
    host: '127.0.0.1',
    port: 22,
    winrm_port: 5985,
    username: 'administrator',
    auth_method: 'password_ntlm',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: '',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('buildRdpServerOptions（网络设备不进 RDP 下拉）', () => {
  it('network_device（ssh 或 telnet）直接过滤不渲染', () => {
    const options = buildRdpServerOptions([
      serverOf({ id: 'win-1', name: 'win-ad' }),
      serverOf({
        id: 'net-1',
        name: 'netdev-ssh-w2',
        device_kind: 'network_device',
        os: 'network',
        netdev_brand: 'huawei',
        access_proto: 'ssh',
        username: 'admin',
        auth_method: 'password',
      }),
      serverOf({
        id: 'net-2',
        name: 'netdev-telnet-w1',
        device_kind: 'network_device',
        os: 'network',
        netdev_brand: 'cisco',
        access_proto: 'telnet',
        username: 'admin',
        auth_method: 'password',
      }),
    ]);
    expect(options.map((option) => option.value)).toEqual(['win-1']);
    expect(options.some((option) => option.label.includes('netdev'))).toBe(false);
  });

  it('Windows 可选、Linux 禁用并提示「仅 Windows 服务器支持 RDP」', () => {
    const options = buildRdpServerOptions([
      serverOf({ id: 'win-1', name: 'win-ad', os: 'windows' }),
      serverOf({ id: 'lin-1', name: 'web-01', os: 'linux', auth_method: 'private_key' }),
    ]);
    expect(options).toEqual([
      { value: 'win-1', label: 'win-ad（127.0.0.1）' },
      {
        value: 'lin-1',
        label: 'web-01（127.0.0.1）',
        disabled: true,
        title: '仅 Windows 服务器支持 RDP',
      },
    ]);
  });

  it('过滤后顺序保持原有资产顺序（win/linux 相对次序不变）', () => {
    const options = buildRdpServerOptions([
      serverOf({ id: 'lin-1', name: 'web-01', os: 'linux' }),
      serverOf({ id: 'net-1', name: 'core-sw', device_kind: 'network_device', os: 'network' }),
      serverOf({ id: 'win-1', name: 'win-ad', os: 'windows' }),
    ]);
    expect(options.map((option) => option.value)).toEqual(['lin-1', 'win-1']);
  });

  it('空列表返回空数组', () => {
    expect(buildRdpServerOptions([])).toEqual([]);
  });
});
