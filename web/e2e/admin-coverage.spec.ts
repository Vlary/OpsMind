import { expect, test } from '@playwright/test';

import { ADMIN_PASSWORD, AUTH_STATE_FILE } from './helpers';

// 管理页补充覆盖：供应商「测试连接」失败路径与表单校验、数据库 SQL 查询抽屉
// （含只读拦截）、审计筛选与分页、服务器搜索、退出登录。

const PREVIEW_ORIGIN = 'http://localhost:4173';

/** 数据库列表分页为 10 条/页：先按名称搜索过滤，保证目标行可见且唯一。 */
async function focusDatabaseRow(page: import('@playwright/test').Page, name: string) {
  await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill(name);
  const row = page.locator('tr', { hasText: name }).first();
  await expect(row).toBeVisible({ timeout: 20_000 });
  return row;
}

test.describe('供应商管理补充覆盖', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('「测试连接」失败：标题透出底层 connection refused', async ({ page }) => {
    // 指向不可达端口（127.0.0.1:1）的供应商：连接立即被拒绝。
    const created = await page.request.post('/api/providers', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: {
        name: 'e2e-bad-provider',
        preset: 'custom',
        kind: 'openai_compatible',
        base_url: 'http://127.0.0.1:1/v1',
        api_key: 'e2e-key',
        default_model: 'fake-model',
        models: ['fake-model'],
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/providers');
    const row = page.locator('tr', { hasText: 'e2e-bad-provider' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const dialog = page.getByRole('dialog', { name: /测试连接 · e2e-bad-provider/ });
    await expect(dialog).toBeVisible({ timeout: 20_000 });
    const alert = dialog.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });
    // 冲刺 1 修复前：标题是「供应商连接测试失败（provider request failed）」，
    // 无信息量英文短语拼进中文标题；修复后对齐 round 6 的 ssh/database——
    // 标题透出底层 dial 原因，错误码降级为描述小字。
    await expect(alert.locator('.ant-alert-title')).toContainText('供应商连接测试失败');
    await expect(alert.locator('.ant-alert-title')).toContainText('connection refused');
    await expect(alert).toContainText('PROVIDER_TEST_FAILED');
    // 表格状态列记录失败结果。
    await expect(row.getByText('测试失败').first()).toBeVisible({ timeout: 20_000 });
  });

  test('新增供应商弹窗必填校验：清空名称与 Base URL 提交显示字段错误', async ({ page }) => {
    await page.goto('/providers');
    await page.getByRole('button', { name: '新增供应商' }).click();
    const dialog = page.getByRole('dialog', { name: '新增供应商' });
    await expect(dialog).toBeVisible();
    // 默认预设（openai）会自动填充名称与 Base URL：清空后再提交。
    await dialog.getByRole('textbox', { name: '名称' }).fill('');
    await dialog.getByRole('textbox', { name: 'Base URL' }).fill('');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog.getByText('请输入名称').first()).toBeVisible();
    await expect(dialog.getByText('请输入 Base URL').first()).toBeVisible();
    // 弹窗未关闭。
    await expect(dialog).toBeVisible();
  });
});

test.describe('数据库 SQL 查询抽屉', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('postgres 只读查询：结果表带列名与行数', async ({ page }) => {
    // 指向 e2e 自身的 postgres（global-setup 每轮重建，表恒存在）。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: {
        name: 'e2e-pg-query',
        engine: 'postgres',
        host: '127.0.0.1',
        port: 65432,
        database_name: 'aiops_e2e',
        username: 'aiops',
        password: 'aiops',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/databases');
    const row = await focusDatabaseRow(page, 'e2e-pg-query');
    await row.getByRole('button', { name: /查询/ }).click();

    const drawer = page.getByRole('dialog', { name: 'SQL 查询 · e2e-pg-query' });
    await expect(drawer).toBeVisible();
    // 只读标识与连接目标。
    await expect(drawer.getByText('只读', { exact: true })).toBeVisible();
    await expect(drawer.getByText(/127\.0\.0\.1:65432\/aiops_e2e/)).toBeVisible();

    const editor = drawer.locator('.opsmind-sql-editor');
    await editor.fill('SELECT count(*) AS conv_count FROM conversations;');
    await drawer.getByRole('button', { name: '执行查询' }).click();
    await expect(drawer.getByText('返回 1 行')).toBeVisible({ timeout: 20_000 });
    await expect(drawer.locator('th', { hasText: 'conv_count' })).toBeVisible();
    await expect(drawer.getByText('共 1 行').first()).toBeVisible();
  });

  test('写语句被只读守卫拦截：SQL_NOT_READONLY 提示且无结果、不落库', async ({ page }) => {
    const created = await page.request.post('/api/databases', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: {
        name: 'e2e-pg-readonly',
        engine: 'postgres',
        host: '127.0.0.1',
        port: 65432,
        database_name: 'aiops_e2e',
        username: 'aiops',
        password: 'aiops',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/databases');
    const row = await focusDatabaseRow(page, 'e2e-pg-readonly');
    await row.getByRole('button', { name: /查询/ }).click();

    const drawer = page.getByRole('dialog', { name: 'SQL 查询 · e2e-pg-readonly' });
    await expect(drawer).toBeVisible();
    const editor = drawer.locator('.opsmind-sql-editor');
    await editor.fill("INSERT INTO conversations (title) VALUES ('write-blocked-e2e');");
    await drawer.getByRole('button', { name: '执行查询' }).click();

    // 错误提示（中文映射）+ 专门的写操作警告，均不提供「重试」歧义。
    await expect(drawer.getByText(/仅支持只读查询，写操作不会执行/).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(
      drawer.getByText('本系统只执行只读查询，写操作不会执行', { exact: false }),
    ).toBeVisible();
    // 没有结果表。
    await expect(drawer.getByText(/返回 \d+ 行/)).toHaveCount(0);

    // API 侧确认没有写入。
    const response = await page.request.get('/api/conversations', {
      params: { limit: 200 },
    });
    const payload = (await response.json()) as { items: Array<{ title: string }> };
    expect(payload.items.some((item) => item.title === 'write-blocked-e2e')).toBe(false);
  });

  test('sqlite 缺文件：执行失败透出底层 unable to open database file', async ({ page }) => {
    const created = await page.request.post('/api/databases', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: {
        name: 'e2e-sqlite-missing',
        engine: 'sqlite',
        sqlite_path: '/nonexistent-cov/missing.db',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/databases');
    const row = await focusDatabaseRow(page, 'e2e-sqlite-missing');
    await row.getByRole('button', { name: /查询/ }).click();

    const drawer = page.getByRole('dialog', { name: 'SQL 查询 · e2e-sqlite-missing' });
    await expect(drawer).toBeVisible();
    // 连接目标显示 sqlite 路径。
    await expect(drawer.getByText(/\/nonexistent-cov\/missing\.db/)).toBeVisible();
    await drawer.getByRole('button', { name: '执行查询' }).click();

    // round 3 修复前：执行路径（区别于 round6 的连接测试路径）后端
    // message 为通用 "query failed"，不透出底层原因；修复后对齐连接
    // 测试——标题透出底层打开失败原因，错误码降级为描述小字。
    const alert = drawer.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });
    await expect(alert).toContainText('SQL 执行失败');
    await expect(alert).toContainText('unable to open database file');
    await expect(alert).toContainText('SQL_QUERY_FAILED');
  });
});

test.describe('审计与全局交互补充覆盖', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('审计按动作筛选：仅显示所选动作的记录', async ({ page }) => {
    await page.goto('/audit');
    const table = page.locator('.ant-table');
    await expect(table).toBeVisible();

    // placeholder 被搜索输入拦截点击且下拉 option 点击不稳定：
    // 打开动作筛选（toolbar 第 1 个 Select）后键盘输入过滤 + Enter 选中。
    await page.locator('.opsmind-admin-toolbar .ant-select').first().click();
    await page.keyboard.type('登录成功');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(page.locator('.opsmind-admin-toolbar .ant-select').first()).toContainText(
      '登录成功',
    );

    const rows = table.locator('.ant-table-row');
    const loginRows = table.locator('.ant-table-row', { hasText: 'auth.login_succeeded' });
    await expect(loginRows.first()).toBeVisible({ timeout: 20_000 });
    // 当前页全部行都是登录成功（动作筛选下服务器创建记录不出现）。
    expect(await loginRows.count()).toBe(await rows.count());
    await expect(table.getByText('server.create')).toHaveCount(0);
  });

  test('审计游标分页：下一页到第 2 页，上一页返回第 1 页', async ({ page }) => {
    // 分页断言依赖「记录数 > 每页条数」；文件并行执行顺序不定，
    // 本用例先自行写入 25 对审计（数据库创建 + 删除），保证与执行顺序
    // 无关；用后即删，避免残留把既有用例（round6 等）的库挤出第一页。
    const seedIds: string[] = [];
    for (let i = 0; i < 25; i += 1) {
      const response = await page.request.post('/api/databases', {
        headers: { Origin: PREVIEW_ORIGIN },
        data: {
          name: `e2e-audit-page-seed-${i}`,
          engine: 'sqlite',
          sqlite_path: '/nonexistent-audit-seed/seed.db',
        },
      });
      expect(response.status()).toBe(201);
      seedIds.push(((await response.json()) as { id: string }).id);
    }
    for (const id of seedIds) {
      const removed = await page.request.delete(`/api/databases/${id}`, {
        headers: { Origin: PREVIEW_ORIGIN },
      });
      expect(removed.status()).toBeLessThan(300);
    }

    await page.goto('/audit');
    const table = page.locator('.ant-table');
    await expect(table).toBeVisible();
    await expect(table.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

    // 缩小每页条数以保证存在第二页（种子 + 本用例写入已超 20 条）。
    // 页大小 Select 是 toolbar 第 3 个：打开后 ArrowUp（50→20）+ Enter。
    const pageSizeSelect = page.locator('.opsmind-admin-toolbar .ant-select').nth(2);
    await pageSizeSelect.click();
    await page.keyboard.press('ArrowUp');
    await page.keyboard.press('Enter');
    await expect(pageSizeSelect).toContainText('20 条 / 页');
    await expect(page.getByText('还有更多记录')).toBeVisible({ timeout: 20_000 });

    await page.getByRole('button', { name: '下一页' }).click();
    await expect(page.getByText('第 2 页')).toBeVisible({ timeout: 20_000 });
    await expect(table.locator('.ant-table-row').first()).toBeVisible();

    await page.getByRole('button', { name: '上一页' }).click();
    await expect(page.getByText('第 1 页')).toBeVisible({ timeout: 20_000 });
  });

  test('服务器搜索过滤：命中显示，未命中空态', async ({ page }) => {
    await page.goto('/servers');
    const search = page.getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签');
    await expect(search).toBeVisible();
    // 「共 N 台」在 toolbar 与表格分页各出现一次，用 toolbar 容器精确定位。
    const counter = page.locator('.opsmind-admin-toolbar').getByText(/共 \d+ 台/);

    await search.fill('e2e-fake-host');
    await expect(page.locator('tr', { hasText: 'e2e-fake-host' }).first()).toBeVisible();
    await expect(counter).toHaveText('共 1 台');

    await search.fill('不存在的服务器-no-such');
    await expect(counter).toHaveText('共 0 台');
    await expect(page.locator('.ant-table-row')).toHaveCount(0);
    // ServersPage 自定义空态（EmptyState）。
    await expect(page.getByText('还没有服务器')).toBeVisible();
  });

  test('顶栏退出登录：清除会话并回到登录页', async ({ page, request }) => {
    // storageState 的会话 Cookie 被全部用例共享：logout 会注销该会话，
    // 拖垮并行与后续用例。本用例先自行登录换取独立 Cookie 再覆盖注入，
    // 退出只影响本用例的会话。
    const login = await request.post('/api/auth/login', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: { password: ADMIN_PASSWORD },
    });
    expect(login.status()).toBe(200);
    const setCookie = login.headers()['set-cookie'] ?? '';
    const session = /opsmind_session=([^;]+)/.exec(setCookie)?.[1];
    expect(session).toBeTruthy();
    await page
      .context()
      .addCookies([{ name: 'opsmind_session', value: session, domain: 'localhost', path: '/' }]);

    await page.goto('/chat');
    await expect(page.getByText('开始新的运维对话')).toBeVisible();

    // 右上角用户菜单（登录用户名恒为 admin）。
    await page.getByRole('button', { name: /admin/ }).click();
    await page.getByRole('menuitem', { name: '退出登录' }).click();
    await expect(page).toHaveURL(/\/login/, { timeout: 20_000 });
    await expect(page.getByPlaceholder('请输入管理员密码')).toBeVisible();
  });
});
