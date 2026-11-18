import { expect, test } from './fixtures';
import { ADMIN_PASSWORD, AUTH_STATE_FILE } from './helpers';

// 登录链路：失败提示、成功跳转、会话失效后的受保护页跳转。

test.describe('登录与会话守卫', () => {
  test('错误密码显示失败提示，且不跳转', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('请输入用户名').fill('admin');
    const input = page.getByPlaceholder('请输入密码');
    await expect(input).toBeVisible();
    await input.fill('definitely-wrong-password');
    // antd 对纯双字文本按钮自动插入空格（accessible name 为 “登 录”），用正则兼容。
    await page.getByRole('button', { name: /登\s*录/ }).click();

    // ApiErrorAlert：00012 多用户后用户名/密码任一错误统一显示
    // 「用户名或密码错误」（不区分账号是否存在，避免枚举）。
    await expect(page.getByText('用户名或密码错误')).toBeVisible();
    await expect(page).toHaveURL(/\/login/);
  });

  test('正确密码登录成功并跳转到 /chat', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('请输入用户名').fill('admin');
    const input = page.getByPlaceholder('请输入密码');
    await expect(input).toBeVisible();
    await input.fill(ADMIN_PASSWORD);
    await page.getByRole('button', { name: /登\s*录/ }).click();

    await expect(page).toHaveURL(/\/chat$/);
    // 登录后进入空会话引导页。
    await expect(page.getByText('开始新的运维对话')).toBeVisible();
  });

  test('清除会话 Cookie 后访问受保护页跳转登录并携带 next', async ({ browser }) => {
    // 先以 globalSetup 写入的会话 Cookie 建立登录态。
    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    // workers:2 并行时另一 worker 可能正在跑 SSE 场景，preview 代理偶发变慢：
    // 关键断言放宽到 30s，避免满载环境下的假失败。
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible({ timeout: 30_000 });

    await context.clearCookies();
    await page.goto('/audit');

    // RequireAuth 检测 401 后跳 /login?next=%2Faudit，并带 reason 让登录页
    // 明确提示会话已失效（RBAC 走查 sprint 1）。
    await expect(page).toHaveURL(/\/login\?next=%2Faudit&reason=session_expired$/, {
      timeout: 30_000,
    });
    await expect(
      page.getByText('登录已失效（会话过期或角色/权限已变更），请重新登录。'),
    ).toBeVisible({ timeout: 30_000 });
    await expect(page.getByPlaceholder('请输入用户名')).toBeVisible({ timeout: 30_000 });
    await context.close();
  });
});
