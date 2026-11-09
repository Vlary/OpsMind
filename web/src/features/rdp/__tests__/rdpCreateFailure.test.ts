import { describe, expect, it } from 'vitest';

// p2 sprint 4 打磨锁定：RDP 创建会话失败的场景化文案。修复前
// RdpPage 对 createError 直接用通用 describeTermFailure，409 只显示
// 「该服务器已有活动 RDP 会话（server already has an active rdp
// session）」；终端同场景已有收尾时序解释（describeTermCreateFailure），
// RDP 未对齐——断开后秒点「新建会话」的用户拿到无引导的报错。
import { makeFailure } from '../../../components/admin/apiError';
import { describeRdpCreateFailure } from '../RdpPage';

describe('describeRdpCreateFailure（RDP_SESSION_EXISTS 场景化文案）', () => {
  it('解释收尾时序并引导稍候重试（对齐终端文案）', () => {
    const text = describeRdpCreateFailure(
      makeFailure('RDP_SESSION_EXISTS', 'server already has an active rdp session'),
    );
    expect(text).toContain('已有活动 RDP 会话');
    expect(text).toContain('若您刚断开上一会话');
    expect(text).toContain('收尾');
    expect(text).toContain('稍候重试');
  });

  it('其他错误码回落通用映射（保留底层原因拼接）', () => {
    expect(describeRdpCreateFailure(makeFailure('RDP_SESSION_LIMIT', 'x'))).toBe(
      '活动 RDP 会话数达到上限（x）',
    );
    expect(describeRdpCreateFailure(makeFailure('GUACD_UNAVAILABLE', 'x'))).toBe(
      'RDP 网关（guacd）未配置或不可达（x）',
    );
    expect(describeRdpCreateFailure(makeFailure('RDP_NO_CREDENTIALS', 'x'))).toBe(
      '服务器未保存可用的密码凭据（x）',
    );
  });
});
