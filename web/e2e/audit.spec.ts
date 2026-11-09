import { expect, test } from '@playwright/test';
import { AUTH_STATE_FILE } from './helpers';

// 审计页：种子数据（provider/server 创建、登录成功）必须出现在审计列表。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

test('审计页能看到登录与资产创建记录', async ({ page }) => {
  await page.goto('/audit');
  await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();

  const table = page.locator('.ant-table');
  await expect(table).toBeVisible();

  // globalSetup 通过 API 创建了 provider 与 server，并完成过一次登录。
  const rows = table.locator('.ant-table-row');
  await expect(rows.filter({ hasText: '登录成功' }).first()).toBeVisible({ timeout: 20_000 });
  await expect(rows.filter({ hasText: '新增供应商' }).first()).toBeVisible();
  await expect(rows.filter({ hasText: '新增服务器' }).first()).toBeVisible();
});

// 第 3 轮打磨（已知限制 21 修复）：目标 ID 筛选此前不走 uuidFilter，
// 非法 UUID 静默按 200 空结果处理，得到误导性的「没有符合条件的审计记录」。
test('目标 ID 筛选拦截非法 UUID，合法 UUID 正常过滤', async ({ page }) => {
  // 自种审计记录并取真实资源 UUID（POST /api/databases 产生 database.create）。
  const created = await page.request.post('/api/databases', {
    headers: ORIGIN_HEADERS,
    data: {
      name: 'e2e-audit-target-id-uuid',
      engine: 'sqlite',
      sqlite_path: '/nonexistent-audit-target-id/seed.db',
    },
  });
  expect(created.status()).toBe(201);
  const resourceId = ((await created.json()) as { id: string }).id;

  try {
    await page.goto('/audit');
    await expect(page.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

    // 非法 UUID：前端轻校验拦下（提示 + 不发请求）。
    let leakedBadRequest = false;
    page.on('request', (request) => {
      if (request.url().includes('resource_id=not-an-uuid')) {
        leakedBadRequest = true;
      }
    });
    const search = page.getByPlaceholder('目标 ID（资源 UUID）');
    await search.fill('not-an-uuid');
    // toolbar 第 1 个「筛选」按钮属于目标 ID 搜索框（其后是会话/服务器 ID）。
    await page
      .locator('.opsmind-admin-toolbar')
      .getByRole('button', { name: /筛\s*选/ })
      .nth(0)
      .click();
    // 冲刺 5：中英文之间补空格，精确断言「目标 ID 必须是合法的 UUID」
    //（修复前是「目标 ID必须是合法的 UUID」，label 与「必须」连写）。
    await expect(page.locator('.ant-message').getByText('目标 ID 必须是合法的 UUID')).toBeVisible({
      timeout: 10_000,
    });
    // 提示出现后确认非法参数从未发出（拦截发生在请求前）。
    await page.waitForTimeout(1_500);
    expect(leakedBadRequest).toBe(false);

    // 合法 UUID：筛出该资源的创建记录。
    await search.fill(resourceId);
    await page
      .locator('.opsmind-admin-toolbar')
      .getByRole('button', { name: /筛\s*选/ })
      .nth(0)
      .click();
    const rows = page.locator('.ant-table-row');
    await expect(rows.first()).toBeVisible({ timeout: 20_000 });
    expect(await rows.count()).toBeGreaterThanOrEqual(1);
    await expect(rows.first()).toContainText('新增数据库');
  } finally {
    await page.request.delete(`/api/databases/${resourceId}`, { headers: ORIGIN_HEADERS });
  }
});
