import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { makeFailure } from '../../../components/admin/apiError';
import {
  localizeCleartextWarning,
  localizeTestFailure,
  serverMatchesKeyword,
} from '../ServersPage';

type Server = components['schemas']['Server'];

function serverOf(overrides: Partial<Server> = {}): Server {
  return {
    id: 'srv-1',
    name: 'walk-host-2',
    device_kind: 'server',
    os: 'linux',
    netdev_brand: 'generic',
    access_proto: 'ssh',
    host: '10.0.0.99',
    port: 2222,
    winrm_port: 5985,
    username: 'root',
    auth_method: 'password',
    tags: ['walk'],
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

// 第 10 轮打磨：地址列渲染 host:port，端口此前不参与搜索（按「2222」
// 恒 0 行）。serverMatchesKeyword 抽出为纯函数后在此做行为矩阵回归。
describe('serverMatchesKeyword 端口参与匹配（第 10 轮）', () => {
  it('裸端口命中：2222 匹配 port=2222 的服务器', () => {
    expect(serverMatchesKeyword(serverOf({ port: 2222 }), '2222')).toBe(true);
  });

  it('host:port 组合命中（与地址列渲染文本一致，复制地址搜索不漏）', () => {
    expect(serverMatchesKeyword(serverOf(), '10.0.0.99:2222')).toBe(true);
  });

  it('端口不匹配时仍过滤：3333 搜不到 2222 端口的服务器', () => {
    expect(serverMatchesKeyword(serverOf({ port: 2222 }), '3333')).toBe(false);
  });

  it('既有字段匹配不受影响：名称 / host / 用户 / 标签', () => {
    const server = serverOf();
    expect(serverMatchesKeyword(server, 'walk-host-2')).toBe(true);
    expect(serverMatchesKeyword(server, '10.0.0.99')).toBe(true);
    expect(serverMatchesKeyword(server, 'root')).toBe(true);
    expect(serverMatchesKeyword(server, 'walk')).toBe(true);
  });

  it('大小写不敏感；关键词仅空白视为不过滤', () => {
    const server = serverOf({ name: 'Prod-DB' });
    expect(serverMatchesKeyword(server, 'prod-db')).toBe(true);
    expect(serverMatchesKeyword(server, '   ')).toBe(true);
    expect(serverMatchesKeyword(server, '')).toBe(true);
  });

  it('默认端口 5432 也可按端口搜到', () => {
    expect(serverMatchesKeyword(serverOf({ port: 5432 }), '5432')).toBe(true);
  });
});

// 三期（N3）：网络设备行参与关键词匹配（品牌 / 接入协议 / 设备类型）。
describe('serverMatchesKeyword 网络设备字段（三期）', () => {
  const netdev = serverOf({
    device_kind: 'network_device',
    os: 'network',
    netdev_brand: 'huawei',
    access_proto: 'telnet',
    port: 23,
  });

  it('品牌关键词命中（枚举值与中文标签均可）', () => {
    expect(serverMatchesKeyword(netdev, 'huawei')).toBe(true);
    expect(serverMatchesKeyword(netdev, '华为')).toBe(true);
  });

  it('接入协议与设备类型关键词命中', () => {
    expect(serverMatchesKeyword(netdev, 'telnet')).toBe(true);
    expect(serverMatchesKeyword(netdev, 'network_device')).toBe(true);
  });

  it('server 形态搜索网络设备关键词不命中', () => {
    expect(serverMatchesKeyword(serverOf(), 'telnet')).toBe(false);
  });
});

// 三期评审：server 类资产曾被常量值污染（netdevBrandLabel('') = 「通用」、
// access_proto 恒 'ssh'），搜索「通用」「通」「ssh」会匹配所有服务器行。
describe('serverMatchesKeyword 常量污染（三期）', () => {
  it('server 类资产：搜「通用」「通」「ssh」不匹配（这些是 netdev 专属展示值）', () => {
    // 后端对 server 类资产实际返回 netdev_brand:''（schema 枚举未表达）。
    const server = serverOf({ netdev_brand: '' as Server['netdev_brand'] });
    expect(serverMatchesKeyword(server, '通用')).toBe(false);
    expect(serverMatchesKeyword(server, '通')).toBe(false);
    expect(serverMatchesKeyword(server, 'ssh')).toBe(false);
  });

  it('网络设备：品牌中文名与接入协议仍可命中', () => {
    const netdev = serverOf({
      device_kind: 'network_device',
      os: 'network',
      netdev_brand: 'cisco',
      access_proto: 'telnet',
    });
    expect(serverMatchesKeyword(netdev, '通用')).toBe(false); // 品牌是 cisco
    expect(serverMatchesKeyword(netdev, 'telnet')).toBe(true);
    expect(serverMatchesKeyword(netdev, 'Cisco')).toBe(true); // netdevBrandLabel 显示英文品牌名
  });

  it('常规字段不受影响：名称/主机/端口/标签仍命中', () => {
    const server = serverOf({ netdev_brand: '' as Server['netdev_brand'] });
    expect(serverMatchesKeyword(server, 'walk-host')).toBe(true);
    expect(serverMatchesKeyword(server, '10.0.0.99')).toBe(true);
    expect(serverMatchesKeyword(server, '2222')).toBe(true);
    expect(serverMatchesKeyword(server, 'walk')).toBe(true);
  });
});

// p3 sprint 3：NETCLI_*（SSH 登录/expect 失败）与 SERVER_TEST_FAILED 同源，
// 后端 message 头部带英文通道前缀 "network device connection test failed"。
// localizeTestFailure 拼中文标题前必须剥掉该前缀，否则弹窗出现
// 「网络设备登录失败：network device connection test failed: 无法连接 …」
// 的中英重复（实测走查 sprint 3，request_id 9469b0ddf1b46af8）。
describe('localizeTestFailure 剥离英文通道前缀（p3 sprint 3）', () => {
  const RAW =
    'network device connection test failed: 无法连接 127.0.0.1:18083：连接超时（未在时限内完成握手）；原因：netdev: device login failed: expect timeout after total budget';

  it('NETCLI_LOGIN_FAILED：剥离前缀后拼「网络设备登录失败：无法连接 …」', () => {
    expect(localizeTestFailure(makeFailure('NETCLI_LOGIN_FAILED', RAW)).message).toBe(
      '网络设备登录失败：无法连接 127.0.0.1:18083：连接超时（未在时限内完成握手）；原因：netdev: device login failed: expect timeout after total budget',
    );
  });

  it('NETCLI_EXEC_FAILED：同样剥离前缀', () => {
    expect(localizeTestFailure(makeFailure('NETCLI_EXEC_FAILED', RAW)).message).toBe(
      '网络设备命令执行失败：无法连接 127.0.0.1:18083：连接超时（未在时限内完成握手）；原因：netdev: device login failed: expect timeout after total budget',
    );
  });

  it('无前缀的 message 原样拼中文标题（不做额外处理）', () => {
    expect(
      localizeTestFailure(makeFailure('NETCLI_LOGIN_FAILED', 'device login failed')).message,
    ).toBe('网络设备登录失败：device login failed');
  });

  it('非 NETCLI_* 错误码原样返回（不本地化）', () => {
    const failure = makeFailure('SERVER_TEST_FAILED', RAW);
    expect(localizeTestFailure(failure)).toBe(failure);
  });

  it('message 仅剩前缀时回落为纯中文标题', () => {
    expect(
      localizeTestFailure(
        makeFailure('NETCLI_LOGIN_FAILED', 'network device connection test failed'),
      ).message,
    ).toBe('网络设备登录失败');
  });

  // p3 sprint 5：后端对登录/执行期哨兵失败补中文分类（describeNetdev
  // TestFailure，「无法登录 host:port：设备侧在登录阶段断开通道」），
  // 前端剥 http 前缀后 detail 直接以中文分类开头——锁定新形态不再出现
  // 「网络设备登录失败：netdev: device login failed: …」中英重复。
  it('p3 sprint 5：后端哨兵分类形态剥前缀后 detail 以中文分类开头', () => {
    const RAW5 =
      'network device connection test failed: 无法登录 192.0.2.123:23：设备侧在登录阶段断开通道；原因：channel closed: EOF';
    const out = localizeTestFailure(makeFailure('NETCLI_LOGIN_FAILED', RAW5));
    expect(out.message).toBe(
      '网络设备登录失败：无法登录 192.0.2.123:23：设备侧在登录阶段断开通道；原因：channel closed: EOF',
    );
    expect(out.message).not.toContain('netdev: device login failed');
  });
});

// p3 sprint 4：Telnet 连接测试成功弹窗的明文风险告警此前把后端英文常量
// 原文直达用户（telnetx.CleartextWarning，走查 W2 实测），与全中文弹窗
// 口径相悖——localizeCleartextWarning 对已知文案映射中文、未知值回退原文。
describe('localizeCleartextWarning 明文告警中文化（p3 sprint 4）', () => {
  // 与 internal/netdev/telnetx/telnetx.go:29 的 CleartextWarning 常量逐字一致
  //（后端单一事实源，前端只映射不改写）。
  const RAW =
    'telnet transmits the login password and enable secret in cleartext; prefer ssh when the device supports it';

  it('已知文案（后端常量原文）映射为中文告警', () => {
    expect(localizeCleartextWarning(RAW)).toBe(
      'Telnet 以明文传输登录与特权密码，设备支持时建议改用 SSH',
    );
  });

  it('首尾空白不干扰匹配（trim 后映射）', () => {
    expect(localizeCleartextWarning(`  ${RAW}\n`)).toBe(
      'Telnet 以明文传输登录与特权密码，设备支持时建议改用 SSH',
    );
  });

  it('未知文案回退原文（不吞后端语义）', () => {
    expect(localizeCleartextWarning('some future warning text')).toBe('some future warning text');
  });

  it('null / undefined 归一为 null（无告警不渲染）', () => {
    expect(localizeCleartextWarning(null)).toBeNull();
    expect(localizeCleartextWarning(undefined)).toBeNull();
  });

  it('空串保持原 ?? null 透传语义（falsy，弹窗同样不渲染）', () => {
    expect(localizeCleartextWarning('')).toBe('');
  });
});
