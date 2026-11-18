import { describe, expect, it } from 'vitest';

import { formatToolArguments, messageRoleLabel } from './ApprovalContextDrawer';

// RBAC 走查 sprint 2：审批上下文抽屉的纯展示口径。
// 1) 消息角色必须中文（user/assistant），未知值原样保留；
// 2) 工具参数空值与序列化失败必须降级为空串，由调用方渲染「无工具参数」，
//    不能让抽屉因异常数据整块报错。
describe('messageRoleLabel 审批上下文消息角色', () => {
  it('user → 用户，assistant → 助手', () => {
    expect(messageRoleLabel('user')).toBe('用户');
    expect(messageRoleLabel('assistant')).toBe('助手');
  });

  it('未知角色原样保留（不掩盖后端新增角色）', () => {
    expect(messageRoleLabel('system')).toBe('system');
  });
});

describe('formatToolArguments 工具参数格式化', () => {
  it('空对象 / null / undefined 返回空串（用于「无工具参数」提示）', () => {
    expect(formatToolArguments({})).toBe('');
    expect(formatToolArguments(null)).toBe('');
    expect(formatToolArguments(undefined)).toBe('');
  });

  it('非空参数输出两空格缩进 JSON（可复制）', () => {
    expect(formatToolArguments({ command: 'systemctl restart nginx', timeout: 30 })).toBe(
      '{\n  "command": "systemctl restart nginx",\n  "timeout": 30\n}',
    );
  });

  it('序列化失败（循环引用）降级为空串，不抛错', () => {
    const cyclic: Record<string, unknown> = {};
    cyclic['self'] = cyclic;
    expect(formatToolArguments(cyclic)).toBe('');
  });
});
