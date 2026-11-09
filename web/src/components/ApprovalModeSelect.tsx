import { Select, Tag, Tooltip } from 'antd';
import type { CSSProperties, ReactNode } from 'react';

import type { components } from '../api/schema';
import {
  APPROVAL_MODE_FOLLOW,
  APPROVAL_MODE_META,
  APPROVAL_MODES,
  followGlobalLabel,
  toSelectValue,
} from './approvalMode';

// CONTRACT.md 2.11 约定 1 / 8.2：门限四档选择器——全局（/settings，PUT
// /api/settings）与会话覆盖（ChatPage 头部，PATCH /api/conversations/:id，
// 含「跟随全局」项）共用。档位元数据与取值映射见 approvalMode.ts。

type ApprovalMode = components['schemas']['ApprovalMode'];
type ApprovalModeChoice = ApprovalMode | typeof APPROVAL_MODE_FOLLOW;

interface ModeOption {
  value: ApprovalModeChoice;
  label: ReactNode;
  /** 下拉项 hover 的完整说明（Tooltip）。 */
  description: string;
}

export interface ApprovalModeSelectProps {
  /** 当前档位；会话侧传 null 表示「跟随全局」。 */
  value: ApprovalMode | null;
  /** null 仅在 allowFollowGlobal 时回调（PATCH approval_mode:null 清除覆盖）。 */
  onChange: (mode: ApprovalMode | null) => void;
  /** 会话覆盖侧为 true：下拉含「跟随全局」项。 */
  allowFollowGlobal?: boolean;
  /** 会话侧传入：「跟随全局」项展示当前全局档。 */
  globalMode?: ApprovalMode | undefined;
  size?: 'small' | 'middle' | 'large';
  disabled?: boolean;
  style?: CSSProperties;
  /**
   * 下拉的可访问名（六期打磨 sprint 3 走查 low）。antd Select 不会从相邻
   * 文本推断名称——此前两处使用点的 combobox aria-label/aria-labelledby
   * 全为 null，屏幕阅读器与键盘用户无法得知这是「审批门限」。全局侧
   * 「审批门限」、会话侧「会话审批门限」。
   */
  ariaLabel?: string;
}

/**
 * 门限档位 Tag（8.7：审批卡 approval_mode 徽标 / 生效门限展示用；none 档红色）。
 */
export function ApprovalModeTag({ mode }: { mode: ApprovalMode }) {
  const meta = APPROVAL_MODE_META[mode];
  return (
    <Tag color={meta.danger ? 'red' : 'default'}>
      {meta.danger ? <span className="approval-mode-danger-text">{meta.label}</span> : meta.label}
    </Tag>
  );
}

/**
 * antd Select 形态的门限选择器（契约 8.2）。
 *
 * 说明的呈现：每档 option 的 label 为 ReactNode（danger 档红字，回显同款），
 * 下拉项外层包 Tooltip——ChatPage 头部空间紧张，完整说明不铺进选项文本，
 * hover 选项时浮出（契约「各档说明 Tooltip」）。
 */
export default function ApprovalModeSelect({
  value,
  onChange,
  allowFollowGlobal = false,
  globalMode,
  size = 'middle',
  disabled = false,
  style,
  ariaLabel = '审批门限',
}: ApprovalModeSelectProps) {
  const options: ModeOption[] = [];
  if (allowFollowGlobal) {
    options.push({
      value: APPROVAL_MODE_FOLLOW,
      label: followGlobalLabel(globalMode ?? 'standard'),
      description: '本会话不单独设置，使用全局审批门限（/settings 可修改）。',
    });
  }
  for (const mode of APPROVAL_MODES) {
    const meta = APPROVAL_MODE_META[mode];
    options.push({
      value: mode,
      label: meta.danger ? (
        <span className="approval-mode-danger-text">{meta.label}</span>
      ) : (
        meta.label
      ),
      description: meta.description,
    });
  }

  return (
    <Select<ApprovalModeChoice>
      size={size}
      value={toSelectValue(value)}
      options={options}
      disabled={disabled}
      aria-label={ariaLabel}
      style={{ minWidth: 160, ...style }}
      virtual={false}
      onChange={(choice) => onChange(choice === APPROVAL_MODE_FOLLOW ? null : choice)}
      optionRender={(option) => (
        <Tooltip title={option.data['description'] as string} placement="right">
          <div className="approval-mode-option">{option.label}</div>
        </Tooltip>
      )}
    />
  );
}
