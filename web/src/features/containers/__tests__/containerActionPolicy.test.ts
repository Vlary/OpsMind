// 六期打磨 sprint 6（走查 medium）：容器操作确认弹窗的分流预测单测。
//
// 逐格镜像 `approvals.NeedsApproval`（internal/approvals/policy.go，契约
// 2.11 约定 2）在容器动作输入下的结果：容器动作 start=safe；
// stop/restart=needs_approval 且规则 CONTAINER_CHANGE 的 severity=high
//（不在 7.3E warning 集）。后端 G38 对 24 格有硬断言，这里覆盖本页可达的
// 8 格 + 未知门限。
//
// 仅用于 confirm 态文案预告，不是权限判定——真正分流永远取 action 响应。

import { describe, expect, it } from 'vitest';

import { containerActionNeedsApproval } from '../containerActionPolicy';

describe('containerActionNeedsApproval（契约 2.11 约定 2 矩阵的容器动作切片）', () => {
  it('start（safe）', () => {
    expect(containerActionNeedsApproval('all', true, 'start')).toBe(true);
    expect(containerActionNeedsApproval('all', false, 'start')).toBe(true);
    expect(containerActionNeedsApproval('standard', true, 'start')).toBe(false);
    expect(containerActionNeedsApproval('standard', false, 'start')).toBe(true);
    expect(containerActionNeedsApproval('high', true, 'start')).toBe(false);
    expect(containerActionNeedsApproval('high', false, 'start')).toBe(false);
    expect(containerActionNeedsApproval('none', true, 'start')).toBe(false);
    expect(containerActionNeedsApproval('none', false, 'start')).toBe(false);
  });

  it('stop/restart（needs_approval，severity=high）', () => {
    expect(containerActionNeedsApproval('all', true, 'stop')).toBe(true);
    expect(containerActionNeedsApproval('all', false, 'restart')).toBe(true);
    expect(containerActionNeedsApproval('standard', true, 'stop')).toBe(true);
    expect(containerActionNeedsApproval('standard', false, 'restart')).toBe(true);
    expect(containerActionNeedsApproval('high', true, 'stop')).toBe(true);
    expect(containerActionNeedsApproval('high', false, 'restart')).toBe(true);
    // none 档：needs_approval 免审，除非服务器关闭了「自动执行」硬保护。
    expect(containerActionNeedsApproval('none', true, 'stop')).toBe(false);
    expect(containerActionNeedsApproval('none', true, 'restart')).toBe(false);
    expect(containerActionNeedsApproval('none', false, 'stop')).toBe(true);
    expect(containerActionNeedsApproval('none', false, 'restart')).toBe(true);
  });

  it('门限未知（settings 未加载/失败/非法值）返回 null，调用方必须给中性文案', () => {
    expect(containerActionNeedsApproval(undefined, true, 'stop')).toBeNull();
    expect(containerActionNeedsApproval(null, true, 'stop')).toBeNull();
    expect(containerActionNeedsApproval('', true, 'stop')).toBeNull();
    expect(containerActionNeedsApproval('fast', true, 'stop')).toBeNull();
  });

  it('门限单调性：审批面 all ≥ standard ≥ high ≥ none', () => {
    for (const action of ['start', 'stop', 'restart'] as const) {
      for (const autoExec of [true, false]) {
        const all = containerActionNeedsApproval('all', autoExec, action);
        const standard = containerActionNeedsApproval('standard', autoExec, action);
        const high = containerActionNeedsApproval('high', autoExec, action);
        const none = containerActionNeedsApproval('none', autoExec, action);
        const rank = (v: boolean | null) => (v === true ? 1 : 0);
        expect(rank(all)).toBeGreaterThanOrEqual(rank(standard));
        expect(rank(standard)).toBeGreaterThanOrEqual(rank(high));
        expect(rank(high)).toBeGreaterThanOrEqual(rank(none as boolean | null));
      }
    }
  });
});
