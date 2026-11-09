import { describe, expect, it } from 'vitest';

import { describeRiskRule } from './riskRules';

describe('describeRiskRule', () => {
  it('常见审批规则返回中文说明', () => {
    expect(describeRiskRule('RM_ANY')).toBe('删除文件');
    expect(describeRiskRule('RM_PROTECTED')).toContain('递归');
    expect(describeRiskRule('FILE_WRITE')).toContain('写入');
    expect(describeRiskRule('DEFAULT_ASK')).toContain('人工确认');
  });

  it('未知规则 ID 与空值返回 null（调用方回退原始 reason）', () => {
    expect(describeRiskRule('SOMETHING_NEW')).toBeNull();
    expect(describeRiskRule('')).toBeNull();
    expect(describeRiskRule(undefined)).toBeNull();
    expect(describeRiskRule(null)).toBeNull();
  });
});
