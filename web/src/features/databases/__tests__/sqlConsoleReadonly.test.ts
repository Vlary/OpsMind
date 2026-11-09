// p3 sprint 4：MongoDB JSON 查询因参数超白名单上限（如 limit 5000 > 1000）
// 被拒（422 SQL_NOT_READONLY / MONGO_PARAM_NOT_WHITELISTED，走查 W3b）时，
// 通用警示描述只引导「如确需变更数据，请在对话中提出操作请求」——与场景
// 跑偏（用户并无写数据意图）。文案补充参数类拒绝说明（导出常量在此回归）。
import { describe, expect, it } from 'vitest';

import { READONLY_REJECTION_GUIDE } from '../SqlConsole';

describe('READONLY_REJECTION_GUIDE 只读拒绝警示文案（p3 sprint 4）', () => {
  it('描述包含参数超白名单同样拒绝的说明（补句不丢）', () => {
    expect(READONLY_REJECTION_GUIDE.description).toContain('参数超出白名单限制的查询同样以此拒绝');
  });

  it('写操作引导句保留（只读告警语义不削弱）', () => {
    expect(READONLY_REJECTION_GUIDE.message).toBe('本系统只执行只读查询，写操作不会执行');
    expect(READONLY_REJECTION_GUIDE.description).toContain(
      '如确需变更数据，请在对话中提出操作请求',
    );
  });

  it('警示仍指向对话审批通道（引导链路完整）', () => {
    expect(READONLY_REJECTION_GUIDE.description).toContain('由审批流程确认后执行');
  });
});
