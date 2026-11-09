import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 二期冲刺 1 第 1 轮打磨的端到端验证：
//  1) mssql/redis 数据库此前在 handler 落入 default 分支 400，前端表单
//     整条流程第一步被打死。修复后 UI 表单保存成功且列表出现。
//  2) 新增服务器/数据库弹窗取消后再开不清空表单（含密码明文残留、
//     引擎残留连锁 400）。修复后取消路径 resetFields。
//  3) 终端页服务器下拉对 WinRM 认证机器显示无关的 SSH 端口
//     （walk-win 127.0.0.1:22）。修复后显示 winrm_port。
//  4) 断开后立刻「新建会话」409 的文案解释收尾时序（mock 409）。
//  5) 终端 WS 异常断开（preview 代理 ws:false 必然失败）显示
//     「连接异常断开」提示——旧条件 `!ended && abnormalClose` 恒假，
//     提示是死代码。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

/** 当前可见的下拉选项（antd：非 hidden 的 dropdown 内的 option）。 */
function dropdownOption(page: Page, text: string) {
  return page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option')
    .filter({ hasText: text });
}

test.describe('二期冲刺 1 打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('mssql/redis 数据库经 UI 表单创建成功', async ({ page }) => {
    test.setTimeout(150_000);

    await page.goto('/databases');
    await expect(page.getByRole('heading', { name: '数据库' })).toBeVisible();
    const openCreate = page.getByRole('button', { name: /新增数据库/ });
    await expect(openCreate).toBeVisible();

    // mssql：选 SQL Server、填必填（host/数据库名/用户名/密码）。
    const suffix = Date.now().toString().slice(-6);
    const mssqlName = `e2e-p2s1-mssql-${suffix}`;
    await openCreate.click();
    const dialog = page.locator('.ant-modal').filter({ hasText: '新增数据库' });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('名称').fill(mssqlName);
    await dialog.locator('#engine').click();
    await dropdownOption(page, 'SQL Server').click();
    await dialog.getByLabel('主机地址').fill('10.255.255.98');
    await dialog.getByLabel('数据库名').fill('master');
    await dialog.getByLabel('用户名').fill('sa');
    await dialog.getByLabel('密码', { exact: true }).fill('E2eSqlPass99');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeHidden({ timeout: 20_000 });

    // 列表出现新建行（修复前：保存 400，横幅「表单校验未通过」）。
    // 列表按 created_at 升序、每页 10 条（DatabasesPage 分页），并行跑的
    // 其它 spec 会留下未清理的 e2e-* 库——累计 ≥10 时新建行落到第二页，
    // 直接断言第一页会超时（三期 sprint 5 实测 flaky）。先用搜索框过滤
    // 名称，行必在过滤后的第一页。
    const dbSearch = page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名');
    await dbSearch.fill(mssqlName);
    await expect(page.locator('.ant-table-row').filter({ hasText: mssqlName }).first()).toBeVisible(
      { timeout: 20_000 },
    );

    // redis：只填 host/port。
    const redisName = `e2e-p2s1-redis-${suffix}`;
    await dbSearch.fill('');
    await openCreate.click();
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('名称').fill(redisName);
    await dialog.locator('#engine').click();
    await dropdownOption(page, 'Redis').click();
    await dialog.getByLabel('主机地址').fill('10.255.255.98');
    await dialog.getByLabel('端口').fill('6380');
    await dialog.getByLabel('DB 序号').fill('7');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeHidden({ timeout: 20_000 });
    await dbSearch.fill(redisName);
    const redisRow = page.locator('.ant-table-row').filter({ hasText: redisName }).first();
    await expect(redisRow).toBeVisible({ timeout: 20_000 });
    await dbSearch.fill('');

    // 清理，避免污染后续轮次的列表断言。
    const list = await page.request.get('/api/databases');
    const payload = (await list.json()) as { items: { id: string; name: string }[] };
    for (const item of payload.items) {
      if (item.name === mssqlName || item.name === redisName) {
        const deleted = await page.request.delete(`/api/databases/${item.id}`, {
          headers: ORIGIN_HEADERS,
        });
        expect(deleted.status()).toBe(204);
      }
    }
  });

  test('数据库弹窗取消后再开不残留上次输入（含密码明文）', async ({ page }) => {
    test.setTimeout(120_000);

    await page.goto('/databases');
    const openCreate = page.getByRole('button', { name: /新增数据库/ });
    await expect(openCreate).toBeVisible();

    // 第一次：填满含密码，点取消。
    await openCreate.click();
    let dialog = page.locator('.ant-modal').filter({ hasText: '新增数据库' });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('名称').fill('first-attempt-db');
    await dialog.locator('#engine').click();
    await dropdownOption(page, 'SQL Server').click();
    await dialog.getByLabel('主机地址').fill('10.0.0.9');
    await dialog.getByLabel('密码', { exact: true }).fill('SecretPass99');
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // 第二次打开：表单应回到初始默认（名称空、引擎 postgres、密码空）。
    await openCreate.click();
    dialog = page.locator('.ant-modal').filter({ hasText: '新增数据库' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByLabel('名称')).toHaveValue('');
    // antd6 的 #engine 是 Select 内部 input（值恒空）；选中文案在
    // selection-item——引擎选择器是弹窗内第一个 Select。
    await expect(dialog.locator('.ant-select').first()).toContainText('PostgreSQL');
    // 引擎回默认后不再渲染 SQL Server 专属密码必填文案。
    await expect(dialog.getByText('SQL Server 密码必填；已保存后留空表示不修改。')).toHaveCount(0);
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });
  });

  test('服务器弹窗取消后再开不残留上次输入（含密码明文）', async ({ page }) => {
    test.setTimeout(120_000);

    await page.goto('/servers');
    const openCreate = page.getByRole('button', { name: /新增服务器/ });
    await expect(openCreate).toBeVisible();

    await openCreate.click();
    let dialog = page.locator('.ant-modal').filter({ hasText: '新增服务器' });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('名称').fill('first-attempt-server');
    await dialog.getByLabel('主机地址').fill('10.0.0.9');
    await dialog.getByLabel('登录用户').fill('root');
    await dialog.getByLabel('密码', { exact: true }).fill('ServerPass99');
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    await openCreate.click();
    dialog = page.locator('.ant-modal').filter({ hasText: '新增服务器' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByLabel('名称')).toHaveValue('');
    await expect(dialog.getByLabel('主机地址')).toHaveValue('');
    await expect(dialog.getByLabel('登录用户')).toHaveValue('');
    await expect(dialog.getByLabel('密码', { exact: true })).toHaveValue('');
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });
  });

  test('终端下拉 WinRM 认证机器显示 winrm_port 而非 SSH 端口', async ({ page }) => {
    test.setTimeout(120_000);

    // 造一台 WinRM 认证服务器（地址不拨号，只验证展示）。
    const name = `e2e-p2s1-winrm-${Date.now().toString().slice(-6)}`;
    const created = await page.request.post('/api/servers', {
      headers: ORIGIN_HEADERS,
      data: {
        name,
        os: 'windows',
        host: '10.255.255.97',
        port: 22,
        winrm_port: 5985,
        username: 'administrator',
        auth_method: 'password_ntlm',
        password: 'not-a-real-password',
        tags: ['e2e'],
        auto_exec_enabled: false,
      },
    });
    expect(created.status()).toBe(201);
    const server = (await created.json()) as { id: string };

    try {
      await page.goto('/term');
      await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
      const select = page.getByLabel('选择服务器');
      await select.click();
      // 修复前：显示 `${name}（10.255.255.97:22）`——SSH 端口与 WinRM
      // 通道无关；修复后显示 winrm_port 5985（且该项禁用）。
      const option = dropdownOption(page, name);
      await expect(option).toBeVisible();
      await expect(option).toContainText('10.255.255.97:5985');
      await expect(option).not.toContainText('10.255.255.97:22');
      await page.keyboard.press('Escape');
    } finally {
      const deleted = await page.request.delete(`/api/servers/${server.id}`, {
        headers: ORIGIN_HEADERS,
      });
      expect([200, 204]).toContain(deleted.status());
    }
  });

  test('创建会话 409：文案解释收尾时序；会话列表被失效刷新', async ({ page }) => {
    test.setTimeout(120_000);

    // mock 创建会话端点恒 409 TERM_SESSION_EXISTS（walk-a3 场景：断开后
    // 立刻新建，服务端异步收尾中）。
    await page.route('**/api/term/sessions', async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 409,
          contentType: 'application/json',
          body: JSON.stringify({
            error: {
              code: 'TERM_SESSION_EXISTS',
              message: 'server already has an active session',
              request_id: 'e2e-p2s1-409',
            },
          }),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [] }),
      });
    });

    await page.goto('/term');
    await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
    await page.getByRole('button', { name: /打开终端/ }).click();

    // 修复后：409 的文案解释「上一会话正在收尾」并引导稍候重试
    // （修复前：只显示「该服务器已有活动终端会话（server already has
    // an active session）」）。
    const alert = page.locator('.ant-alert-error').first();
    await expect(alert).toBeVisible({ timeout: 20_000 });
    await expect(alert).toContainText('稍候重试');
    await expect(alert).toContainText('收尾');
  });

  test('终端 WS 异常断开显示「连接异常断开」提示', async ({ page }) => {
    test.setTimeout(120_000);

    // 创建会话 mock 201；WS 层用假 WebSocket 模拟「已连接后无 close 帧
    // 断开」（vite preview 代理 ws:false 下真实 WS 升级会挂起在
    // connecting，不触发 onclose——无法自然到达异常断开分支）。
    await page.addInitScript(() => {
      const OriginalWebSocket = window.WebSocket;
      class AbnormalCloseWebSocket extends OriginalWebSocket {
        constructor(url: string | URL, protocols?: string | string[]) {
          super(url, protocols);
          // 打开后 100ms 无 close 帧直接断开（模拟后端崩溃/断网，
          // 契约 6.4 abnormalClose 分支）。
          setTimeout(() => {
            this.dispatchEvent(new Event('open'));
            setTimeout(() => {
              this.dispatchEvent(new Event('close'));
            }, 100);
          }, 50);
        }
      }
      window.WebSocket = AbnormalCloseWebSocket as unknown as typeof WebSocket;
    });
    const now = new Date().toISOString();
    const sessionId = 'e2e-p2s1-0000-4000-8000-000000000001';
    await page.route('**/api/term/sessions**', async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 201,
          contentType: 'application/json',
          body: JSON.stringify({
            session: {
              id: sessionId,
              server_id: 'seed-server',
              server_name: 'e2e-fake-host',
              os: 'linux',
              username: 'admin',
              status: 'active',
              cols: 120,
              rows: 40,
              ended_reason: '',
              has_recording: false,
              bytes_written: 0,
              started_at: now,
              last_activity_at: now,
              ended_at: null,
              created_at: now,
              updated_at: now,
            },
            ws_url: `/api/term/sessions/${sessionId}/ws`,
          }),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [] }),
      });
    });

    await page.goto('/term');
    await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
    await page.getByRole('button', { name: /打开终端/ }).click();

    // 修复前：abnormalClose=true 时 ended 恒 true，「连接异常断开」提示
    // 是永不可达的死代码（页面只静默变「已结束」）。修复后提示可见。
    await expect(page.getByText('连接异常断开（服务端未发送结束原因）')).toBeVisible({
      timeout: 20_000,
    });
  });
});
