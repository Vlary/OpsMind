import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 冲刺 9 打磨项的端到端验证：SQL 查询抽屉里语句为空时点击
// 「执行查询」原本静默无响应（无提示、无禁用）。修复后：
//  1) 空语句时执行按钮禁用（disabled={!sql.trim()}）；
//  2) Ctrl/⌘+Enter 键盘路径仍可触发 run()，空语句改为
//     message.warning('请输入要执行的 SQL')，且不发起查询请求；
//  3) 有语句时按钮恢复可用、只读查询正常执行（安全逻辑不变）。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

/** 建一个指向 e2e postgres 的数据库资产（唯一名，天然幂等）。 */
async function createPgDatabase(page: Page, name: string): Promise<void> {
  const created = await page.request.post('/api/databases', {
    headers: ORIGIN_HEADERS,
    data: {
      name,
      engine: 'postgres',
      host: '127.0.0.1',
      port: 65432,
      database_name: 'aiops_e2e',
      username: 'aiops',
      password: 'aiops',
    },
  });
  expect(created.status()).toBe(201);
}

/** 按名称删除数据库资产（存在才删，幂等）。 */
async function deleteDatabaseByName(page: Page, name: string): Promise<void> {
  const list = await page.request.get('/api/databases');
  const payload = (await list.json()) as { items: Array<{ id: string; name: string }> };
  const created = payload.items.find((item) => item.name === name);
  if (created) {
    const removed = await page.request.delete(`/api/databases/${created.id}`, {
      headers: ORIGIN_HEADERS,
    });
    expect(removed.status()).toBeLessThan(300);
  }
}

test.describe('冲刺 9 打磨：SQL 抽屉空语句反馈', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('空语句时按钮禁用、Ctrl+Enter 弹提示且不发请求；恢复语句后可正常执行', async ({ page }) => {
    test.setTimeout(120_000);

    const dbName = 'e2e-sprint9-sql-empty';
    await createPgDatabase(page, dbName);

    // 记录发往查询接口的请求（证明空语句不发起调用）。
    const queryRequests: string[] = [];
    page.on('request', (request) => {
      if (/\/api\/databases\/[^/]+\/query/.test(request.url())) {
        queryRequests.push(request.url());
      }
    });

    try {
      await page.goto('/databases');
      await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill(dbName);
      const row = page.locator('tr', { hasText: dbName }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      await row.getByRole('button', { name: /查询/ }).click();

      const drawer = page.getByRole('dialog', { name: `SQL 查询 · ${dbName}` });
      await expect(drawer).toBeVisible();

      const runButton = drawer.getByRole('button', { name: '执行查询' });
      const editor = drawer.locator('.opsmind-sql-editor');

      // 预置语句 'SELECT 1;'：按钮可用。
      await expect(editor).toHaveValue('SELECT 1;');
      await expect(runButton).toBeEnabled();

      // 语句清为空白：按钮转为禁用（修复前可点但点击后静默无响应）。
      await editor.fill('');
      await expect(runButton).toBeDisabled();

      // 键盘路径（Ctrl+Enter）不受按钮禁用影响：给出明确中文提示。
      await editor.press('Control+Enter');
      await expect(page.locator('.ant-message').getByText('请输入要执行的 SQL')).toBeVisible({
        timeout: 10_000,
      });

      // 空语句不发起查询请求，结果区保持初始提示。
      expect(queryRequests).toEqual([]);
      await expect(drawer.getByText('执行查询后在此显示结果。')).toBeVisible();

      // 恢复语句：按钮重新可用，只读查询照常执行（安全逻辑未变）。
      await editor.fill('SELECT 1 AS ok;');
      await expect(runButton).toBeEnabled();
      await runButton.click();
      await expect(drawer.getByText('返回 1 行')).toBeVisible({ timeout: 20_000 });
      await expect(drawer.locator('th', { hasText: 'ok' })).toBeVisible();
      expect(queryRequests).toHaveLength(1);
    } finally {
      await deleteDatabaseByName(page, dbName);
    }
  });
});
