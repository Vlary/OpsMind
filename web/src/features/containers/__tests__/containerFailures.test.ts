import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import {
  commandSectionLabel,
  describeContainerFailure,
  describeDecisionOutcome,
  previewContainerCommand,
} from '../containerFailures';

type Approval = components['schemas']['Approval'];

function makeApproval(overrides: Partial<Approval>): Approval {
  return {
    id: 'apr-1',
    conversation_id: null,
    tool_call_id: null,
    source: 'console',
    server_id: 'srv-1',
    server_name: 'web-01',
    command: 'docker stop abc123def456',
    risk_level: 'needs_approval',
    rule_id: 'CONTAINER_CHANGE',
    reason: '容器变更需人工确认',
    status: 'pending',
    requested_at: '2026-10-27T00:00:00Z',
    expires_at: '2026-10-27T00:02:00Z',
    decided_at: null,
    decided_by: null,
    note: '',
    approval_mode: 'standard',
    edited_command: '',
    edited_risk_level: '',
    edited_rule_id: '',
    ...overrides,
  };
}

describe('describeContainerFailure（契约 9.6 CONTAINER_* 中文映射）', () => {
  it('三个容器错误码映射中文', () => {
    expect(
      describeContainerFailure({
        code: 'CONTAINER_UNSUPPORTED',
        message: 'unsupported',
        requestId: 'r1',
        details: null,
        status: 400,
      }),
    ).toContain('未探测到受支持的容器运行时');
    expect(
      describeContainerFailure({
        code: 'CONTAINER_NOT_FOUND',
        message: 'no such container',
        requestId: 'r2',
        details: null,
        status: 404,
      }),
    ).toContain('容器不存在');
    expect(
      describeContainerFailure({
        code: 'CONTAINER_CLI_FAILED',
        message: 'exit 1',
        requestId: 'r3',
        details: null,
        status: 502,
      }),
    ).toContain('CLI 执行失败');
  });

  it('SSH_EXEC_FAILED（通道层失败，p4 sprint 6 走查 D）：文案指向服务器连接而非 daemon', () => {
    const text = describeContainerFailure({
      code: 'SSH_EXEC_FAILED',
      message: 'sshx: dial 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused',
      requestId: 'r7',
      details: null,
      status: 502,
    });
    expect(text).toContain('无法连接服务器执行容器命令');
    expect(text).toContain('可先在服务器页测试连接');
    // 根因是服务器 SSH 不可达，不得再断言 daemon。
    expect(text).not.toContain('daemon');
  });

  it('CONTAINER_UNSUPPORTED 携带 detail.runtime 时拼进现值（契约 S21 ①）', () => {
    const text = describeContainerFailure({
      code: 'CONTAINER_UNSUPPORTED',
      message: 'unsupported',
      requestId: 'r4',
      details: { runtime: 'containerd' },
      status: 400,
    });
    expect(text).toContain('当前运行时：containerd');
    expect(text).toContain('不支持管理');
    // runtime 为空串（network_device）不拼接。
    expect(
      describeContainerFailure({
        code: 'CONTAINER_UNSUPPORTED',
        message: 'x',
        requestId: '',
        details: { runtime: '' },
        status: 400,
      }),
    ).not.toContain('当前运行时');
  });

  it('探测通道错误（detail=none 且库值仍 ""）：改「本次探测失败」文案，不断言「当前运行时：none」', () => {
    // 四期走查 T1：SSH 立拒 → 后端按 none 返回 400 但不写回（库值 ''），
    // 「当前运行时：none」与库中状态矛盾。
    const text = describeContainerFailure(
      {
        code: 'CONTAINER_UNSUPPORTED',
        message: 'server has no supported container runtime (container_runtime=none)',
        requestId: 'r5',
        details: { runtime: 'none' },
        status: 400,
      },
      '',
    );
    expect(text).toContain('本次探测失败，按无容器处理');
    expect(text).toContain('服务器记录未修改');
    expect(text).not.toContain('当前运行时：none');
  });

  it('探测确认写回（库值 none）：保持「当前运行时：none」拼接', () => {
    const text = describeContainerFailure(
      {
        code: 'CONTAINER_UNSUPPORTED',
        message: 'unsupported',
        requestId: 'r6',
        details: { runtime: 'none' },
        status: 400,
      },
      'none',
    );
    expect(text).toContain('当前运行时：none');
    expect(text).not.toContain('本次探测失败');
  });

  it('不传库值（向后兼容）：detail=none 走现值拼接', () => {
    expect(
      describeContainerFailure({
        code: 'CONTAINER_UNSUPPORTED',
        message: 'x',
        requestId: '',
        details: { runtime: 'none' },
        status: 400,
      }),
    ).toContain('当前运行时：none');
  });

  it('未知错误码回退通用 describeFailure 文案（不吞后端语义）', () => {
    expect(
      describeContainerFailure({
        code: 'AUTH_REQUIRED',
        message: 'login required',
        requestId: '',
        details: null,
        status: 401,
      }),
    ).toContain('登录已过期，请重新登录');
  });
});

describe('describeDecisionOutcome（契约 8.7：execution 回填 exit_code/duration/error_code）', () => {
  it('denied：未执行', () => {
    expect(describeDecisionOutcome(makeApproval({ status: 'denied' }), null)).toEqual({
      kind: 'denied',
      message: '已拒绝，命令未执行',
    });
  });

  it('approved + execution exit 0：成功并展示 exit 与时长', () => {
    const outcome = describeDecisionOutcome(makeApproval({ status: 'approved' }), {
      exit_code: 0,
      output: 'abc',
      truncated: false,
      duration_ms: 1200,
    });
    expect(outcome.kind).toBe('success');
    expect(outcome.message).toContain('exit 0');
    expect(outcome.message).toContain('1.20 s');
  });

  it('approved + 执行失败：exit_code 与 error_code（如 SSH 不可达 exit -1）', () => {
    const outcome = describeDecisionOutcome(makeApproval({ status: 'approved' }), {
      exit_code: -1,
      output: '',
      truncated: false,
      duration_ms: 850,
      error_code: 'SSH_EXEC_FAILED',
    });
    expect(outcome.kind).toBe('failed');
    expect(outcome.message).toContain('exit -1');
    expect(outcome.message).toContain('SSH_EXEC_FAILED');
    expect(outcome.message).toContain('850 ms');
  });

  it('approved + 通道失败（exit -1 且无 duration，后端 fail 路径哨兵）：语义文案不展示 -1 与 —', () => {
    // 走查 T4：decide 批准后连接/命令通道失败，后端回填哨兵 exit_code=-1
    // 且不带 duration_ms（executeConsoleApproval 初始值）。JSON 反序列化
    // 后 duration_ms 为 undefined——与运行时真实形状一致。
    const execution = JSON.parse(
      '{"exit_code":-1,"output":"","truncated":false,"error_code":"CONTAINER_CLI_FAILED"}',
    );
    const outcome = describeDecisionOutcome(makeApproval({ status: 'approved' }), execution);
    expect(outcome.kind).toBe('failed');
    expect(outcome.message).toContain('连接或命令通道失败');
    expect(outcome.message).toContain('命令未执行');
    expect(outcome.message).toContain('CONTAINER_CLI_FAILED');
    expect(outcome.message).not.toContain('exit -1');
    expect(outcome.message).not.toContain('—');
  });

  it('approved + execution null（chat 形状兜底）：只报已批准', () => {
    expect(describeDecisionOutcome(makeApproval({ status: 'approved' }), null).message).toBe(
      '已批准',
    );
  });

  it('非终态（decide 前的理论形状）报未生效', () => {
    expect(describeDecisionOutcome(makeApproval({ status: 'pending' }), null).kind).toBe('denied');
  });
});

describe('commandSectionLabel（p4 sprint 5 walk5：命令区标签按阶段取文案）', () => {
  it('confirm：门限预测需审批时预告将申请执行的命令（approval 未创建，传 null）', () => {
    expect(commandSectionLabel('confirm', null, true)).toBe('将申请执行的命令');
  });

  it('confirm：门限预测免审批直执行时标签为「将直接执行的命令」（六期打磨 sprint 6）', () => {
    expect(commandSectionLabel('confirm', null, false)).toBe('将直接执行的命令');
  });

  it('confirm：门限未知时不承诺任一分支（中性「将执行的命令」）', () => {
    expect(commandSectionLabel('confirm', null)).toBe('将执行的命令');
    expect(commandSectionLabel('confirm', null, null)).toBe('将执行的命令');
  });

  it('pending：待执行命令', () => {
    expect(commandSectionLabel('pending', makeApproval({ status: 'pending' }))).toBe('待执行命令');
  });

  it('result 批准路径（含 exit 非 0 的执行失败）：已执行命令', () => {
    expect(commandSectionLabel('result', makeApproval({ status: 'approved' }))).toBe('已执行命令');
  });

  it('result 拒绝路径：命令从未执行，标签为未执行命令（与 outcome 同口径）', () => {
    expect(commandSectionLabel('result', makeApproval({ status: 'denied' }))).toBe('未执行命令');
  });
});

describe('previewContainerCommand（契约 2.9 约定 3：与固化命令同构）', () => {
  it('runtime action cid 空格拼接', () => {
    expect(previewContainerCommand('docker', 'abc123def456', 'stop')).toBe(
      'docker stop abc123def456',
    );
    expect(previewContainerCommand('podman', 'web-1', 'restart')).toBe('podman restart web-1');
  });
});
