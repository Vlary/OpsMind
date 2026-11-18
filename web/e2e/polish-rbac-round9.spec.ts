import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { expect, test, type Page } from './fixtures';
import { AUTH_STATE_FILE } from './helpers';

// RBAC 第 9 轮走查（high）：用户管理的弹窗表单在关闭后会保留上一条草稿。
//
// 1) 改角色：先打开 A 的「改角色」并选一个角色 → 取消 → 打开 B，弹窗标题
//    是 B，但下拉仍是 A 的角色；直接保存会把 B 改成 A 的角色。回归断言
//    B 的弹窗显示 B 的当前角色，且保存请求体是 B 本次选择的角色。
// 2) 创建用户：填写草稿后取消再打开，用户名/邮箱/角色必须回到初始值。
//
// 账号来源：e2e 环境未配置 SMTP，POST /api/users 恒 503（见 rbac-roles.spec.ts
// 文件头），因此用 psql 直插两个临时启用账号（复用 admin 的 password_hash）。
// 临时账号只用于本文件的角色弹窗，跑完即删，不触碰 reviewer/auditor 等
// 其它并行用例依赖的账号。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const E2E_DATABASE = 'aiops_e2e';
// psql 输出上限 1MB：这里只取单行 id，任何意外放大都会立即失败（防 OOM）。
const PSQL_MAX_BUFFER = 1024 * 1024;

async function psql(sql: string): Promise<string> {
  const { stdout } = await execFileAsync(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      'db',
      'psql',
      '-U',
      'aiops',
      '-d',
      E2E_DATABASE,
      '-v',
      'ON_ERROR_STOP=1',
      '-A',
      '-t',
      '-c',
      sql,
    ],
    { cwd: repoRoot, maxBuffer: PSQL_MAX_BUFFER },
  );
  return stdout.trim();
}

function firstUuid(output: string): string {
  const match = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/.exec(output);
  const id = match?.[0] ?? '';
  expect(id, output).not.toBe('');
  return id;
}

/** 直插一个启用账号（复用 admin 密码哈希），返回 id。 */
async function seedUser(username: string, role: string): Promise<string> {
  const output = await psql(`
    INSERT INTO users (username, password_hash, role, enabled, display_name, email)
    SELECT '${username}', password_hash, '${role}', true, '${username}', '${username}@example.com'
    FROM users WHERE username = 'admin'
    RETURNING id::text;`);
  return firstUuid(output);
}

async function deleteUsers(usernames: readonly string[]): Promise<void> {
  const list = usernames.map((name) => `'${name}'`).join(', ');
  await psql(`
    DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE username IN (${list}));
    DELETE FROM users WHERE username IN (${list});`);
}

/** 当前可见的 antd 下拉选项（与 polish-p2sprint1.spec.ts 同口径）。 */
function dropdownOption(page: Page, text: string) {
  return page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option')
    .filter({ hasText: text });
}

function uniqueSuffix(): string {
  return `${Date.now().toString(36)}-${Math.floor(Math.random() * 1_000_000).toString(36)}`;
}

test.describe('RBAC 第 9 轮：用户弹窗不残留上一个目标', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('改角色 A→取消→B：显示 B 当前角色，保存请求体是 B 选择的角色', async ({ page }) => {
    test.setTimeout(120_000);
    const suffix = uniqueSuffix();
    const userA = `r9-role-a-${suffix}`;
    const userB = `r9-role-b-${suffix}`;
    await seedUser(userA, 'operator');
    const bId = await seedUser(userB, 'auditor');
    try {
      await page.goto('/users');
      await expect(page.getByRole('heading', { name: '用户' })).toBeVisible();

      // 1) A：把下拉改成「审批者」后取消，留下一个与 A 原角色不同的草稿。
      const rowA = page.getByRole('row').filter({ hasText: userA });
      await rowA.getByRole('button', { name: '改角色' }).click();
      let dialog = page.locator('.ant-modal').filter({ hasText: `修改角色 · ${userA}` });
      await expect(dialog).toBeVisible();
      await dialog.locator('.ant-select').click();
      await dropdownOption(page, '审批者').click();
      await expect(dialog.locator('.ant-select')).toContainText('审批者');
      await dialog.getByRole('button', { name: /取\s*消/ }).click();
      await expect(dialog).toBeHidden({ timeout: 10_000 });

      // 2) B：弹窗标题切换到 B 后，下拉必须回填 B 的当前角色「审计员」。
      const rowB = page.getByRole('row').filter({ hasText: userB });
      await rowB.getByRole('button', { name: '改角色' }).click();
      dialog = page.locator('.ant-modal').filter({ hasText: `修改角色 · ${userB}` });
      await expect(dialog).toBeVisible();
      await expect(dialog.locator('.ant-select')).toContainText('审计员');

      // 3) 为 B 选择「运维工程师」并保存：PATCH 请求体必须是 B 的选择。
      await dialog.locator('.ant-select').click();
      await dropdownOption(page, '运维工程师').click();
      const patchWait = page.waitForRequest(
        (request) => request.method() === 'PATCH' && request.url().endsWith(`/api/users/${bId}`),
      );
      await dialog.getByRole('button', { name: /保\s*存/ }).click();
      const patch = await patchWait;
      expect(patch.postDataJSON()).toEqual({ role: 'operator' });
      await expect(
        page.locator('.ant-message-success', {
          hasText: `已将 ${userB} 的角色改为「运维工程师」`,
        }),
      ).toBeVisible();
      await expect(dialog).toBeHidden({ timeout: 10_000 });
    } finally {
      await deleteUsers([userA, userB]);
    }
  });

  test('创建用户取消后重开：用户名/邮箱/角色回到初始值', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/users');
    const openCreate = page.getByRole('button', { name: '创建用户' });
    await expect(openCreate).toBeVisible();

    await openCreate.click();
    let dialog = page.locator('.ant-modal').filter({ hasText: '创建用户' });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('用户名').fill('r9-draft-user');
    await dialog.getByLabel('邮箱').fill('r9-draft@example.com');
    await dialog.locator('.ant-select').click();
    await dropdownOption(page, '审批者').click();
    await expect(dialog.locator('.ant-select')).toContainText('审批者');
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    await openCreate.click();
    dialog = page.locator('.ant-modal').filter({ hasText: '创建用户' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByLabel('用户名')).toHaveValue('');
    await expect(dialog.getByLabel('邮箱')).toHaveValue('');
    await expect(dialog.locator('.ant-select')).toContainText('审计员');
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });
  });

  test('未配置 SMTP 创建用户：错误块给出 AIOPS_SMTP_* 指引且无英文 detail', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/users');
    await page.getByRole('button', { name: '创建用户' }).click();
    const dialog = page.locator('.ant-modal').filter({ hasText: '创建用户' });
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('用户名').fill(`r9-smtp-${uniqueSuffix()}`);
    await dialog.getByLabel('邮箱').fill('r9-smtp@example.com');
    await dialog.getByRole('button', { name: /创建并发送邀请/ }).click();

    // e2e 后端未配置 SMTP：POST /users 返回 503 DELIVERY_UNAVAILABLE。
    // 错误块必须告诉管理员配置哪些环境变量，不能只显示通用中文 + 英文 detail。
    await expect(dialog.getByText(/AIOPS_SMTP_HOST/).first()).toBeVisible();
    await expect(dialog.getByText(/AIOPS_SMTP_PORT/).first()).toBeVisible();
    await expect(dialog.getByText(/no delivery channel available/)).toHaveCount(0);
  });
});
