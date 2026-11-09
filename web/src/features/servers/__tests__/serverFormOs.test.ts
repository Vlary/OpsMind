import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import {
  authMethodOptionsForOS,
  AUTH_METHOD_LABELS,
  isWinRMAuthMethod,
  normalizeAuthMethodForOS,
  type AuthMethod,
  type TargetOS,
} from '../../../components/admin/meta';
import {
  buildCreatePayload,
  buildUpdatePayload,
  editingHostKeyNoticeVisible,
  resolveNetdevFields,
  type ServerFormValues,
} from '../ServerFormDrawer';

type Server = components['schemas']['Server'];

function serverOf(overrides: Partial<Server> = {}): Server {
  return {
    id: 'srv-1',
    name: 'win-host-1',
    device_kind: 'server',
    os: 'windows',
    netdev_brand: 'generic',
    access_proto: 'ssh',
    host: '10.0.0.5',
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

function netdevServerOf(overrides: Partial<Server> = {}): Server {
  return serverOf({
    id: 'net-1',
    name: 'core-sw-1',
    device_kind: 'network_device',
    os: 'network',
    netdev_brand: 'cisco',
    access_proto: 'ssh',
    host: '10.0.0.254',
    port: 22,
    winrm_port: 5985,
    username: 'admin',
    auth_method: 'password',
    has_password: true,
    ...overrides,
  });
}

interface FormValuesInput {
  name?: string;
  host?: string;
  username?: string;
  device_kind?: components['schemas']['DeviceKind'];
  os?: TargetOS;
  netdev_brand?: components['schemas']['NetdevBrand'];
  access_proto?: components['schemas']['AccessProto'];
  port?: number;
  auth_method?: AuthMethod;
  winrm_port?: number;
  password?: string;
  private_key?: string;
  passphrase?: string;
  enable_secret?: string;
  clear_enable_secret?: boolean;
}

function formValues(input: FormValuesInput = {}): ServerFormValues {
  return {
    name: 'win-host-1',
    host: '10.0.0.5',
    port: 22,
    username: 'administrator',
    tags: [],
    auto_exec_enabled: false,
    device_kind: input.device_kind ?? 'server',
    os: input.os ?? 'linux',
    auth_method: input.auth_method ?? 'password',
    ...input,
  };
}

// 二期 F2：os × auth_method 联动（契约 2.6/2.7）。
// linux 只允许 password/private_key；windows 额外允许 WinRM 的 NTLM/Basic。

describe('authMethodOptionsForOS 按 os 过滤选项', () => {
  it('linux 只有 password/private_key 两项', () => {
    expect(authMethodOptionsForOS('linux').map((option) => option.value)).toEqual([
      'password',
      'private_key',
    ]);
  });

  it('windows 四项：password/private_key/password_ntlm/password_basic', () => {
    expect(authMethodOptionsForOS('windows').map((option) => option.value)).toEqual([
      'password',
      'private_key',
      'password_ntlm',
      'password_basic',
    ]);
  });

  it('选项 label 与 AUTH_METHOD_LABELS 同源一致', () => {
    for (const option of [
      ...authMethodOptionsForOS('linux'),
      ...authMethodOptionsForOS('windows'),
    ]) {
      expect(AUTH_METHOD_LABELS[option.value]).toBe(option.label);
    }
  });
});

describe('normalizeAuthMethodForOS 切换 os 后的认证方式归一', () => {
  it('linux 下 WinRM 认证回退为 password（后端会 400，前端先归一）', () => {
    expect(normalizeAuthMethodForOS('linux', 'password_ntlm')).toBe('password');
    expect(normalizeAuthMethodForOS('linux', 'password_basic')).toBe('password');
  });

  it('windows 下四种认证全部保留', () => {
    for (const method of ['password', 'private_key', 'password_ntlm', 'password_basic'] as const) {
      expect(normalizeAuthMethodForOS('windows', method)).toBe(method);
    }
  });

  it('linux 下 password/private_key 不变', () => {
    expect(normalizeAuthMethodForOS('linux', 'password')).toBe('password');
    expect(normalizeAuthMethodForOS('linux', 'private_key')).toBe('private_key');
  });
});

describe('isWinRMAuthMethod', () => {
  it('仅 NTLM/Basic 为 WinRM 通道', () => {
    expect(isWinRMAuthMethod('password_ntlm')).toBe(true);
    expect(isWinRMAuthMethod('password_basic')).toBe(true);
    expect(isWinRMAuthMethod('password')).toBe(false);
    expect(isWinRMAuthMethod('private_key')).toBe(false);
  });
});

describe('buildCreatePayload os/winrm 联动', () => {
  it('linux + password：os=linux，winrm_port 缺省 5985，凭据走 password', () => {
    const payload = buildCreatePayload(formValues({ password: 'pw' }));
    expect(payload.os).toBe('linux');
    expect(payload.winrm_port).toBe(5985);
    expect(payload.password).toBe('pw');
    expect(payload.private_key).toBeUndefined();
  });

  it('windows + NTLM：携带表单 os 与 winrm_port，密码写入 password 字段', () => {
    const payload = buildCreatePayload(
      formValues({ os: 'windows', auth_method: 'password_ntlm', winrm_port: 5986, password: 'pw' }),
    );
    expect(payload.os).toBe('windows');
    expect(payload.winrm_port).toBe(5986);
    expect(payload.auth_method).toBe('password_ntlm');
    expect(payload.password).toBe('pw');
  });

  it('windows + Basic：同样走 password 字段', () => {
    const payload = buildCreatePayload(
      formValues({ os: 'windows', auth_method: 'password_basic', password: 'pw' }),
    );
    expect(payload.password).toBe('pw');
    expect(payload.winrm_port).toBe(5985);
  });

  it('private_key：凭据走 private_key/passphrase，不带 password', () => {
    const payload = buildCreatePayload(
      formValues({ auth_method: 'private_key', private_key: 'KEY', passphrase: 'ph' }),
    );
    expect(payload.private_key).toBe('KEY');
    expect(payload.passphrase).toBe('ph');
    expect(payload.password).toBeUndefined();
  });
});

describe('buildUpdatePayload os/winrm 增量', () => {
  it('os 与 winrm_port 未变化时不携带两字段', () => {
    const server = serverOf();
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'windows', auth_method: 'password_ntlm', winrm_port: 5985 }),
    );
    expect(payload.os).toBeUndefined();
    expect(payload.winrm_port).toBeUndefined();
  });

  it('winrm_port 变化时携带新值（服务端据此失效 WinRM 连接池）', () => {
    const server = serverOf();
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'windows', auth_method: 'password_ntlm', winrm_port: 5986 }),
    );
    expect(payload.winrm_port).toBe(5986);
  });

  it('windows+NTLM 改回 linux（认证归一为 password）：携带 os 与 auth_method，凭据必填', () => {
    const server = serverOf();
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'linux', auth_method: 'password', password: 'pw' }),
    );
    expect(payload.os).toBe('linux');
    expect(payload.auth_method).toBe('password');
    expect(payload.password).toBe('pw');
  });

  it('linux+password 切到 windows（认证不变）：仅携带 os，不动凭据', () => {
    const server = serverOf({
      os: 'linux',
      auth_method: 'password',
    });
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'windows', auth_method: 'password' }),
    );
    expect(payload.os).toBe('windows');
    expect(payload.auth_method).toBeUndefined();
    expect(payload.password).toBeUndefined();
  });
});

describe('buildUpdatePayload 残留凭据死锁（评审发现）', () => {
  it('编辑私钥服务器时输入过密码再切回私钥：残留 password 不触发 auth_method，避免 400', () => {
    const server = serverOf({
      os: 'linux',
      auth_method: 'private_key',
      has_password: false,
      has_private_key: true,
    });
    // 用户切到 password 认证输入了密码，又切回 private_key（表单残留 password）。
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'linux', auth_method: 'private_key', password: 'leftover-pw' }),
    );
    // methodChanged=false 且 private_key 无新输入：不得携带 auth_method，
    // 否则 allOf 条件 required private_key 缺失 → 服务端 400，且密码框
    // 已卸载无法清除残留值，保存死锁。
    expect(payload.auth_method).toBeUndefined();
    expect(payload.private_key).toBeUndefined();
    expect(payload.password).toBeUndefined();
  });

  it('残留 password 不影响合法的新私钥提交：private_key 有输入时正常携带', () => {
    const server = serverOf({
      os: 'linux',
      auth_method: 'private_key',
      has_private_key: true,
    });
    const payload = buildUpdatePayload(
      server,
      formValues({
        os: 'linux',
        auth_method: 'private_key',
        password: 'leftover-pw',
        private_key: '-----BEGIN…',
      }),
    );
    expect(payload.auth_method).toBe('private_key');
    expect(payload.private_key).toBe('-----BEGIN…');
    expect(payload.password).toBeUndefined();
  });

  it('残留 private_key 同理不影响密码方式提交', () => {
    const server = serverOf({
      os: 'linux',
      auth_method: 'password',
      has_password: true,
    });
    const payload = buildUpdatePayload(
      server,
      formValues({ os: 'linux', auth_method: 'password', private_key: 'leftover-key' }),
    );
    expect(payload.auth_method).toBeUndefined();
    expect(payload.private_key).toBeUndefined();
  });
});

// 三期（N3）：device_kind / 品牌五选 / access_proto / telnet 明文确认 /
// enable_secret（契约 2.6、2.8）。
describe('resolveNetdevFields 三期字段归一', () => {
  it('network_device：os=network、auth_method=password，品牌/协议取缺省 cisco/ssh', () => {
    expect(
      resolveNetdevFields(
        formValues({ device_kind: 'network_device', auth_method: 'private_key' }),
      ),
    ).toEqual({
      os: 'network',
      netdev_brand: 'cisco',
      access_proto: 'ssh',
      auth_method: 'password',
    });
  });

  it('server：netdev_brand 恒空串、access_proto 恒 ssh，不携带网络设备字段', () => {
    expect(
      resolveNetdevFields(
        formValues({ device_kind: 'server', netdev_brand: 'huawei', access_proto: 'telnet' }),
      ),
    ).toEqual({
      os: 'linux',
      netdev_brand: '',
      access_proto: 'ssh',
      auth_method: 'password',
    });
  });
});

describe('buildCreatePayload 网络设备（三期）', () => {
  it('ssh + 品牌：device_kind/netdev_brand/access_proto 透传，confirm=false，凭据走 password', () => {
    const payload = buildCreatePayload(
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'huawei',
        access_proto: 'ssh',
        port: 22,
        password: 'pw',
      }),
    );
    expect(payload.device_kind).toBe('network_device');
    expect(payload.os).toBe('network');
    expect(payload.netdev_brand).toBe('huawei');
    expect(payload.access_proto).toBe('ssh');
    expect(payload.auth_method).toBe('password');
    expect(payload.password).toBe('pw');
    expect(payload.confirm_telnet_cleartext).toBe(false);
    expect(payload.enable_secret).toBeUndefined();
  });

  it('telnet：confirm_telnet_cleartext 必须显式 true（契约 2.6：缺失/false 服务端 400）', () => {
    const payload = buildCreatePayload(
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'telnet',
        port: 23,
        password: 'pw',
      }),
    );
    expect(payload.confirm_telnet_cleartext).toBe(true);
    expect(payload.access_proto).toBe('telnet');
  });

  it('enable_secret 提供即携带；server 形态不携带', () => {
    const payload = buildCreatePayload(
      formValues({ device_kind: 'network_device', password: 'pw', enable_secret: 'secret' }),
    );
    expect(payload.enable_secret).toBe('secret');
    const serverPayload = buildCreatePayload(formValues({ password: 'pw', enable_secret: 'x' }));
    expect(serverPayload.enable_secret).toBeUndefined();
  });

  it('server 形态：netdev_brand 空串、access_proto=ssh；不携带网络设备专属字段（契约 2.6）', () => {
    const payload = buildCreatePayload(
      formValues({ os: 'windows', auth_method: 'password_ntlm', password: 'pw' }),
    );
    expect(payload.netdev_brand).toBe('');
    expect(payload.access_proto).toBe('ssh');
    // 普通服务器携带 confirm_telnet_cleartext 会被服务端以
    // 「only allowed for network devices」拒绝（三期拼装接口不一致修复）。
    expect(payload.confirm_telnet_cleartext).toBeUndefined();
    expect('confirm_telnet_cleartext' in payload).toBe(false);
  });
});

describe('buildUpdatePayload 网络设备增量（三期）', () => {
  it('编辑网络设备未变化：不携带 device_kind/os/netdev_brand/access_proto', () => {
    const server = netdevServerOf();
    const payload = buildUpdatePayload(
      server,
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'ssh',
        host: '10.0.0.254',
        port: 22,
        username: 'admin',
        name: 'core-sw-1',
      }),
    );
    expect(payload.device_kind).toBeUndefined();
    expect(payload.os).toBeUndefined();
    expect(payload.netdev_brand).toBeUndefined();
    expect(payload.access_proto).toBeUndefined();
    expect(payload.confirm_telnet_cleartext).toBe(false);
  });

  it('ssh 改 telnet：携带 access_proto 且 confirm=true（更新使 telnet 生效）', () => {
    const server = netdevServerOf({ access_proto: 'ssh' });
    const payload = buildUpdatePayload(
      server,
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'telnet',
        host: '10.0.0.254',
        port: 23,
        username: 'admin',
        name: 'core-sw-1',
      }),
    );
    expect(payload.access_proto).toBe('telnet');
    expect(payload.confirm_telnet_cleartext).toBe(true);
  });

  it('编辑 telnet 网络设备（协议不变）：confirm 仍显式 true（契约 2.6 更新请求口径）', () => {
    const server = netdevServerOf({ access_proto: 'telnet', port: 23 });
    const payload = buildUpdatePayload(
      server,
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'telnet',
        host: '10.0.0.254',
        port: 23,
        username: 'admin',
        name: 'core-sw-1',
      }),
    );
    expect(payload.access_proto).toBeUndefined();
    expect(payload.confirm_telnet_cleartext).toBe(true);
  });

  it('enable_secret 提供即替换；勾选清除发送空串；server 切到 network_device 携带全套字段', () => {
    const server = netdevServerOf({ has_enable_secret: true });
    const replaced = buildUpdatePayload(
      server,
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'ssh',
        host: '10.0.0.254',
        port: 22,
        username: 'admin',
        name: 'core-sw-1',
        enable_secret: 'new-secret',
      }),
    );
    expect(replaced.enable_secret).toBe('new-secret');

    const cleared = buildUpdatePayload(
      server,
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'cisco',
        access_proto: 'ssh',
        host: '10.0.0.254',
        port: 22,
        username: 'admin',
        name: 'core-sw-1',
        clear_enable_secret: true,
      }),
    );
    expect(cleared.enable_secret).toBe('');

    const switched = buildUpdatePayload(
      serverOf({ os: 'windows', auth_method: 'password_ntlm' }),
      formValues({
        device_kind: 'network_device',
        netdev_brand: 'h3c',
        access_proto: 'ssh',
        host: '10.0.0.5',
        port: 22,
        username: 'admin',
        name: 'win-host-1',
        password: 'pw',
      }),
    );
    expect(switched.device_kind).toBe('network_device');
    expect(switched.os).toBe('network');
    expect(switched.netdev_brand).toBe('h3c');
    expect(switched.access_proto).toBe('ssh');
    expect(switched.auth_method).toBe('password');
    expect(switched.password).toBe('pw');
  });
});

// p3 sprint 3：编辑态主机密钥提示的可见性（对齐 serverChannelOf 的
// hasHostKey 语义）。此前对 telnet 网络设备也渲染「修改地址或端口会清除
// 已信任的主机密钥」，与同一弹窗底部「Telnet 通道为登录探针，无主机
// 密钥语义」自相矛盾（实测走查 sprint 3）。
describe('editingHostKeyNoticeVisible 编辑态提示按通道区分（p3 sprint 3）', () => {
  it('SSH 接入的网络设备：有主机密钥语义，渲染提示', () => {
    expect(editingHostKeyNoticeVisible(netdevServerOf({ access_proto: 'ssh' }))).toBe(true);
  });

  it('Telnet 接入的网络设备：登录探针无主机密钥语义，不渲染', () => {
    expect(editingHostKeyNoticeVisible(netdevServerOf({ access_proto: 'telnet' }))).toBe(false);
  });

  it('WinRM 认证的 Windows 服务器：无 TOFU 语义，不渲染', () => {
    expect(editingHostKeyNoticeVisible(serverOf({ auth_method: 'password_ntlm' }))).toBe(false);
    expect(editingHostKeyNoticeVisible(serverOf({ auth_method: 'password_basic' }))).toBe(false);
  });

  it('SSH 通道的服务器（linux 全部 / windows password/private_key）：渲染提示', () => {
    expect(editingHostKeyNoticeVisible(serverOf({ os: 'linux', auth_method: 'password' }))).toBe(
      true,
    );
    expect(
      editingHostKeyNoticeVisible(serverOf({ os: 'windows', auth_method: 'private_key' })),
    ).toBe(true);
  });
});
