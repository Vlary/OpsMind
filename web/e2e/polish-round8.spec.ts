import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 走查驱动第 3 轮打磨的端到端验证（沿用 polish-roundN 编号顺延）：
//  - SQL 控制台查询失败不再显示英文裸串 "query failed"：底层原因
//    （表不存在 / 库不可达等）脱敏后透出到提示标题，对齐连接测试
//    （round 6 / sprint 1）的做法；只读守卫拒绝与超时的 message 同步中文化。
//  - 页面标题随路由区分（成品标准「页面标题与 favicon」）：多标签页
//    操作时可按 title 分辨所在页面。

const PREVIEW_ORIGIN = 'http://localhost:4173';

test.describe('走查三：页面标题随路由区分', () => {
  test('公开页：登录与 404', async ({ page }) => {
    await page.goto('/login');
    await expect(page.getByPlaceholder('请输入管理员密码')).toBeVisible();
    await expect(page).toHaveTitle('登录 · 云枢 OpsMind');

    // 未知路径是公开路由（NotFoundPage），未登录也显示 404 而非跳登录。
    await page.goto('/no-such-round8-page');
    await expect(page.getByText('页面不存在或已被移动')).toBeVisible();
    await expect(page).toHaveTitle('页面不存在 · 云枢 OpsMind');
  });
});

test.describe('走查三：页面标题随路由区分（登录态）', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('主导航各页面：title 为「页面名 · 云枢 OpsMind」', async ({ page }) => {
    const cases: Array<[path: string, title: string]> = [
      ['/chat', '对话 · 云枢 OpsMind'],
      ['/servers', '服务器 · 云枢 OpsMind'],
      ['/databases', '数据库 · 云枢 OpsMind'],
      ['/providers', '模型 · 云枢 OpsMind'],
      ['/audit', '审计 · 云枢 OpsMind'],
    ];
    for (const [path, title] of cases) {
      await page.goto(path);
      // 等待页面真正渲染（壳层 + 懒加载路由）再断言 title。
      await expect(page.locator('.app-header')).toBeVisible({ timeout: 20_000 });
      await expect(page).toHaveTitle(title);
    }
  });

  test('会话详情路由 /c/:id 与对话页同名', async ({ page }) => {
    await page.goto('/chat');
    await expect(page.getByText('开始新的运维对话')).toBeVisible({ timeout: 20_000 });
    // 发送一条消息触发跳转 /c/:id（假 LLM 默认场景）。
    const box = page.getByLabel('消息输入框');
    await box.fill('round8 title 探测');
    await page.getByRole('button', { name: '发送' }).click();
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page).toHaveTitle('对话 · 云枢 OpsMind');
  });
});

test.describe('走查三：SQL 查询失败透出底层原因', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('postgres 查询不存在的表：标题含表名与 does not exist', async ({ page }) => {
    // 走查员实测场景：对新建资产执行 SELECT，表不存在时修复前只显示
    // 「SQL 执行失败（query failed）」，密码错 / 库不可达全显示同一句。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: PREVIEW_ORIGIN },
      data: {
        name: 'round8-pg-missing-table',
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
    await page
      .getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名')
      .fill('round8-pg-missing-table');
    const row = page.locator('tr', { hasText: 'round8-pg-missing-table' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /查询/ }).click();

    const drawer = page.getByRole('dialog', { name: 'SQL 查询 · round8-pg-missing-table' });
    await expect(drawer).toBeVisible();
    await drawer.locator('.opsmind-sql-editor').fill('SELECT * FROM no_such_table_round8;');
    await drawer.getByRole('button', { name: '执行查询' }).click();

    const alert = drawer.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });
    // 修复前标题是「SQL 执行失败（query failed）」；修复后透出底层
    // pq: relation "no_such_table_round8" does not exist。
    await expect(alert.locator('.ant-alert-title')).toContainText('SQL 执行失败');
    await expect(alert.locator('.ant-alert-title')).toContainText('no_such_table_round8');
    await expect(alert.locator('.ant-alert-title')).toContainText('does not exist');
    await expect(alert).toContainText('SQL_QUERY_FAILED');
  });

  // 只读守卫拒绝的 UI 中文提示已有用例（admin-coverage.spec.ts
  // 「写语句被只读守卫拦截」）；超时 message 的展示行为由单测
  // apiErrorMore.test.ts「SQL 超时固定中文 message 与标题同义」覆盖，
  // 此处不重复造慢查询用例。
});
