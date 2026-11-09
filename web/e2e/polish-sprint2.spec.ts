import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 冲刺 2 打磨项的端到端验证：
//  - 会话列表游标分页：会话超过 200 个后，侧栏首屏只显示最近 200 个，
//    通过「加载更早的会话」按钮向历史翻页，旧会话不再静默不可达。
//
// 登录限流提示去英文（AUTH_RATE_LIMITED + "too many login attempts"）
// 属纯文案映射，由 apiError.test.ts 单测覆盖；不在 E2E 触发限流——
// 登录限流为按 IP 令牌桶 5 次/12s（playwright.config.ts 注释），触发后
// 会让并行 worker 的登录用例连带 429。

const ORIGIN = 'http://localhost:4173';
const PREFIX = 'sprint2-pagination-';
const SEED_COUNT = 205;

test.describe('冲刺 2 打磨：会话列表游标分页', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('超过 200 个会话：首屏 200 条，「加载更早的会话」补齐历史', async ({ page }) => {
    // 通过 API 并发 seed 205 个会话（本 spec 独有前缀，结束时清理）。
    const responses = await Promise.all(
      Array.from({ length: SEED_COUNT }, (_, i) =>
        page.request.post('/api/conversations', {
          headers: { Origin: ORIGIN },
          data: { title: `${PREFIX}${String(i).padStart(3, '0')}` },
        }),
      ),
    );
    const created: string[] = [];
    for (const res of responses) {
      expect(res.status()).toBe(201);
      created.push((await res.json()).id as string);
    }

    try {
      await page.goto('/chat');
      // 首屏：总数已超过 200，侧栏只渲染最近 200 条（旧契约每页上限）。
      await expect(page.locator('.chat-conv-item')).toHaveCount(200, { timeout: 20_000 });

      // 分页入口存在；修复前侧栏无任何加载更多按钮。
      const loadMore = page.getByTestId('load-more-conversations');
      await expect(loadMore).toBeVisible();

      // 翻页：剩余会话（含最旧的 seed）全部可达。
      await loadMore.click();
      await expect(page.locator('.chat-conv-item').first()).toBeVisible();
      const total = await page.locator('.chat-conv-item').count();
      expect(total).toBeGreaterThan(200);
      // seed 的 205 个会话全部可见（首屏 + 一页即可补齐）。
      await expect(page.locator('.chat-conv-item', { hasText: PREFIX })).toHaveCount(SEED_COUNT);

      // 没有更多页时按钮消失。
      await expect(loadMore).toBeHidden();
    } finally {
      // 清理 seed，避免污染共享 e2e 库的其他侧栏断言。
      await Promise.all(
        created.map((id) =>
          page.request.delete(`/api/conversations/${id}`, { headers: { Origin: ORIGIN } }),
        ),
      );
    }
  });
});
