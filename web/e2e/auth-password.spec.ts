import { expect, test } from '@playwright/test';
import { ADMIN_PASSWORD, AUTH_STATE_FILE } from './helpers';

// 修改密码全链路（顶栏头像下拉 → 弹窗 → API → 会话吊销 → 重新登录）。
//
// 本文件在独立 Playwright project 中运行（见 playwright.config.ts 的
// projects.dependencies），保证在其余全部用例之后串行执行：改密会吊销
// 管理员的全部会话，若与其他用例并行，它们持有的 storageState Cookie
// 会立即失效导致连锁失败。

const NEW_PASSWORD = 'e2e-rotated-pass-456';

test.use({ storageState: AUTH_STATE_FILE });

test.describe('修改密码', () => {
  test('头像下拉打开弹窗；错误旧密码被拒；改密后全部会话失效并可用新密码重新登录', async ({
    page,
  }) => {
    await page.goto('/chat');
    await expect(page.getByText('开始新的运维对话')).toBeVisible();

    // 顶栏头像下拉 → 修改密码弹窗。
    await page.getByRole('button', { name: 'admin' }).click();
    await page.getByRole('menuitem', { name: '修改密码' }).click();
    const dialog = page.getByRole('dialog', { name: '修改密码' });
    await expect(dialog).toBeVisible();
    // 会话将全部失效的提示。
    await expect(dialog.getByText('修改成功后当前所有登录会话都会失效')).toBeVisible();

    const oldInput = dialog.getByPlaceholder('请输入当前密码');
    const newInput = dialog.getByPlaceholder('请输入新密码', { exact: true });
    const confirmInput = dialog.getByPlaceholder('请再次输入新密码');

    // 1) 确认密码不一致 → 前端校验拦截，不发请求。
    await oldInput.fill(ADMIN_PASSWORD);
    await newInput.fill(NEW_PASSWORD);
    await confirmInput.fill('different-pass');
    await dialog.getByRole('button', { name: '确认修改' }).click();
    await expect(dialog.getByText('两次输入的新密码不一致')).toBeVisible();

    // 2) 错误旧密码 → AUTH_INVALID 错误条：改密场景无用户名字段，
    //    文案必须是「旧密码不正确」（登录页的 AUTH_INVALID 为
    //    「管理员密码错误」，见本用例第 4 步登录失败断言）。
    await confirmInput.fill(NEW_PASSWORD);
    await oldInput.fill('definitely-wrong-old-pass');
    await dialog.getByRole('button', { name: '确认修改' }).click();
    await expect(dialog.getByText('旧密码不正确')).toBeVisible({ timeout: 20_000 });

    // 3) 正确旧密码 → 改密成功，清缓存并回到登录页。
    await oldInput.fill(ADMIN_PASSWORD);
    await dialog.getByRole('button', { name: '确认修改' }).click();
    await expect(page).toHaveURL(/\/login/, { timeout: 20_000 });
    await expect(page.getByText('密码已修改，请使用新密码重新登录')).toBeVisible();

    // 冲刺 7 打磨：改密清缓存会中止在途 me 查询，登录页曾把这类会话结束
    // 兜底成合成 INTERNAL（无 request_id），与上面的成功提示并存渲染出
    // 「服务器内部错误」假故障横幅。此处断言横幅不再出现；且需先等 me
    // 探测结束（表单可见）再断言，覆盖横幅与表单同帧出现的时序。
    await expect(page.getByPlaceholder('请输入管理员密码')).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('.opsmind-login-page').getByText('INTERNAL')).toHaveCount(0);
    await expect(page.locator('.opsmind-login-page').getByText('服务器内部错误')).toHaveCount(0);

    // 旧会话 Cookie 已被服务端吊销：API 直接 401。
    const me = await page.request.get('/api/auth/me');
    expect(me.status()).toBe(401);

    // 4) 旧密码不能再登录；新密码可以。
    const loginInput = page.getByPlaceholder('请输入管理员密码');
    await expect(loginInput).toBeVisible();
    await loginInput.fill(ADMIN_PASSWORD);
    await page.getByRole('button', { name: /登\s*录/ }).click();
    await expect(page.getByText('管理员密码错误')).toBeVisible();
    await expect(page).toHaveURL(/\/login/);

    await loginInput.fill(NEW_PASSWORD);
    await page.getByRole('button', { name: /登\s*录/ }).click();
    await expect(page).toHaveURL(/\/chat$/, { timeout: 20_000 });
    await expect(page.getByText('开始新的运维对话')).toBeVisible();

    // 5) 冲刺 7 打磨：认证生命周期终点（登出）不再误报服务器故障。
    //    改密后 clear() 中止在途 me 查询曾把登录页兜底成「服务器内部错误
    //    （请求失败）INTERNAL」横幅（见第 3 步断言）；登出走同一条
    //    clear + navigate 链路，此处用刚以新密码建立的会话走完整登出，
    //    断言 /login 表单可用且无 INTERNAL 横幅。放本用例尾部是因为登出
    //    会吊销当前会话，不可与依赖共享 storageState 的用例并行（见文件
    //    头注释）。
    await page.getByRole('button', { name: 'admin' }).click();
    await page.getByRole('menuitem', { name: '退出登录' }).click();
    await expect(page).toHaveURL(/\/login/, { timeout: 20_000 });
    await expect(page.getByPlaceholder('请输入管理员密码')).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('.opsmind-login-page').getByText('INTERNAL')).toHaveCount(0);
    await expect(page.locator('.opsmind-login-page').getByText('服务器内部错误')).toHaveCount(0);
  });
});
