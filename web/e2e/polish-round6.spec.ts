import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE, SCENARIO_RISKY, sendMessage } from './helpers';

// 第 6 轮打磨项的端到端验证：
//  - 连接测试失败把底层原因（connection refused / unable to open database
//    file）透出到弹窗文案，运维可区分防火墙、地址错还是文件不存在；
//  - 登录失败提示不再混入「invalid credentials」英文；
//  - 对话内审批通过后工具卡失败状态显示中文说明，英文错误码与底层
//    原因降级为小字保留排查信息。

test.describe('第 6 轮打磨：连接测试失败透出真实原因', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('服务器连接测试失败：弹窗透出底层 connection refused', async ({ page }) => {
    await page.goto('/servers');
    // e2e-fake-host 指向 127.0.0.1:1，SSH 立即被拒绝。
    const row = page.locator('tr', { hasText: 'e2e-fake-host' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const dialog = page.getByRole('dialog', { name: /测试连接 · e2e-fake-host/ });
    await expect(dialog).toBeVisible({ timeout: 20_000 });
    const alert = dialog.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });

    // 修复前：标题只有「SSH 连接测试失败」，真实原因只在后端日志里。
    await expect(alert.locator('.ant-alert-title')).toContainText('SSH 连接测试失败');
    await expect(alert.locator('.ant-alert-title')).toContainText('connection refused');
  });

  test('数据库连接测试失败：弹窗透出底层 unable to open database file', async ({ page }) => {
    // 预置一个指向不存在路径的 sqlite 数据库。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: 'http://localhost:4173' },
      data: {
        name: 'round6-bad-sqlite',
        engine: 'sqlite',
        sqlite_path: '/nonexistent-round6/missing.db',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/databases');
    // 列表按 created_at 升序、每页 10 条，并行 spec 累积的 e2e-* 库会把
    // 新建的行挤到第二页——先搜索过滤名称再断言（三期 sprint 5 实测 flaky）。
    await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill('round6-bad-sqlite');
    const row = page.locator('tr', { hasText: 'round6-bad-sqlite' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const dialog = page.getByRole('dialog', { name: /测试连接 · round6-bad-sqlite/ });
    await expect(dialog).toBeVisible({ timeout: 20_000 });
    const alert = dialog.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });

    await expect(alert.locator('.ant-alert-title')).toContainText('数据库连接测试失败');
    await expect(alert.locator('.ant-alert-title')).toContainText('unable to open database file');
  });
});

test.describe('第 6 轮打磨：登录失败提示全中文', () => {
  test('错误密码登录：提示不再混入 invalid credentials 英文', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('请输入管理员密码').fill('definitely-wrong-round6');
    await page.getByRole('button', { name: /登\s*录/ }).click();

    const alert = page.locator('.ant-alert-error').filter({ hasText: '管理员密码错误' });
    await expect(alert).toBeVisible({ timeout: 20_000 });
    // 修复前：标题是「管理员密码错误（invalid credentials）」。
    await expect(alert).not.toContainText('invalid credentials');
  });
});

test.describe('第 6 轮打磨：工具卡失败状态中文化', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('审批批准后 ssh_exec 失败：中文说明为主文案，英文码与原因降级小字', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: '批准' }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    // 修复前：主文案是「执行失败：SSH_EXEC_FAILED（ssh command failed）」。
    await expect(sshCard.getByText('执行失败：远程命令执行失败').first()).toBeVisible({
      timeout: 30_000,
    });
    // 小字保留底层英文原因与错误码，供排查。
    await expect(sshCard.getByText(/ssh command failed/).first()).toBeVisible();
    await expect(sshCard.getByText(/connection refused/).first()).toBeVisible();
    await expect(sshCard.getByText('SSH_EXEC_FAILED').first()).toBeVisible();
  });
});
