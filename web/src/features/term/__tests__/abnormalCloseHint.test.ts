import { describe, expect, it } from 'vitest';

// 走查修复（二期冲刺 1）：终端 WS 异常断开时页面必须显示「连接异常断开」
// 提示（契约 6.4）。此前 TermLive 的判断是 `!ended && abnormalClose ===
// true`，而 ended = closedReason !== null || abnormalClose——条件恒假，
// 提示是永不可达的死代码。shouldShowAbnormalCloseHint 锁定正确语义。
import { shouldShowAbnormalCloseHint } from '../termCodes';

describe('shouldShowAbnormalCloseHint', () => {
  it('WS 意外断开（abnormalClose 且无 closed 帧）→ true（回归：旧条件恒假）', () => {
    expect(shouldShowAbnormalCloseHint({ closedReason: null, abnormalClose: true })).toBe(true);
  });

  it('收到服务端 closed 帧 → false（由「会话已结束」提示接管）', () => {
    expect(shouldShowAbnormalCloseHint({ closedReason: 'error', abnormalClose: false })).toBe(
      false,
    );
    // closed 帧先到、随后连接关闭但 abnormalClose 置位同样不提示。
    expect(
      shouldShowAbnormalCloseHint({ closedReason: 'server_shutdown', abnormalClose: true }),
    ).toBe(false);
  });

  it('用户主动断开 → false', () => {
    expect(
      shouldShowAbnormalCloseHint({ closedReason: 'client_closed', abnormalClose: false }),
    ).toBe(false);
  });

  it('连接正常 → false；无状态（null）→ false', () => {
    expect(shouldShowAbnormalCloseHint({ closedReason: null, abnormalClose: false })).toBe(false);
    expect(shouldShowAbnormalCloseHint(null)).toBe(false);
  });
});
