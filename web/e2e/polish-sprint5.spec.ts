import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 冲刺 5 打磨项的端到端验证：
//  - 会话侧栏搜索只过滤已加载页（首页 limit=200）：搜索无匹配且仍有
//    未加载的更早会话时，空态不再断言「没有匹配的会话」，而是提示
//    「已加载的 N 条会话中没有匹配，加载更早的会话？」并提供联动按钮
//    继续向历史翻页（修复前第 201+ 条会话搜不到且被误报不存在）；
//    全部加载完之后的无匹配仍显示确定空态。
//  另两项打磨（登录页「管理员密码错误」、审计 UUID 提示空格）分别由
//  auth.spec.ts / auth-password.spec.ts 与 audit.spec.ts /
//  frontend-coverage.spec.ts 的既有断言收紧覆盖。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };
const DEEP_TITLE = 'e2e冲刺5深页唯一标题会话';
// 目标会话之后再造 210 个更新的会话：目标至少排在第 211 位，
// 首页（limit=200）必然不含它。
const NOISE_COUNT = 210;
const BATCH = 10;

async function createConversation(page: Page, title: string): Promise<string> {
  const response = await page.request.post('/api/conversations', {
    headers: ORIGIN_HEADERS,
    data: { title },
  });
  expect(response.status()).toBe(201);
  return ((await response.json()) as { id: string }).id;
}

test.describe('冲刺 5 打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('侧栏搜索无匹配且未加载完：提示已加载数量并可联动加载更早的会话', async ({ page }) => {
    test.setTimeout(180_000);

    const targetId = await createConversation(page, DEEP_TITLE);
    const noiseIds: string[] = [];
    for (let start = 1; start <= NOISE_COUNT; start += BATCH) {
      const size = Math.min(BATCH, NOISE_COUNT - start + 1);
      const ids = await Promise.all(
        Array.from({ length: size }, (_, offset) =>
          createConversation(page, `e2e冲刺5噪音会话${String(start + offset).padStart(3, '0')}`),
        ),
      );
      noiseIds.push(...ids);
    }

    try {
      await page.goto('/chat');
      const search = page.getByLabel('搜索会话');
      await expect(search).toBeVisible({ timeout: 20_000 });
      // 首页满页（limit=200）加载完成后再搜索：否则过滤的是部分列表，
      // 空态会短暂走「还没有会话」分支。
      await expect(page.locator('.chat-conv-item')).toHaveCount(200, { timeout: 30_000 });

      // 修复前：目标会话（第 211 位）不在已加载的 200 条内，这里直接
      // 断言「没有匹配的会话」，把真实存在的会话误报为不存在。
      await search.fill(DEEP_TITLE);
      await expect(page.getByText(/已加载的 \d+ 条会话中没有匹配，加载更早的会话？/)).toBeVisible();
      await expect(page.getByText('没有匹配的会话')).toHaveCount(0);
      await expect(page.locator('.chat-conv-item')).toHaveCount(0);
      // 空态联动按钮出现，底部常规翻页按钮让位（同屏仅一个入口）。
      const loadMore = page.getByTestId('search-load-more');
      await expect(loadMore).toBeVisible();
      await expect(page.getByTestId('load-more-conversations')).toHaveCount(0);

      // 联动加载第 2 页：目标会话进入已加载集合，被过滤出来（唯一标题）。
      await loadMore.click();
      await expect(page.locator('.chat-conv-item', { hasText: DEEP_TITLE })).toHaveCount(1, {
        timeout: 30_000,
      });

      // 全部加载完（has_more=false）后再搜不存在的关键词：恢复确定空态，
      // 不再出现联动入口。
      await search.fill('不存在的会话关键词sprint5');
      await expect(page.getByText('没有匹配的会话')).toBeVisible();
      await expect(page.getByTestId('search-load-more')).toHaveCount(0);
    } finally {
      const all = [targetId, ...noiseIds];
      for (let start = 0; start < all.length; start += BATCH) {
        await Promise.all(
          all
            .slice(start, start + BATCH)
            .map((id) =>
              page.request.delete(`/api/conversations/${id}`, { headers: ORIGIN_HEADERS }),
            ),
        );
      }
    }
  });
});
