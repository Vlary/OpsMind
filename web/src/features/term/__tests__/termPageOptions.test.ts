import { describe, expect, it } from 'vitest';

// 二期冲刺 1 走查修复的纯函数锁定：
// - 走查 C2：WinRM 认证服务器的终端下拉不再显示无关的 SSH 端口。
// - 走查 A3：断开后立刻「新建会话」409 的文案解释收尾时序。
// 三期（N3）：网络设备（SSH 可选 / Telnet 禁用）的下拉文案与通道判定。
import type { components } from '../../../api/schema';
import { makeFailure } from '../../../components/admin/apiError';
import { describeTermCreateFailure } from '../TermPage';
// 三期 p3 sprint 3：termChannelOf/termServerOptionLabel 抽至 serverOptions
// 共享模块（ChatPage 复用），单测同步改导入路径。
import { termChannelOf, termServerOptionLabel, telnetNetdevNotice } from '../serverOptions';

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
    // 四期字段：容器运行时探测结果（空串=未探测，server 类 fixture 取空串）。
    container_runtime: '',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

function netdevOf(overrides: Partial<Server> = {}): Server {
  return serverOf({
    id: 'net-1',
    name: 'core-sw-1',
    device_kind: 'network_device',
    os: 'network',
    netdev_brand: 'huawei',
    access_proto: 'ssh',
    port: 22,
    username: 'admin',
    auth_method: 'password',
    ...overrides,
  });
}

describe('termServerOptionLabel（走查 C2）', () => {
  it('SSH 通道服务器显示 SSH 端口', () => {
    expect(termServerOptionLabel(serverOf({ auth_method: 'password', name: 'lin' }))).toBe(
      'lin（127.0.0.1:22）',
    );
  });

  it('Windows+SSH 通道同样显示 SSH 端口', () => {
    expect(termServerOptionLabel(serverOf({ auth_method: 'private_key' }))).toBe(
      'walk-win（127.0.0.1:22）',
    );
  });

  it('WinRM 认证服务器显示 winrm_port 而非无关的 SSH 端口', () => {
    // 实测缺陷形态：password_ntlm 机器显示「walk-win（127.0.0.1:22）」。
    expect(termServerOptionLabel(serverOf({}))).toBe('walk-win（127.0.0.1:5985）');
  });

  it('WinRM 认证且 winrm_port 缺省为 0 时回落 5985', () => {
    expect(termServerOptionLabel(serverOf({ winrm_port: 0 }))).toBe('walk-win（127.0.0.1:5985）');
  });

  it('password_basic 同样按 WinRM 通道展示', () => {
    expect(
      termServerOptionLabel(serverOf({ auth_method: 'password_basic', winrm_port: 5986 })),
    ).toBe('walk-win（127.0.0.1:5986）');
  });
});

describe('describeTermCreateFailure（走查 A3）', () => {
  it('TERM_SESSION_EXISTS 解释收尾时序并引导稍候重试', () => {
    const text = describeTermCreateFailure(
      makeFailure('TERM_SESSION_EXISTS', 'server already has an active session'),
    );
    expect(text).toContain('已有活动终端会话');
    expect(text).toContain('收尾');
    expect(text).toContain('稍候重试');
  });

  it('其他错误码回落通用映射（保留底层原因拼接）', () => {
    expect(describeTermCreateFailure(makeFailure('TERM_SESSION_LIMIT', 'x'))).toBe(
      '活动终端会话数达到上限（x）',
    );
    expect(describeTermCreateFailure(makeFailure('TERM_UNSUPPORTED', 'x'))).toBe(
      '该服务器不支持交互终端（WinRM 认证无 PTY；Telnet 接入的网络设备不支持终端）（x）',
    );
  });
});

// 三期（N3）：网络设备的终端通道判定与下拉文案（契约 2.8 约定 6）。
describe('termChannelOf 网络设备（三期）', () => {
  it('SSH 接入的网络设备可开终端（netdev=true, ssh=true, windows=false）', () => {
    expect(termChannelOf(netdevOf({ access_proto: 'ssh' }))).toEqual({
      ssh: true,
      windows: false,
      netdev: true,
    });
  });

  it('Telnet 接入的网络设备不可开终端（v1 无 telnet 终端桥）', () => {
    expect(termChannelOf(netdevOf({ access_proto: 'telnet', port: 23 }))).toEqual({
      ssh: false,
      windows: false,
      netdev: true,
    });
  });

  it('server 类判定不受影响：linux+password 可开、WinRM 不可开', () => {
    expect(termChannelOf(serverOf({ os: 'linux', auth_method: 'password' }))).toEqual({
      ssh: true,
      windows: false,
      netdev: false,
    });
    expect(termChannelOf(serverOf({})).ssh).toBe(false);
  });
});

describe('termServerOptionLabel 网络设备（三期）', () => {
  it('SSH 网络设备显示 host:port + 品牌 + 协议', () => {
    expect(termServerOptionLabel(netdevOf({ netdev_brand: 'huawei', access_proto: 'ssh' }))).toBe(
      'core-sw-1（127.0.0.1:22 · 华为 · SSH）',
    );
  });

  it('Telnet 网络设备同样显示协议（入口在 serverOptions 层禁用）', () => {
    expect(
      termServerOptionLabel(netdevOf({ netdev_brand: 'cisco', access_proto: 'telnet', port: 23 })),
    ).toBe('core-sw-1（127.0.0.1:23 · Cisco · TELNET）');
  });
});

// p3 sprint 3：ChatPage「服务器上下文」下拉改用 termServerOptionLabel
//（此前仅裸 server.name，网络设备无品牌/协议标识，同名时无法与服务器
// 区分，实测走查 sprint 3）。锁定共享口径的区分性。
describe('termServerOptionLabel 共享口径（p3 sprint 3：ChatPage 复用）', () => {
  it('同名时网络设备与服务器 label 可区分（网络设备带品牌/协议标识）', () => {
    const name = 'e2e-fake-host';
    const server = serverOf({ name, auth_method: 'password', os: 'linux', port: 22 });
    const netdev = netdevOf({ name, netdev_brand: 'cisco', access_proto: 'ssh', port: 18083 });
    expect(termServerOptionLabel(server)).toBe('e2e-fake-host（127.0.0.1:22）');
    expect(termServerOptionLabel(netdev)).toBe('e2e-fake-host（127.0.0.1:18083 · Cisco · SSH）');
    expect(termServerOptionLabel(server)).not.toBe(termServerOptionLabel(netdev));
  });
});

// p3 sprint 5：Telnet 接入的网络设备在终端下拉中是 disabled 项，点击零
// 反馈，「为什么不能开终端」此前只能靠 hover 原生 title；选中态警告
// Alert 因 disabled 不可选而不可达（死代码，已删除，语义由常驻提示
// 承接）。telnetNetdevNotice 决定「打开终端」卡片是否常驻说明。
describe('telnetNetdevNotice 常驻提示（p3 sprint 5）', () => {
  it('列表存在 Telnet 接入的网络设备 → 返回常驻文案（说明能力与替代路径）', () => {
    const notice = telnetNetdevNotice([netdevOf({ access_proto: 'telnet', port: 23 })]);
    expect(notice).toContain('Telnet 接入的网络设备暂不支持终端');
    expect(notice).toContain('v1 无 telnet 终端桥');
    expect(notice).toContain('命令执行请走对话或命令预览');
    // 常驻文案同时承接被删 Alert 的能力说明（SSH 网络设备可开终端）。
    expect(notice).toContain('网络设备经 SSH 通道提供交互式 CLI 终端');
  });

  it('混合列表（SSH 设备 + Telnet 设备）同样提示', () => {
    const notice = telnetNetdevNotice([
      netdevOf({ id: 'net-ssh', access_proto: 'ssh' }),
      netdevOf({ id: 'net-tel', access_proto: 'telnet', port: 23 }),
      serverOf({ id: 'srv-1', os: 'linux', auth_method: 'password' }),
    ]);
    expect(notice).not.toBeNull();
  });

  it('仅 SSH 接入的网络设备 → 不提示（不打扰普通部署）', () => {
    expect(telnetNetdevNotice([netdevOf({ access_proto: 'ssh' })])).toBeNull();
  });

  it('仅普通服务器 → 不提示', () => {
    expect(telnetNetdevNotice([serverOf({ os: 'linux', auth_method: 'password' })])).toBeNull();
  });

  it('server 类资产即使 access_proto 异常为 telnet 也不提示（判定按 device_kind）', () => {
    expect(
      telnetNetdevNotice([
        serverOf({
          os: 'linux',
          auth_method: 'password',
          access_proto: 'telnet' as Server['access_proto'],
        }),
      ]),
    ).toBeNull();
  });

  it('空列表 / undefined / null（加载中或无数据）→ 不提示', () => {
    expect(telnetNetdevNotice([])).toBeNull();
    expect(telnetNetdevNotice(undefined)).toBeNull();
    expect(telnetNetdevNotice(null)).toBeNull();
  });
});
