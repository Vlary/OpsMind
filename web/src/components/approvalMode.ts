// 审批门限四档的纯逻辑（六期 FE-P1，契约 2.11 约定 1 / 8.2 / 8.7）。
// 与 ApprovalModeSelect 组件分离：组件文件只导出组件（react-refresh），
// 这里承载档位元数据与取值映射，供组件、审批卡、ChatPage 与单测共用。

import type { components } from '../api/schema';

type ApprovalMode = components['schemas']['ApprovalMode'];

/** 会话覆盖下拉中「跟随全局」的哨兵值（提交时映射回 null 清除覆盖）。 */
export const APPROVAL_MODE_FOLLOW = 'follow' as const;

/** 四档全集（契约 3.1 store.ApprovalMode）。 */
export const APPROVAL_MODES: readonly ApprovalMode[] = ['all', 'standard', 'high', 'none'];

export interface ApprovalModeMeta {
  value: ApprovalMode;
  label: string;
  /** 下拉项 Tooltip 说明（契约 8.2：各档说明）。 */
  description: string;
  /** none 档为危险档（8.7：红字展示）。 */
  danger: boolean;
}

export const APPROVAL_MODE_META: Record<ApprovalMode, ApprovalModeMeta> = {
  all: {
    value: 'all',
    label: '全部审批',
    description: '所有命令（含只读命令）都需人工批准后才执行。',
    danger: false,
  },
  standard: {
    value: 'standard',
    label: '危险命令审批',
    description: '危险命令需人工批准，只读命令免审直接执行（默认档，与一至五期行为一致）。',
    danger: false,
  },
  high: {
    value: 'high',
    label: '仅高危审批',
    description: '仅高危命令需批准；warning 级危险命令与只读命令免审直接执行。',
    danger: false,
  },
  none: {
    value: 'none',
    label: '免审批',
    description:
      // 六期打磨 sprint 5（走查 low）：此处与 SettingsPage 的免审批警示
      // 曾把数据库字段名 `auto_exec_enabled=false` 摆进中文句子。字段名
      // 只留在代码注释里：auto_exec_enabled=false 即「关闭了『自动执行』
      // 开关」的服务器（ServersPage 列头/ServerFormDrawer 的开关名）。
      '所有命令自动执行（含高危），仅保留审计留痕；关闭了「自动执行」开关的服务器仍会拦截非只读命令。',
    danger: true,
  },
};

/** 档位中文文案；未知值原样返回（防御后端扩展新档位时不渲染空白）。 */
export function approvalModeLabel(mode: string): string {
  const hit = APPROVAL_MODES.find((item) => item === mode);
  return hit ? APPROVAL_MODE_META[hit].label : mode;
}

/** 任意输入收敛为合法四档（非法/缺失兜底 standard，与契约 2.11 约定 1 后端口径一致）。 */
export function normalizeApprovalMode(value: unknown): ApprovalMode {
  return APPROVAL_MODES.find((item) => item === value) ?? 'standard';
}

/**
 * 会话覆盖下拉的选中值映射：null/undefined（跟随全局）→ 'follow' 哨兵；
 * 否则原样返回四档值。全局侧不使用（四档值直传）。
 */
export function toSelectValue(override: ApprovalMode | null | undefined): ApprovalMode | typeof APPROVAL_MODE_FOLLOW {
  return override ?? APPROVAL_MODE_FOLLOW;
}

/** 「跟随全局」项文案：附当前生效的全局门限（不打开设置页也能看到全局档位）。 */
export function followGlobalLabel(globalMode: ApprovalMode): string {
  return `跟随全局（当前：${approvalModeLabel(globalMode)}）`;
}

/** 生效门限与来源（任务：对话页头部显示生效门限与来源）。 */
export function effectiveModeText(
  override: ApprovalMode | null,
  effective: ApprovalMode,
): string {
  return override
    ? `${approvalModeLabel(override)}（本会话）`
    : `${approvalModeLabel(effective)}（跟随全局）`;
}
