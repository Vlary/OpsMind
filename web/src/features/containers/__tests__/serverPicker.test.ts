import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { containerServerOptions, pickDefaultServer } from '../serverPicker';

type Server = components['schemas']['Server'];

let seq = 0;

/** 完整 Server fixture（契约 3.2 全字段；container_runtime 按需覆盖）。 */
function makeServer(overrides: Partial<Server>): Server {
  seq += 1;
  return {
    id: `srv-${seq}`,
    name: `server-${seq}`,
    device_kind: 'server',
    os: 'linux',
    netdev_brand: 'generic',
    access_proto: 'ssh',
    host: '10.0.0.1',
    port: 22,
    winrm_port: 5985,
    username: 'root',
    auth_method: 'password',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: '',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    ...overrides,
  };
}

describe('containerServerOptions（契约 8.7：仅 server 类资产；无运行时禁用并提示）', () => {
  it('network_device 不进下拉（容器接口一律 400 CONTAINER_UNSUPPORTED）', () => {
    const options = containerServerOptions([
      makeServer({ device_kind: 'network_device', os: 'network' as Server['os'] }),
      makeServer({ container_runtime: 'docker' }),
    ]);
    expect(options).toHaveLength(1);
    expect(options[0]?.runtime).toBe('docker');
  });

  it('none/containerd 禁用且 title 说明原因；空串（未探测）可选——选中触发探测', () => {
    const options = containerServerOptions([
      makeServer({ container_runtime: 'none' }),
      makeServer({ container_runtime: 'containerd' }),
      makeServer({ container_runtime: '' }),
      makeServer({ container_runtime: 'podman' }),
    ]);
    expect(options[0]).toMatchObject({ disabled: true });
    expect(options[0]?.title).toContain('未探测到');
    expect(options[1]).toMatchObject({ disabled: true });
    expect(options[1]?.title).toContain('containerd');
    expect(options[2]).toMatchObject({ disabled: false, title: null });
    expect(options[3]).toMatchObject({ disabled: false });
  });

  it('下拉文案含 host:port（同名服务器可区分）', () => {
    const options = containerServerOptions([
      makeServer({ name: 'web', host: '10.1.1.1', port: 2222 }),
    ]);
    expect(options[0]?.label).toBe('web（10.1.1.1:2222）');
  });
});

describe('pickDefaultServer（七期走查：不预选无运行时的服务器）', () => {
  it('优先选 docker/podman，跳过列表更靠前的 none/containerd', () => {
    const servers = [
      makeServer({ container_runtime: 'none' }),
      makeServer({ container_runtime: 'containerd' }),
      makeServer({ container_runtime: 'podman' }),
      makeServer({ container_runtime: 'docker' }),
    ];
    expect(pickDefaultServer(servers)).toBe(servers[2]?.id);
  });

  it('无已确认运行时但存在未探测（空串）服务器时兜底选它（触发一次探测）', () => {
    const servers = [
      makeServer({ container_runtime: 'none' }),
      makeServer({ container_runtime: '' }),
    ];
    expect(pickDefaultServer(servers)).toBe(servers[1]?.id);
  });

  it('全部为 none/containerd 时返回 null（不预选，进页不发明知 400 的请求）', () => {
    const servers = [
      makeServer({ container_runtime: 'none' }),
      makeServer({ container_runtime: 'containerd' }),
    ];
    expect(pickDefaultServer(servers)).toBeNull();
  });

  it('network_device 不参与选择（即使 runtime 空串）', () => {
    const servers = [
      makeServer({
        device_kind: 'network_device',
        os: 'network' as Server['os'],
        container_runtime: '',
      }),
      makeServer({ container_runtime: 'none' }),
    ];
    expect(pickDefaultServer(servers)).toBeNull();
  });

  it('无 server 类资产返回 null', () => {
    expect(pickDefaultServer([])).toBeNull();
    expect(
      pickDefaultServer([
        makeServer({ device_kind: 'network_device', os: 'network' as Server['os'] }),
      ]),
    ).toBeNull();
  });
});
