import { describe, expect, it } from 'vitest';

import type { components } from '../../../api/schema';
import { commandCellOf, executedCommandOf } from '../batchView';

type BatchResult = components['schemas']['BatchResult'];

function result(overrides: Partial<BatchResult> = {}): Pick<BatchResult, 'edited_command'> {
  return { edited_command: '', ...overrides };
}

// 六期打磨 sprint 5（走查 medium）：批量详情「命令」列必须显示实际执行的
// 命令——用户在审批行改写为 uptime 并批准后，此前抽屉只显示 job 原文
// `rm -rf …`，与同刻 API 的 output / approval.edited_command 背离。
describe('批量结果的实际执行命令（p6 sprint 5）', () => {
  it('未改写：命令回落 job 原文，不带改写标记', () => {
    expect(executedCommandOf('df -h', result())).toEqual({
      command: 'df -h',
      edited: false,
    });
  });

  it('用户改写：命令取 edited_command 并标记改写', () => {
    expect(executedCommandOf('rm -rf /tmp/x', result({ edited_command: 'uptime' }))).toEqual({
      command: 'uptime',
      edited: true,
    });
  });

  it('命令列前缀：改写行为「实际执行」，未改写行用传入前缀', () => {
    expect(commandCellOf('rm -rf /tmp/x', result({ edited_command: 'uptime' }))).toEqual({
      command: 'uptime',
      edited: true,
      prefix: '实际执行',
    });
    expect(commandCellOf('df -h', result())).toEqual({
      command: 'df -h',
      edited: false,
      prefix: '命令',
    });
    expect(commandCellOf('df -h', result(), '下发的命令').prefix).toBe('下发的命令');
  });

  it('改写命令与原文相同时仍算改写（服务端 trimmed 非空即落库）', () => {
    // 服务端只在 hasEditedChange 为真时提交 edited_command；这里模拟旧数据/
    // 直接 PUT 的边界：非空即视为改写，展示以 edited_command 为准。
    expect(executedCommandOf('df -h', result({ edited_command: 'df -h' })).edited).toBe(true);
  });
});
