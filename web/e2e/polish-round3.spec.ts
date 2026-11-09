import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 第 3 轮打磨项的端到端验证：
//  - favicon.svg 随应用提供（浏览器标签页/收藏夹图标）；
//  - 供应商「测试连接」对带 /v1 的 base_url 也能走通（fake-llm 的
//    /models 路由修复，base_url 形如 http://127.0.0.1:18086/v1）；
//  - 未知路径展示 404 页并提供「返回对话」出口，不再静默重定向 /chat。

test.describe('第 3 轮打磨', () => {
  test('favicon.svg 以 image/svg+xml 提供', async ({ request }) => {
    const response = await request.get('/favicon.svg');
    expect(response.status()).toBe(200);
    expect(response.headers()['content-type'] ?? '').toContain('image/svg+xml');
    expect(await response.text()).toContain('<svg');
  });

  test('未登录访问未知路径显示 404，返回对话跳登录并带 next 回跳', async ({ page }) => {
    await page.goto('/this-page-does-not-exist');
    // URL 保持原样（此前会被静默重定向到 /chat，用户毫无感知）。
    await expect(page).toHaveURL(/\/this-page-does-not-exist$/);
    // 冲刺 4 起 404 页传自定义 icon 替换 antd 内置插画（内置插画自带
    // 拼写错误英文「No Found」），antd v6 该场景不再渲染 ant-result-404
    // class；容器 class ant-result 恒在，语义断言不变。
    await expect(page.locator('.ant-result')).toBeVisible();
    await expect(page.getByText('页面不存在或已被移动')).toBeVisible();

    // NotFoundPage 是公开路由；「返回对话」进入受保护路由，未登录时由
    // RequireAuth 跳 /login?next=/chat，登录后仍能回到对话。
    await page.getByRole('button', { name: '返回对话' }).click();
    await expect(page).toHaveURL(/\/login\?next=/);
  });

  test.describe('已登录', () => {
    test.use({ storageState: AUTH_STATE_FILE });

    test('供应商测试连接（base_url 带 /v1）显示连接成功与延迟', async ({ page }) => {
      await page.goto('/providers');
      // global-setup seed 的供应商：base_url http://127.0.0.1:18086/v1。
      // 修复前 fake-llm 对 /v1/models 返回 404，产品把上游 404 报成
      // PROVIDER_TEST_FAILED，「连接成功」分支永远走不到。
      const row = page.locator('tr', { hasText: 'e2e-fake-llm' }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      await row.getByRole('button', { name: /测\s*试/ }).click();

      const dialog = page.getByRole('dialog', { name: /测试连接 · e2e-fake-llm/ });
      await expect(dialog).toBeVisible({ timeout: 20_000 });
      await expect(dialog.getByText('连接成功')).toBeVisible();
      // ConnectionTestModal 的延迟 Tag（formatDuration：<1s 输出「N ms」）。
      await expect(dialog.getByText(/延迟 \d+ ms/)).toBeVisible();
      // 表格状态列 Tag：测试通过 + 延迟。
      await expect(row.getByText(/测试通过 \d+ ms/)).toBeVisible();
    });

    test('已登录访问未知路径：404 页返回对话回到 /chat', async ({ page }) => {
      await page.goto('/no-such-page-e2e');
      await expect(page).toHaveURL(/\/no-such-page-e2e$/);
      // 同上：自定义 icon 后断言 Result 容器而非 ant-result-404 class。
      await expect(page.locator('.ant-result')).toBeVisible();
      await expect(page.getByText('页面不存在或已被移动')).toBeVisible();

      await page.getByRole('button', { name: '返回对话' }).click();
      await expect(page).toHaveURL(/\/chat$/);
    });
  });
});
