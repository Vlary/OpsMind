import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 第 5 轮打磨项的端到端验证：
//  - 后端断连（代理 502 纯文本）时对话页显示中文「网络异常」提示，
//    不再堆出 TanStack Query 的 "data is undefined" 等英文技术横幅；
//  - 同页多个查询失败聚合为一条横幅；
//  - 错误横幅第一行是中文文案，大写错误码（AUTH_INVALID 等）降级为
//    description 同行小字。

// 模拟反向代理在后端不可达时返回的 502 纯文本（vite dev proxy 的实际行为；
// 不能真的 pkill 共享后端，那会打挂并行 worker 的其他用例）。
// /api/auth/* 放行真实后端：RequireAuth 对 /api/me 的非 401 错误会渲染
// 整页错误而不是放行路由，资源类接口的横幅就测不到了。
async function proxyDown(page: Page) {
  await page.route('**/api/**', (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.startsWith('/api/auth/')) {
      void route.fallback();
      return;
    }
    void route.fulfill({
      status: 502,
      contentType: 'text/plain',
      body: 'Error occurred while trying to proxy: connect ECONNREFUSED 127.0.0.1:8080',
    });
  });
}

test.describe('第 5 轮打磨：后端断连的错误提示', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('代理 502：新会话页显示中文网络异常，无英文技术横幅', async ({ page }) => {
    await proxyDown(page);
    await page.goto('/chat');

    // 修复前：providers/servers/databases 各自挂掉，页面上堆出
    // "UNKNOWN[...] data is undefined" / "Cannot read properties of undefined"
    // 等英文横幅。修复后 servers + databases 聚合为一条，中文提示。
    await expect(page.getByText('2 项数据加载失败：网络异常，请检查连接后重试。')).toBeVisible({
      timeout: 20_000,
    });
    // providers 空态横幅独立于聚合条，同为中文。
    await expect(page.getByText('网络异常，请检查连接后重试。').first()).toBeVisible();
    // 主内容区错误 Alert 恰好两条：聚合 1 + providers 空态 1（侧栏的会话列表
    // 错误条在 .chat-sidebar 内，独立计数，也是中文）。
    expect(await page.locator('.chat-main .ant-alert-error').count()).toBe(2);

    await expect(page.getByText(/data is undefined/)).toHaveCount(0);
    await expect(page.getByText(/Cannot read properties of undefined/)).toHaveCount(0);
  });

  test('代理 502：会话页 4 项上下文查询聚合成一条横幅', async ({ page }) => {
    // 先在正常网络下创建会话，再模拟断连进入会话页
    // （servers/databases/toolCalls/approvals 四个查询 + conversation 全部失败）。
    const created = await page.request.post('/api/conversations', {
      headers: { Origin: 'http://localhost:4173' },
      data: { title: 'round5-断连场景' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };

    await proxyDown(page);
    await page.goto(`/c/${id}`);

    await expect(page.getByText('4 项数据加载失败：网络异常，请检查连接后重试。')).toBeVisible({
      timeout: 20_000,
    });
    // conversation 加载失败在消息区单独一条中文提示（位置语义不同，不参与聚合）。
    await expect(page.getByText('网络异常，请检查连接后重试。').first()).toBeVisible();
    // 主内容区错误 Alert 恰好两条：聚合 1 + 会话区 1（providers 失败只显示
    // 顶栏「重试加载供应商」按钮，不是 Alert；侧栏错误条在 .chat-sidebar 内）。
    expect(await page.locator('.chat-main .ant-alert-error').count()).toBe(2);

    await expect(page.getByText(/data is undefined/)).toHaveCount(0);
    await expect(page.getByText(/Cannot read properties of undefined/)).toHaveCount(0);
  });
});

test.describe('第 5 轮打磨：错误横幅标题中文化', () => {
  test('错误密码登录：标题是中文说明，AUTH_INVALID 降级为小字', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('请输入管理员密码').fill('definitely-wrong-round5');
    await page.getByRole('button', { name: /登\s*录/ }).click();

    // 修复前：Alert 标题是裸码 AUTH_INVALID，中文说明退居第二行。
    const alert = page.locator('.ant-alert-error').filter({ hasText: '管理员密码错误' });
    await expect(alert).toBeVisible({ timeout: 20_000 });
    await expect(alert.locator('.ant-alert-title')).toHaveText(/管理员密码错误/);
    await expect(alert.locator('.ant-alert-title')).not.toHaveText(/AUTH_INVALID/);
    await expect(alert.locator('.ant-alert-description')).toContainText('AUTH_INVALID');
  });
});

test.describe('第 5 轮打磨：连接测试弹窗标题中文化', () => {
  test.use({ storageState: AUTH_STATE_FILE });
  test('服务器连接测试失败：弹窗标题是中文说明，SERVER_TEST_FAILED 降级为小字', async ({
    page,
  }) => {
    await page.goto('/servers');
    // e2e-fake-host 指向 127.0.0.1:1，SSH 立即被拒绝 → SERVER_TEST_FAILED。
    const row = page.locator('tr', { hasText: 'e2e-fake-host' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const dialog = page.getByRole('dialog', { name: /测试连接 · e2e-fake-host/ });
    await expect(dialog).toBeVisible({ timeout: 20_000 });
    const alert = dialog.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });
    await expect(alert.locator('.ant-alert-title')).toHaveText(/SSH 连接测试失败/);
    await expect(alert.locator('.ant-alert-title')).not.toHaveText(/SERVER_TEST_FAILED/);
    await expect(alert.locator('.ant-alert-description')).toContainText('SERVER_TEST_FAILED');
  });
});
