import { expect, test, type Page } from '@playwright/test';
import { AUTH_STATE_FILE } from './helpers';

// 二期冲刺第 4 轮打磨的端到端验证（实测缺陷 w4-rdp2）：
//  1) RDP 断开后秒点「新建会话」→ 后端 409 RDP_SESSION_EXISTS（服务端
//     bridge 停止+封盘+关行异步收尾，日志 "bridge goroutines did not stop
//     in time"，实测约 2.3s）。修复前页面只显示通用文案「该服务器已有
//     活动 RDP 会话（server already has an active rdp session）」；
//     现在给收尾时序解释并引导稍候重试（对齐终端 describeTermCreateFailure），
//     且「稍候重试」真实可行（等服务端收尾后重连成功）。
//  2) 断开后会话列表行「进行中/录制中…」残留 2-3 秒、与页头「已断开/
//     会话已结束」矛盾（终端与 RDP 同形态）。修复：结束时乐观把列表中
//     当前行置 closed，延迟到服务端收尾后再 invalidate 拉真实数据。
//
// RDP 走真链路（假 guacd，p2 sprint 3 基建）；终端自 p2 sprint 5 起
// 同样走真链路（preview 代理放行 /api/term 的 WS 升级 + 假 sshd），此
// 前只能 REST mock 的乐观更新用例一并改写。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';

async function createServer(page: Page, body: Record<string, unknown>) {
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: body,
  });
  expect(created.status()).toBe(201);
  return (await created.json()) as { id: string; name: string };
}

test.describe('RDP 断开后秒点新建会话（真链路）', () => {
  test('409 场景化文案引导稍候重试；列表行立即结束不与页头矛盾；稍候后重连成功', async ({
    page,
  }) => {
    await createServer(page, {
      name: 'p2s4-win-fast-retry',
      host: '127.0.0.1',
      port: 3389,
      os: 'windows',
      auth_method: 'password_ntlm',
      username: 'Administrator',
      password: 'not-a-real-password',
      auto_exec_enabled: false,
    });

    await page.goto('/rdp');
    await expect(page.getByRole('heading', { name: '桌面', exact: true })).toBeVisible();
    // 显式选择本用例的服务器：并行跑时 p2s3 的 RDP 用例已建了自己的
    // Windows 服务器，默认「第一台 Windows」可能选中它并撞上其活动
    // 会话（同服务器单会话 409，从未走到本用例要断言的链路）。
    await page.getByRole('combobox', { name: '选择 Windows 服务器' }).click();
    await page
      .locator('.ant-select-item-option', { hasText: 'p2s4-win-fast-retry' })
      .first()
      .click();
    await page.getByRole('button', { name: /连\s*接/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '已连接' }).first()).toBeVisible({
      timeout: 20_000,
    });

    // 会话列表出现该服务器的「进行中」行。
    const row = page.locator('.ant-table-row', { hasText: 'p2s4-win-fast-retry' }).first();
    await expect(row).toContainText('进行中');

    // 主动断开。
    await page.getByRole('button', { name: /断\s*开/ }).click();
    await expect(page.getByText(/连接已断开/).first()).toBeVisible({ timeout: 20_000 });

    // 项 2：页头已断开，列表行必须立即「已结束」——修复前断开只
    // invalidate 一次，服务端收尾（约 2.3s）前的 refetch 拿回 active
    // 行，「进行中 0:00 录制中…」残留 2-3 秒与页头矛盾（实测 w4-rdp2）。
    await expect(row).toContainText('已结束');
    await expect(row).not.toContainText('进行中');

    // 项 1：断开后秒点「新建会话」——服务端仍在收尾，同服务器单会话
    // 校验返回 409 RDP_SESSION_EXISTS，页面给出场景化文案而非裸通用
    // 报错（修复前：「该服务器已有活动 RDP 会话（server already has
    // an active rdp session）」，无任何引导）。
    await page.getByRole('button', { name: /新建会话/ }).click();
    const alert = page.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 20_000 });
    await expect(alert).toContainText('该服务器已有活动 RDP 会话');
    await expect(alert).toContainText('若您刚断开上一会话');
    await expect(alert).toContainText('数秒内完成收尾');
    await expect(alert).toContainText('稍候重试');
    await expect(alert).not.toContainText('server already has an active rdp session');

    // 文案引导的「稍候重试」真实可行：越过服务端收尾窗口（约 2.3s）后
    // 重连（409 已把 active 会话视图收起，「连接」按钮重新可用）。
    await page.waitForTimeout(4000);
    await page.getByRole('button', { name: /连\s*接/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '已连接' }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});

test.describe('终端断开后会话列表乐观结束（真链路：假 sshd）', () => {
  test('手动断开后列表行立即「已结束」，不再残留「进行中」与页头矛盾', async ({ page }) => {
    // 真链路（p2 sprint 5 起 WS 可升级 + 假 sshd 提供 PTY）：乐观更新在
    // 服务端收尾（约 2.3s）完成前的任何 refetch 之前就把该行置为已结束
    // ——修复前断开只 invalidate 一次，收尾窗口内 refetch 拿回 active 行
    // 盖掉状态，列表与页头「已结束」矛盾残留 2-3 秒。
    await createServer(page, {
      name: 'p2s4-term-optimistic',
      host: '127.0.0.1',
      port: 18083,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-ssh-pass',
      auto_exec_enabled: false,
    });

    await page.goto('/term');
    await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
    await page.getByRole('combobox', { name: '选择服务器' }).click();
    await page
      .locator('.ant-select-item-option', { hasText: 'p2s4-term-optimistic' })
      .first()
      .click();
    await page.getByRole('button', { name: /打开终端/ }).click();

    // PTY 建立（banner 到达）；会话列表先显示「进行中」。
    await expect(
      page.locator('[data-testid="term-viewport"]').getByText(/fake-sshd ready/),
    ).toBeVisible({ timeout: 20_000 });
    const row = page.locator('.ant-table-row', { hasText: 'p2s4-term-optimistic' }).first();
    await expect(row).toContainText('进行中');

    // 手动断开：同一次点击里页头转「已结束」、列表该行被乐观置为已结束
    // （录制大小显示「封盘中…」占位，p2 sprint 5）。
    await page.getByRole('button', { name: /断\s*开/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '已结束' }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(row).toContainText('已结束');
    await expect(row).not.toContainText('进行中');
    await expect(row).toContainText('封盘中…');
  });
});
