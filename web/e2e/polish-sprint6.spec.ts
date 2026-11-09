import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 冲刺 6 打磨项的端到端验证：
//  会话侧栏「新建会话」与每个会话项的「更多」纯图标按钮补 aria-label，
//  屏幕阅读器可读出用途（修复前两者 accessible name 均为空，
//  getByRole('button', { name: ... }) 定位超时）。与同文件搜索框
//  （aria-label="搜索会话"）及 MessageItem 复制按钮的既有写法对齐。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

async function createConversation(page: Page, title: string): Promise<string> {
  const response = await page.request.post('/api/conversations', {
    headers: ORIGIN_HEADERS,
    data: { title },
  });
  expect(response.status()).toBe(201);
  return ((await response.json()) as { id: string }).id;
}

test.describe('冲刺 6 打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('侧栏新建/更多纯图标按钮有可访问名称且可按名称驱动交互', async ({ page }) => {
    test.setTimeout(120_000);

    const title = `e2e冲刺6可访问名称会话${Date.now()}`;
    const id = await createConversation(page, title);
    try {
      await page.goto(`/c/${id}`);
      const item = page.locator('.chat-conv-item', { hasText: title }).first();
      await expect(item).toBeVisible({ timeout: 20_000 });

      // 修复前：两个按钮均无 accessible name，getByRole 按名称定位超时。
      const newButton = page.getByRole('button', { name: '新建会话' });
      await expect(newButton).toBeVisible();

      // 更多按钮的可访问名称带上所属会话名，逐一可区分。
      // 注意会话项容器本身也是 role="button"（ConversationSidebar），
      // 其合成名称包含内部按钮的 aria-label 子串，须 exact 精确匹配。
      const moreButton = page.getByRole('button', { name: `会话操作：${title}`, exact: true });
      await expect(moreButton).toBeVisible();

      // 可访问名称可正常驱动交互：按名称点开更多菜单（重命名入口可见）。
      await item.hover();
      await moreButton.click();
      await expect(page.getByRole('menuitem', { name: '重命名' })).toBeVisible();

      // 点菜单外关闭，再按名称点新建按钮：跳回 /chat 草稿页。
      await newButton.click();
      await expect(page).toHaveURL(/\/chat$/, { timeout: 10_000 });
      await expect(page.getByText('开始新的运维对话')).toBeVisible();
    } finally {
      await page.request.delete(`/api/conversations/${id}`, { headers: ORIGIN_HEADERS });
    }
  });
});
