// 六期打磨 sprint 5（走查 low）：命令改写后的工具卡命令面板口径。
//
// 背景：`chat` 审批把 `rm -rf /tmp/x` 改写为 `uptime` 并批准后，工具卡首屏
// 仍是「ssh_exec 成功 需审批 危险命令审批 高风险」+ 命令面板里的
// `rm -rf /tmp/x`（红字高风险）——实际执行的是 uptime，只有下方的审批卡
// 才体现改写。契约 2.11 约定 3 有意保留 arguments.command（模型原始参数），
// 因此这里不改 arguments，而是推导「实际执行的命令」并在面板里补一行。
//
// 纯函数放在组件外（react-refresh 要求组件文件只导出组件），单测共用。

import type { components } from '../../api/schema';

type Approval = components['schemas']['Approval'];

export interface ToolCommandInput {
  arguments: Record<string, unknown>;
  /** 工具结果（runSSHExec 的 result.command 是实际执行命令）。 */
  result?: Record<string, unknown> | null;
}

export interface CommandPanel {
  /** 面板标题：改写时「模型原命令 / 模型原 SQL」，否则「命令 / SQL」。 */
  label: string;
  /** 面板正文：模型原始命令（改写过）或唯一命令。 */
  text: string;
  language: string;
  /** 实际执行命令（仅改写时非空；未改写为 null）。 */
  executed: string | null;
  /** 是否由用户改写（UI 据此加「用户改写」Tag）。 */
  edited: boolean;
}

/** 命令面板的原始命令与语言（arguments.command 优先，其次 arguments.sql）。 */
function rawCommandOf(arguments_: Record<string, unknown>): {
  raw: string;
  language: string;
  baseLabel: string;
} {
  const command = arguments_['command'];
  if (typeof command === 'string' && command.length > 0) {
    return { raw: command, language: 'bash', baseLabel: '命令' };
  }
  const sql = arguments_['sql'];
  if (typeof sql === 'string' && sql.length > 0) {
    return { raw: sql, language: 'sql', baseLabel: 'SQL' };
  }
  return { raw: '', language: 'bash', baseLabel: '命令' };
}

/**
 * 工具卡命令面板。
 *
 * 实际执行命令的来源优先级：
 *  1. 关联审批的 `edited_command`（改写后命令，与审批卡同口径）；审批未
 *     改写时执行的就是原命令，不产生「实际执行」行；
 *  2. 工具结果里的 `command`（后端 runSSHExec 的 result.command 即实际
 *     执行命令；审批行已被清理的旧会话靠它兜底）；
 * 与模型原始命令相同（或都为空）→ 未改写，面板保持既有单行渲染。
 *
 * `arguments` 原值永远保留：它就是「模型原命令」，是审计与排障的事实。
 */
export function commandPanelOf(
  view: ToolCommandInput,
  approvals: readonly Approval[],
): CommandPanel | null {
  const { raw, language, baseLabel } = rawCommandOf(view.arguments);
  const resultCommand =
    view.result && typeof view.result['command'] === 'string'
      ? (view.result['command'] as string)
      : '';

  const editedFromApproval = editedCommandOfApprovals(approvals);
  let executed: string | null = null;
  if (editedFromApproval !== null && editedFromApproval !== raw) {
    executed = editedFromApproval;
  } else if (editedFromApproval === null && resultCommand !== '' && resultCommand !== raw) {
    // 无审批改写线索（旧会话）但结果里的命令与模型参数不同：同样是「实际
    // 执行的命令」。不确定来源时保守标记为改写——展示两次命令比谎报一次好。
    executed = resultCommand;
  }

  if (executed === null) {
    if (raw === '') {
      return null;
    }
    return { label: baseLabel, text: raw, language, executed: null, edited: false };
  }
  return {
    label: baseLabel === 'SQL' ? '模型原 SQL' : '模型原命令',
    text: raw,
    language,
    executed,
    edited: true,
  };
}

/**
 * 关联审批里用户改写的命令（无改写返回 null）。
 *
 * 同一 tool_call 可能有多条审批（历史重试），优先取最后一条的
 * edited_command。
 */
export function editedCommandOfApprovals(approvals: readonly Approval[]): string | null {
  for (let i = approvals.length - 1; i >= 0; i -= 1) {
    const edited = approvals[i]?.edited_command;
    if (edited) {
      return edited;
    }
  }
  return null;
}
