import { describe, expect, it } from 'vitest';

import {
  buildDecideBody,
  canSubmitEditedCommand,
  describeRiskChange,
  EDITED_COMMAND_MAX_LENGTH,
  hasEditedChange,
  validateEditedCommand,
} from './approvalEdit';

// 审批卡「编辑命令」纯逻辑（六期 FE-P1，契约 2.11 约定 3 / 6.3 / 8.7）：
// 校验、decide 请求体构造、风险对照——服务端 400/403 口径的前置镜像。

describe('validateEditedCommand 改写命令校验', () => {
  it('普通命令与边界长度 2000 合法（返回 null）', () => {
    expect(validateEditedCommand('systemctl restart nginx')).toBeNull();
    expect(validateEditedCommand('a'.repeat(EDITED_COMMAND_MAX_LENGTH))).toBeNull();
  });

  it('空串/纯空白拒绝——空草稿曾被当作「未改写」按原命令批准并真实执行（六期打磨 sprint 2）', () => {
    for (const draft of ['', ' ', '   ', '\t', '\n', ' \t\r\n ']) {
      const error = validateEditedCommand(draft);
      expect(error, `draft=${JSON.stringify(draft)}`).not.toBeNull();
      expect(error).toContain('不能为空');
      expect(error).toContain('取消编辑');
    }
  });

  it('含 NUL 拒绝（服务端 strings.ContainsRune(cmd,0) 同款口径）', () => {
    expect(validateEditedCommand('echo a\u0000b')).toContain('NUL');
    expect(validateEditedCommand('rm -rf /tmp/\0')).not.toBeNull();
  });

  it('超过 2000 字符拒绝并给出上限提示', () => {
    const tooLong = 'a'.repeat(EDITED_COMMAND_MAX_LENGTH + 1);
    expect(validateEditedCommand(tooLong)).toContain('2000');
  });

  it('多字节字符按码点计数（2000 个中文合法，2001 个拒绝）', () => {
    expect(validateEditedCommand('中'.repeat(EDITED_COMMAND_MAX_LENGTH))).toBeNull();
    expect(validateEditedCommand('中'.repeat(EDITED_COMMAND_MAX_LENGTH + 1))).not.toBeNull();
  });
});

describe('hasEditedChange 是否发生改写', () => {
  it('逐字节不同 → true', () => {
    expect(hasEditedChange('rm -rf /tmp/x', 'rm -rf /tmp/y')).toBe(true);
  });

  it('完全相同 → false（编辑态预填未改，等价直接批准）', () => {
    expect(hasEditedChange('ls -la', 'ls -la')).toBe(false);
  });

  it('仅首尾空白差异视为未改写（避免无意义的服务端重分类）', () => {
    expect(hasEditedChange('ls -la', '  ls -la  ')).toBe(false);
  });

  it('空白与空串不同（原命令非空时）', () => {
    expect(hasEditedChange('ls -la', '   ')).toBe(true);
    expect(hasEditedChange('ls -la', '')).toBe(true);
  });
});

describe('buildDecideBody decide 请求体构造', () => {
  it('approve + 确有改写 → 携带 edited_command', () => {
    expect(
      buildDecideBody('approve', 'systemctl restart nginx', 'systemctl restart ngingx'),
    ).toEqual({
      decision: 'approve',
      edited_command: 'systemctl restart ngingx',
    });
  });

  it('approve + 未改写（预填原命令）→ 不携带 edited_command（直接批准）', () => {
    expect(buildDecideBody('approve', 'ls -la', 'ls -la')).toEqual({ decision: 'approve' });
    expect(buildDecideBody('approve', 'ls -la', null)).toEqual({ decision: 'approve' });
  });

  it('approve + 空串/纯空白草稿 → 不携带（服务端要求 1..2000 字符）', () => {
    // 注意：该分支只在「非编辑态直接批准」可达。编辑态空草稿由
    // validateEditedCommand / canSubmitEditedCommand 拦下（见下一条断言），
    // 「保存并批准」保持禁用，不会经由本函数发出纯 approve。
    expect(buildDecideBody('approve', 'ls -la', '')).toEqual({ decision: 'approve' });
    expect(buildDecideBody('approve', 'ls -la', '   ')).toEqual({ decision: 'approve' });
    // 六期打磨 sprint 2（走查 high）：空草稿在编辑态恒不可提交——正是这条
    // 前置拦截挡住了「清空命令后仍可保存并批准、按原命令真实执行」。
    expect(canSubmitEditedCommand('', null)).toBe(false);
    expect(canSubmitEditedCommand('   ', 'safe')).toBe(false);
  });

  it('deny 永不携带 edited_command（契约 2.11：deny 携带返回 400；拒绝流程不变）', () => {
    expect(buildDecideBody('deny', 'ls -la', 'rm -rf /tmp/x')).toEqual({ decision: 'deny' });
    expect(buildDecideBody('deny', 'ls -la', null)).toEqual({ decision: 'deny' });
  });
});

describe('describeRiskChange 改写前后风险对照', () => {
  it('safe → needs_approval：风险升级', () => {
    expect(describeRiskChange('safe', 'needs_approval')).toMatchObject({ kind: 'up' });
  });

  it('needs_approval → safe：风险降级', () => {
    expect(describeRiskChange('needs_approval', 'safe')).toMatchObject({ kind: 'down' });
  });

  it('同级：风险不变', () => {
    expect(describeRiskChange('safe', 'safe')).toMatchObject({ kind: 'same' });
    expect(describeRiskChange('needs_approval', 'needs_approval')).toMatchObject({ kind: 'same' });
  });

  it('改写后 forbidden：单独 kind（decide 将被 403 拒绝）', () => {
    const change = describeRiskChange('needs_approval', 'forbidden');
    expect(change.kind).toBe('forbidden');
    expect(change.label).toContain('禁止');
  });
});

describe('canSubmitEditedCommand 「保存并批准」可提交判定', () => {
  it('合法命令且预览通过 → 可提交', () => {
    expect(canSubmitEditedCommand('ls -la', 'safe')).toBe(true);
    expect(canSubmitEditedCommand('ls -la', 'needs_approval')).toBe(true);
  });

  it('预览 forbidden → 禁止提交（服务端 decide 会 403 且审批保持 pending）', () => {
    expect(canSubmitEditedCommand('rm -rf /', 'forbidden')).toBe(false);
  });

  it('校验失败（NUL/超长）→ 禁止提交', () => {
    expect(canSubmitEditedCommand('echo a\u0000b', 'safe')).toBe(false);
    expect(canSubmitEditedCommand('a'.repeat(2001), 'safe')).toBe(false);
  });

  it('预览未返回/失败（null）不拦截——最终裁决在服务端 decide', () => {
    expect(canSubmitEditedCommand('ls -la', null)).toBe(true);
    expect(canSubmitEditedCommand('ls -la', undefined)).toBe(true);
  });
});

// 六期打磨 sprint 3（走查 medium）：编辑命令从「只在对话审批卡可用」扩到
// 容器操作 / 诊断修复 / 批量详情三个入口。三个入口共用同一个
// ApprovalCommandEditor，因此统一复用本文件的校验/预览/请求体纯逻辑——
// 这里补一组「console/batch 场景」的交叉断言，防止日后某入口自行另写一份
// 提交逻辑（空草稿按原命令批准执行是 sprint 2 修复过的 high）。
describe('console/batch 审批编辑（共用 ApprovalCommandEditor 逻辑）', () => {
  it('批量改写 uptime：body 携带 edited_command，执行取改写后命令', () => {
    const body = buildDecideBody('approve', 'systemctl restart nginx', 'uptime');
    expect(body).toEqual({ decision: 'approve', edited_command: 'uptime' });
  });

  it('未改写时与原命令逐字节一致 → 不携带（等价直接批准）', () => {
    expect(buildDecideBody('approve', 'docker restart abc123', 'docker restart abc123')).toEqual({
      decision: 'approve',
    });
  });

  it('空草稿在所有入口都不可提交（跨入口回归护栏）', () => {
    expect(canSubmitEditedCommand('', null)).toBe(false);
    expect(canSubmitEditedCommand('', 'safe')).toBe(false);
    expect(validateEditedCommand('')).toContain('不能为空');
  });

  it('改写为 forbidden 命令在所有入口都不可提交（服务端 403 且审批保持 pending）', () => {
    expect(canSubmitEditedCommand('rm -rf /', 'forbidden')).toBe(false);
    expect(describeRiskChange('needs_approval', 'forbidden').kind).toBe('forbidden');
  });
});
