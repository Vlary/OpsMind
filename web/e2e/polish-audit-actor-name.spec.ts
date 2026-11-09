import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 本轮打磨：审计页操作者列可读性。
//  - GET /api/audit 为 actor_type=user 的记录装配 actor_name；
//  - 操作者主行显示「管理员 admin」，完整 actor UUID 移入已有 Tooltip 副行；
//  - 修复前主行是「用户」+ 副行 UUID 片段（如 3dc0518e-773f-），整页无用户名。

const PREVIEW_ORIGIN = 'http://localhost:4173';

interface AuditItem {
  actor_type: 'user' | 'system' | 'model';
  actor_id: string;
  actor_name: string | null;
  action: string;
}

async function fetchAudit(page: import('@playwright/test').Page, query: string) {
  const resp = await page.request.get(`/api/audit${query}`, {
    headers: { Origin: PREVIEW_ORIGIN },
  });
  expect(resp.status()).toBe(200);
  return ((await resp.json()) as { items: AuditItem[] }).items;
}

test.describe('审计页操作者显示用户名', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('user 记录主行显示「管理员 admin」，完整 UUID 在 Tooltip', async ({ page }) => {
    // 审计是异步批量写入（500ms 批次）：轮询等种子登录记录落库后再断言。
    // 用 action 过滤查询，避免并行测试写入的记录把登录记录挤出首页。
    const deadline = Date.now() + 15_000;
    let items: AuditItem[] = [];
    while (Date.now() < deadline) {
      items = await fetchAudit(page, '?action=auth.login_succeeded&limit=50');
      if (items.length > 0) {
        break;
      }
      await page.waitForTimeout(300);
    }
    // 后端契约：user 登录记录必须装配 actor_name（单管系统恒为 admin）。
    expect(items.length).toBeGreaterThan(0);
    for (const item of items) {
      expect(item.actor_type).toBe('user');
      expect(item.actor_name).toBe('admin');
    }
    const loginItem = items[0];
    expect(loginItem.actor_id).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );

    // 认证前事件（login_failed）发生在会话建立前，无 actor_id 也就无用户名。
    const failed = await fetchAudit(page, '?action=auth.login_failed&limit=10');
    for (const item of failed) {
      expect(item.actor_name, `${item.action} actor_name`).toBeNull();
    }

    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();
    const table = page.locator('.ant-table');
    await expect(table).toBeVisible();

    // 并行测试可能把登录记录挤出第一页：用「按动作筛选」收窄到登录成功。
    // placeholder 搜索输入会拦截点击，按既有用例的模式键盘过滤 + Enter 选中。
    await page.locator('.opsmind-admin-toolbar .ant-select').first().click();
    await page.keyboard.type('登录成功');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(page.locator('.opsmind-admin-toolbar .ant-select').first()).toContainText(
      '登录成功',
    );

    const row = table.locator('.ant-table-row').filter({ hasText: '登录成功' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });

    // 操作者列是行内唯一含「管理员」的单元格；单元格内结构是
    // td > div(容器) > div(主行) + div.opsmind-admin-cell-sub(UUID 副行)，
    // div > div 的第一个命中即主行，精确断言其文本为「管理员 admin」
    // （修复前是「用户」且整页没有可读用户名）。
    const actorCell = row.locator('td', { hasText: /管理员/ }).first();
    await expect(actorCell).toBeVisible();
    await expect(actorCell.locator('div > div').first()).toHaveText('管理员 admin');

    // 完整 actor UUID 移入 Tooltip：悬停副行短 ID 后提示完整 UUID。
    await actorCell.locator('.opsmind-admin-cell-sub').hover();
    const tooltip = page.locator('.ant-tooltip').filter({ hasText: loginItem.actor_id });
    await expect(tooltip).toBeVisible();
  });
});
