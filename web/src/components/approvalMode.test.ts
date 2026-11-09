import { describe, expect, it } from 'vitest';

import type { components } from '../api/schema';
import {
  APPROVAL_MODE_FOLLOW,
  APPROVAL_MODE_META,
  APPROVAL_MODES,
  approvalModeLabel,
  effectiveModeText,
  followGlobalLabel,
  normalizeApprovalMode,
  toSelectValue,
} from './approvalMode';

// 门限选择器纯逻辑（六期 FE-P1，契约 2.11 约定 1 / 8.2 / 8.7）。

type ApprovalMode = components['schemas']['ApprovalMode'];

describe('APPROVAL_MODES 四档全集', () => {
  it('恰为 all/standard/high/none（契约 3.1 store.ApprovalMode）', () => {
    expect(APPROVAL_MODES).toEqual(['all', 'standard', 'high', 'none']);
  });

  it('每档都有中文 label 与非空说明（8.2：各档说明）', () => {
    for (const mode of APPROVAL_MODES) {
      const meta = APPROVAL_MODE_META[mode];
      expect(meta.value).toBe(mode);
      expect(meta.label.length).toBeGreaterThan(0);
      expect(meta.description.length).toBeGreaterThan(0);
    }
  });

  it('仅 none 为危险档（8.7：免审批红字）', () => {
    expect(APPROVAL_MODE_META.none.danger).toBe(true);
    expect(APPROVAL_MODES.filter((mode) => APPROVAL_MODE_META[mode].danger)).toEqual(['none']);
  });
});

describe('approvalModeLabel 档位文案', () => {
  it('四档各自映射中文', () => {
    expect(approvalModeLabel('all')).toBe('全部审批');
    expect(approvalModeLabel('standard')).toBe('危险命令审批');
    expect(approvalModeLabel('high')).toBe('仅高危审批');
    expect(approvalModeLabel('none')).toBe('免审批');
  });

  it('未知值原样返回（防御后端扩展）', () => {
    expect(approvalModeLabel('ultra')).toBe('ultra');
  });
});

describe('normalizeApprovalMode 取值收敛', () => {
  it('合法四档原样通过', () => {
    for (const mode of APPROVAL_MODES) {
      expect(normalizeApprovalMode(mode)).toBe(mode);
    }
  });

  it('非法/缺失兜底 standard（与后端 approval.mode 键缺失兜底同口径）', () => {
    expect(normalizeApprovalMode('everything')).toBe('standard');
    expect(normalizeApprovalMode(undefined)).toBe('standard');
    expect(normalizeApprovalMode(null)).toBe('standard');
    expect(normalizeApprovalMode(123)).toBe('standard');
  });
});

describe('toSelectValue 会话覆盖下拉映射', () => {
  it('null/undefined（跟随全局）→ follow 哨兵', () => {
    expect(toSelectValue(null)).toBe(APPROVAL_MODE_FOLLOW);
    expect(toSelectValue(undefined)).toBe(APPROVAL_MODE_FOLLOW);
  });

  it('四档覆盖原样返回', () => {
    for (const mode of APPROVAL_MODES) {
      expect(toSelectValue(mode)).toBe(mode);
    }
  });
});

describe('followGlobalLabel 「跟随全局」项文案', () => {
  it('附当前全局档中文（不打开设置页也能看到全局档位）', () => {
    expect(followGlobalLabel('standard')).toBe('跟随全局（当前：危险命令审批）');
    expect(followGlobalLabel('none')).toBe('跟随全局（当前：免审批）');
  });
});

describe('effectiveModeText 生效门限与来源', () => {
  it('会话覆盖非空：显示覆盖档（本会话）', () => {
    expect(effectiveModeText('none', 'none')).toBe('免审批（本会话）');
    expect(effectiveModeText('high', 'high')).toBe('仅高危审批（本会话）');
  });

  it('无覆盖：显示有效档（跟随全局）', () => {
    expect(effectiveModeText(null, 'standard')).toBe('危险命令审批（跟随全局）');
    expect(effectiveModeText(null, 'all')).toBe('全部审批（跟随全局）');
  });

  it('全局改档后 effective 跟随（覆盖为空时来源仍是全局）', () => {
    const afterGlobalChange: ApprovalMode = 'none';
    expect(effectiveModeText(null, afterGlobalChange)).toBe('免审批（跟随全局）');
  });
});

// 六期打磨 sprint 5（走查 low）：免审批档说明与设置页警示曾把数据库字段名
// `auto_exec_enabled=false` 摆进中文句子（用户看不懂字段名）。改为服务器
// 页的开关名「自动执行」，字段名只留代码注释。
describe('免审批档文案不含数据库字段名（p6 sprint 5）', () => {
  it('none 档说明用「关闭了「自动执行」开关」而非字段名', () => {
    const text = APPROVAL_MODE_META.none.description;
    expect(text).toContain('关闭了「自动执行」开关');
    expect(text).not.toContain('auto_exec_enabled');
    expect(text).not.toContain('=false');
  });

  it('四档说明一律不出现 snake_case 字段名与赋值表达式', () => {
    for (const mode of APPROVAL_MODES) {
      const text = APPROVAL_MODE_META[mode].description;
      expect(text, mode).not.toMatch(/[a-z]+_[a-z_]+/);
      expect(text, mode).not.toContain('=');
    }
  });
});
