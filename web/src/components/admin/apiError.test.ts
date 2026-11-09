import { describe, expect, it } from 'vitest';

import {
  describeChangePasswordFailure,
  describeFailure,
  describeLoginFailure,
  failureFromError,
  isSyntheticInternalFailure,
  makeFailure,
} from './apiError';

// 第 4 轮打磨：describeFailure 过滤后端通用 store 映射透传的同义 message
// （conflict / not found / …），只把有信息量的 message（如具体重名冲突）
// 拼进中文文案。

describe('describeFailure', () => {
  it('CONFLICT + 通用同义 message "conflict"：只显示中文兜底，不拼无效后缀', () => {
    expect(describeFailure(makeFailure('CONFLICT', 'conflict'))).toBe(
      '资源冲突或当前状态不允许该操作',
    );
  });

  it('CONFLICT + 具体 message：中文兜底括号内展示具体冲突字段', () => {
    expect(describeFailure(makeFailure('CONFLICT', '服务器名称已存在'))).toBe(
      '资源冲突或当前状态不允许该操作（服务器名称已存在）',
    );
  });

  it('NOT_FOUND + 通用 message "not found"：同义过滤后不拼接', () => {
    expect(describeFailure(makeFailure('NOT_FOUND', 'not found'))).toBe('资源不存在');
  });

  it('未知错误码：有信息量 message 直接展示', () => {
    expect(describeFailure(makeFailure('SOME_NEW_CODE', 'upstream says no'))).toBe(
      'upstream says no',
    );
  });

  it('未知错误码 + 通用 message：兜底「请求失败」', () => {
    expect(describeFailure(makeFailure('SOME_NEW_CODE', 'request failed'))).toBe('请求失败');
  });

  it('空 message 回退中文文案', () => {
    expect(describeFailure(makeFailure('INTERNAL', ''))).toBe('服务器内部错误');
  });
});

// 第 6 轮打磨：后端连接测试失败已透出底层原因（dial 拒绝、无法打开
// 数据库文件等），describeFailure 必须把这类 informative message 拼进
// 中文文案；而 AUTH_INVALID 的固定 message（invalid credentials）与
// 修复前遗留的裸兜底短语没有信息量，不拼进文案。
describe('describeFailure 连接测试与凭据错误（第 6 轮）', () => {
  it('AUTH_INVALID + "invalid credentials"：登录失败只显示中文，不混入英文', () => {
    expect(describeFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '用户名或密码错误',
    );
  });

  it('SERVER_TEST_FAILED + SSH 通道带底层原因：标题按通道，括号内透出真实原因', () => {
    expect(
      describeFailure(
        makeFailure(
          'SERVER_TEST_FAILED',
          'ssh connection test failed: sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused',
        ),
      ),
    ).toBe(
      'SSH 连接测试失败（sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused）',
    );
  });

  it('SERVER_TEST_FAILED + WinRM 通道：标题按通道（二期 2.7 双通道同码）', () => {
    expect(
      describeFailure(
        makeFailure('SERVER_TEST_FAILED', 'winrm connection test failed: 401 Unauthorized'),
      ),
    ).toBe('WinRM 连接测试失败（401 Unauthorized）');
  });

  // p2 sprint 3：后端 message 升级为「通道前缀: 中文分类短语；原因：<底层>」
  // ——通道前缀剥掉后，括号内从中文分类短语开始，不再出现英文前缀重复。
  it('SERVER_TEST_FAILED + p2 sprint 3 中文分类 message：括号内以中文短语开头', () => {
    expect(
      describeFailure(
        makeFailure(
          'SERVER_TEST_FAILED',
          'ssh connection test failed: 无法连接 127.0.0.1:1：连接被拒绝（端口未开放或被防火墙拦截）；原因：sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused',
        ),
      ),
    ).toBe(
      'SSH 连接测试失败（无法连接 127.0.0.1:1：连接被拒绝（端口未开放或被防火墙拦截）；原因：sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused）',
    );
    expect(
      describeFailure(
        makeFailure(
          'SERVER_TEST_FAILED',
          'winrm connection test failed: 无法连接 127.0.0.1:5985：连接被拒绝（端口未开放或被防火墙拦截）；原因：winrmx: run command: ntlm: negotiate message encryption: dial tcp 127.0.0.1:5985: connect: connection refused',
        ),
      ),
    ).toBe(
      'WinRM 连接测试失败（无法连接 127.0.0.1:5985：连接被拒绝（端口未开放或被防火墙拦截）；原因：winrmx: run command: ntlm: negotiate message encryption: dial tcp 127.0.0.1:5985: connect: connection refused）',
    );
  });

  it('SERVER_TEST_FAILED + 无通道前缀的 message：退回通用文案并透出原因', () => {
    expect(describeFailure(makeFailure('SERVER_TEST_FAILED', 'dial timeout'))).toBe(
      '服务器连接测试失败（dial timeout）',
    );
  });

  // 三期 p3 sprint 1：网络设备（device_kind=network_device）连接测试同用
  // SERVER_TEST_FAILED，message 前缀 "network device connection test
  // failed: …"（netcli 探针）。修复前弹窗显示「服务器连接测试失败
  // （network device connection test failed: 无法连接 …）」——标题未按
  // 通道、前缀未剥离。走查原文（wt-netdev，127.0.0.1:18099 拒绝）。
  it('SERVER_TEST_FAILED + 网络设备通道：标题「网络设备连接测试失败」且剥离英文前缀（p3 sprint 1）', () => {
    expect(
      describeFailure(
        makeFailure(
          'SERVER_TEST_FAILED',
          'network device connection test failed: 无法连接 127.0.0.1:18099：连接被拒绝（端口未开放或被防火墙拦截）；原因：netdev: ssh handshake: sshx: dial tcp 127.0.0.1:18099: connect: connection refused',
        ),
      ),
    ).toBe(
      '网络设备连接测试失败（无法连接 127.0.0.1:18099：连接被拒绝（端口未开放或被防火墙拦截）；原因：netdev: ssh handshake: sshx: dial tcp 127.0.0.1:18099: connect: connection refused）',
    );
  });

  it('SERVER_TEST_FAILED + 网络设备裸前缀（无原因）：只显示通道化标题', () => {
    expect(
      describeFailure(makeFailure('SERVER_TEST_FAILED', 'network device connection test failed')),
    ).toBe('网络设备连接测试失败');
  });

  it('SERVER_TEST_FAILED + 裸兜底短语：同义过滤后只显示通道化中文标题', () => {
    expect(describeFailure(makeFailure('SERVER_TEST_FAILED', 'ssh connection test failed'))).toBe(
      'SSH 连接测试失败',
    );
  });

  it('DATABASE_TEST_FAILED + 带底层原因的 message：透出 sqlite 打不开文件', () => {
    expect(
      describeFailure(
        makeFailure(
          'DATABASE_TEST_FAILED',
          'database connection test failed: unable to open database file (14)',
        ),
      ),
    ).toBe('数据库连接测试失败（unable to open database file (14)）');
  });

  it('DATABASE_TEST_FAILED + 中文分类短语：剥离英文通道前缀，无中英重复（p2 sprint 5）', () => {
    expect(
      describeFailure(
        makeFailure(
          'DATABASE_TEST_FAILED',
          'database connection test failed: 无法连接 127.0.0.1:6399：连接被拒绝（端口未开放或被防火墙拦截）；原因：dial tcp 127.0.0.1:6399: connect: connection refused',
        ),
      ),
    ).toBe(
      '数据库连接测试失败（无法连接 127.0.0.1:6399：连接被拒绝（端口未开放或被防火墙拦截）；原因：dial tcp 127.0.0.1:6399: connect: connection refused）',
    );
  });

  it('SSH_EXEC_FAILED + 裸兜底短语：同义过滤后只显示中文', () => {
    expect(describeFailure(makeFailure('SSH_EXEC_FAILED', 'ssh command failed'))).toBe(
      '远程命令执行失败',
    );
  });

  it('SSH_EXEC_FAILED + 带原因的 message：透出真实原因', () => {
    expect(
      describeFailure(makeFailure('SSH_EXEC_FAILED', 'ssh command failed: handshake failed')),
    ).toBe('远程命令执行失败（ssh command failed: handshake failed）');
  });
});

// 冲刺 2 打磨：AUTH_RATE_LIMITED 的固定 message（too many login attempts，
// 后端 auth_handlers.go）没有额外信息量，且错误码已在 description 小字保留，
// 登录限流提示标题不再混入英文短语（与第 6 轮 invalid credentials 同构）。
describe('describeFailure 登录限流提示（冲刺 2）', () => {
  it('AUTH_RATE_LIMITED + "too many login attempts"：标题只显示中文', () => {
    expect(describeFailure(makeFailure('AUTH_RATE_LIMITED', 'too many login attempts'))).toBe(
      '登录尝试过于频繁，请稍后再试',
    );
  });
});

// 冲刺 2 第 2 轮打磨：改密弹窗没有用户名字段，AUTH_INVALID 在改密场景
// 必然来自旧密码校验失败，按场景改写为「旧密码不正确」；通用映射
// CODE_MESSAGES.AUTH_INVALID 维持「用户名或密码错误」不变（其他场景仍走
// describeFailure）。
describe('describeChangePasswordFailure 改密场景（冲刺 2 第 2 轮）', () => {
  it('AUTH_INVALID + "invalid credentials"：改密场景显示「旧密码不正确」', () => {
    expect(describeChangePasswordFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '旧密码不正确',
    );
  });

  it('AUTH_INVALID + 带信息量 message：括号内保留底层原因', () => {
    expect(
      describeChangePasswordFailure(makeFailure('AUTH_INVALID', 'old password mismatch: x')),
    ).toBe('旧密码不正确（old password mismatch: x）');
  });

  it('通用映射不受影响：describeFailure(AUTH_INVALID) 仍为「用户名或密码错误」', () => {
    expect(describeFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '用户名或密码错误',
    );
  });

  it('改密场景其他错误码走默认渲染', () => {
    expect(
      describeChangePasswordFailure(makeFailure('AUTH_RATE_LIMITED', 'too many login attempts')),
    ).toBe('登录尝试过于频繁，请稍后再试');
    expect(describeChangePasswordFailure(makeFailure('VALIDATION_FAILED', ''))).toBe(
      '表单校验未通过',
    );
  });
});

// 冲刺 5 打磨：登录页用户名输入框 disabled 固定为内置 admin，唯一可输入
// 凭据是管理员密码；AUTH_INVALID 在登录场景按 describeLoginFailure 改写为
// 「管理员密码错误」，不再引导用户去找不存在的可编辑用户名字段。
describe('describeLoginFailure 登录场景（冲刺 5）', () => {
  it('AUTH_INVALID + "invalid credentials"：登录场景显示「管理员密码错误」', () => {
    expect(describeLoginFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '管理员密码错误',
    );
  });

  it('AUTH_INVALID + 带信息量 message：括号内保留底层原因', () => {
    expect(describeLoginFailure(makeFailure('AUTH_INVALID', 'auth backend says: nope'))).toBe(
      '管理员密码错误（auth backend says: nope）',
    );
  });

  it('登录场景其他错误码走默认渲染', () => {
    expect(describeLoginFailure(makeFailure('AUTH_RATE_LIMITED', 'too many login attempts'))).toBe(
      '登录尝试过于频繁，请稍后再试',
    );
    expect(describeLoginFailure(makeFailure('INTERNAL', ''))).toBe('服务器内部错误');
  });

  it('与改密场景互不影响：describeChangePasswordFailure(AUTH_INVALID) 仍为「旧密码不正确」', () => {
    expect(describeChangePasswordFailure(makeFailure('AUTH_INVALID', 'invalid credentials'))).toBe(
      '旧密码不正确',
    );
  });
});

// 冲刺 7 打磨：登出/改密后 queryClient.clear() 中止在途 me 查询，其失败
// 兜底为不带 requestId 的合成 INTERNAL；登录页据此把会话结束视为预期态，
// 不再渲染「服务器内部错误」横幅。后端真实 INTERNAL 必带 request_id。
describe('isSyntheticInternalFailure（冲刺 7）', () => {
  it('makeFailure 兜底（空响应/中止）造出的 INTERNAL 无 requestId：是合成失败', () => {
    expect(isSyntheticInternalFailure(makeFailure('INTERNAL', 'empty me response'))).toBe(true);
    expect(isSyntheticInternalFailure(makeFailure('INTERNAL', '请求失败'))).toBe(true);
  });

  it('failureFromError 对非契约错误值（如 AbortError）兜底为合成 INTERNAL', () => {
    expect(isSyntheticInternalFailure(failureFromError(undefined))).toBe(true);
    expect(isSyntheticInternalFailure(failureFromError('boom'))).toBe(true);
  });

  it('带 requestId 的 INTERNAL 来自后端契约（body.request_id 或 X-Request-ID）：不是合成失败', () => {
    expect(
      isSyntheticInternalFailure({
        ...makeFailure('INTERNAL', 'db unavailable'),
        requestId: 'req-abc123',
      }),
    ).toBe(false);
  });

  it('其他错误码与登录页排除规则（AUTH_REQUIRED）无关：一律不是合成 INTERNAL', () => {
    expect(isSyntheticInternalFailure(makeFailure('AUTH_REQUIRED', 'login required'))).toBe(false);
    expect(isSyntheticInternalFailure(makeFailure('NETWORK_ERROR', 'HTTP 502'))).toBe(false);
  });
});
