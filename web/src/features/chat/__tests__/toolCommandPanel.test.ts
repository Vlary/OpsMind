// 六期打磨 sprint 5（走查 low）：命令改写后工具卡的命令面板。
//
// 实测缺陷：把 `rm -rf /tmp/x` 改写为 uptime 并批准后，`.chat-toolcall-card`
// 首屏仍是「ssh_exec 成功 需审批 危险命令审批 高风险」+ 命令面板里的
// `rm -rf /tmp/x`；结果面板 JSON 里才是 command=uptime。根因是
// ToolCallCard 只读 arguments.command（契约 2.11 约定 3 有意保留原参数）。

import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { commandPanelOf, editedCommandOfApprovals } from '../toolCommandPanel';

type Approval = components['schemas']['Approval'];

function approval(overrides: Partial<Approval> = {}): Approval {
  return {
    id: 'ap-1',
    conversation_id: 'c1',
    tool_call_id: 'tc-1',
    source: 'chat',
    server_id: 's1',
    server_name: 'web-1',
    command: 'rm -rf /tmp/x',
    risk_level: 'needs_approval',
    reason: 'dangerous',
    status: 'approved',
    approval_mode: 'standard',
    requested_at: '2026-11-08T10:00:00Z',
    expires_at: '2026-11-08T10:02:00Z',
    decided_at: '2026-11-08T10:01:00Z',
    decided_by: 'u1',
    note: '',
    edited_command: '',
    edited_risk_level: '',
    edited_rule_id: '',
    ...overrides,
  };
}

describe('工具卡命令面板（p6 sprint 5）', () => {
  it('未改写：单行原命令，标签不变（不引入「模型原命令」）', () => {
    const panel = commandPanelOf({ arguments: { command: 'df -h' }, result: null }, [
      approval({ command: 'df -h' }),
    ]);
    expect(panel).toEqual({
      label: '命令',
      text: 'df -h',
      language: 'bash',
      executed: null,
      edited: false,
    });
  });

  it('用户改写：标题改「模型原命令」，另给实际执行命令', () => {
    const panel = commandPanelOf(
      { arguments: { command: 'rm -rf /tmp/x' }, result: { command: 'uptime' } },
      [approval({ edited_command: 'uptime', edited_risk_level: 'safe' })],
    );
    expect(panel).toEqual({
      label: '模型原命令',
      text: 'rm -rf /tmp/x',
      language: 'bash',
      executed: 'uptime',
      edited: true,
    });
  });

  it('无审批但结果命令与参数不同：仍显示实际执行（旧会话兜底）', () => {
    const panel = commandPanelOf(
      { arguments: { command: 'rm -rf /tmp/x' }, result: { command: 'uptime' } },
      [],
    );
    expect(panel?.executed).toBe('uptime');
    expect(panel?.edited).toBe(true);
    expect(panel?.label).toBe('模型原命令');
  });

  it('SQL 面板：改写后标题为「模型原 SQL」', () => {
    const panel = commandPanelOf(
      { arguments: { sql: 'select * from t limit 5000' }, result: null },
      [approval({ command: 'select * from t limit 5000', edited_command: 'select * from t limit 10' })],
    );
    expect(panel?.label).toBe('模型原 SQL');
    expect(panel?.language).toBe('sql');
    expect(panel?.executed).toBe('select * from t limit 10');
  });

  it('结果命令与参数相同（未改写直执行）：不加实际执行行', () => {
    const panel = commandPanelOf(
      { arguments: { command: 'uptime' }, result: { command: 'uptime' } },
      [],
    );
    expect(panel?.executed).toBeNull();
    expect(panel?.edited).toBe(false);
    expect(panel?.label).toBe('命令');
  });

  it('无命令类参数（如 list_servers）：不渲染命令面板', () => {
    expect(commandPanelOf({ arguments: { limit: 10 }, result: null }, [])).toBeNull();
  });

  it('多条审批取最后一条的改写（历史重试场景）', () => {
    const approvals = [
      approval({ id: 'ap-old', edited_command: 'echo old' }),
      approval({ id: 'ap-new', edited_command: 'uptime' }),
    ];
    expect(editedCommandOfApprovals(approvals)).toBe('uptime');
    expect(editedCommandOfApprovals([])).toBeNull();
    expect(editedCommandOfApprovals([approval()])).toBeNull();
  });
});
