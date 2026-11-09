import { expect, test, type Page } from '@playwright/test';
import { AUTH_STATE_FILE } from './helpers';

// 二期冲刺第 3 轮打磨的端到端验证：
//  1) RDP 桌面真链路（修复：后端曾以 WS 二进制帧发 guacamole 指令，
//     官方 guacamole-common-js 按文本帧解析，握手成功即抛
//     `TypeError: ... indexOf is not a function`，页面永久停在
//     「正在建立 RDP 连接」）。preview 此前 /api 统一 ws:false 使该链路
//     从未被 e2e 覆盖——现在 /api/rdp 单独放行（ws:true），配合
//     globalSetup 拉起的假 guacd（fake-guacd.mjs）走完整
//     浏览器 → preview 代理 → 后端网关 → guacd 握手，并下发 sync 指令
//     驱动客户端进入「已连接」。
//  2) WinRM 连接测试失败的弹窗文案中文化（无法连接 host:port：连接被
//     被拒绝…前置；winrm 库的重复 negotiate 包装与 unknown error 占位
//     折叠）。
//  3) Redis 命令查询抽屉失败标题与命令口径一致（「命令执行失败」，
//     不再是「SQL 执行失败」）。

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

test.describe('RDP 真链路（p2 sprint 3：文本帧修复 + ws:true 代理）', () => {
  test('经 preview 代理与假 guacd 建立会话：页面进入已连接且无 pageerror', async ({ page }) => {
    const pageErrors: string[] = [];
    page.on('pageerror', (error) => pageErrors.push(error.message));

    await createServer(page, {
      name: 'p2s3-win-guacd',
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
    // 创建区默认选中唯一的 Windows 服务器，直接连接。
    await page.getByRole('button', { name: /连\s*接/ }).click();

    // WS 升级经 preview 代理（ws:true）→ 后端网关拨假 guacd 完成握手 →
    // stub 下发 sync 指令（文本帧）→ guacamole Client 转 CONNECTED。
    // 修复前：二进制帧在 onmessage 首帧即抛 TypeError（indexOf is not
    // a function），页面卡「正在建立 RDP 连接」直到 guacd 空闲断开。
    await expect(page.locator('.ant-tag', { hasText: '已连接' }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('正在建立 RDP 连接')).toBeHidden();
    await expect(page.getByText('● 录制中').first()).toBeVisible();

    // 回归断言：浏览器无未捕获异常（修复前的 TypeError 必现）。
    expect(pageErrors, pageErrors.join('\n')).toEqual([]);

    // 主动断开：会话收尾、页面给出断开提示。
    await page.getByRole('button', { name: /断\s*开/ }).click();
    await expect(page.getByText(/连接已断开/).first()).toBeVisible({ timeout: 20_000 });
  });
});

test.describe('服务器连接测试失败文案（p2 sprint 3：中文分类前置）', () => {
  test('WinRM 拒连：弹窗显示无法连接 host:port 与连接被拒绝，库冗余包装被折叠', async ({
    page,
  }) => {
    await createServer(page, {
      name: 'p2s3-win-refused',
      host: '127.0.0.1',
      winrm_port: 5985,
      os: 'windows',
      auth_method: 'password_ntlm',
      username: 'Administrator',
      password: 'not-a-real-password',
      auto_exec_enabled: false,
    });

    await page.goto('/servers');
    const row = page.locator('tr', { hasText: 'p2s3-win-refused' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const dialog = page.getByRole('dialog', { name: /测试连接 · p2s3-win-refused/ });
    await expect(dialog).toBeVisible({ timeout: 20_000 });
    const alert = dialog.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });

    // 通道化标题 + 中文分类短语前置（修复前：三层英文包装且同一短语
    // 重复两次，connection refused 埋在 270 字符末尾）。
    await expect(alert.locator('.ant-alert-title')).toContainText('WinRM 连接测试失败');
    await expect(alert.locator('.ant-alert-title')).toContainText(
      '无法连接 127.0.0.1:5985：连接被拒绝',
    );
    await expect(alert.locator('.ant-alert-title')).toContainText('connection refused');
    // winrm 库的占位前缀消失；negotiate 包装只剩一层（修复前同一短语
    // 连续重复两次——保留的一层是库对 NTLM 加密协商的真实语义）。
    await expect(alert.locator('.ant-alert-title')).not.toContainText('unknown error');
    const title = (await alert.locator('.ant-alert-title').textContent()) ?? '';
    expect((title.match(/negotiate message encryption/g) ?? []).length).toBeLessThanOrEqual(1);
  });
});

test.describe('Redis 命令查询抽屉失败文案（p2 sprint 3：命令口径）', () => {
  test('执行 PING 失败：标题是命令执行失败而非 SQL 执行失败', async ({ page }) => {
    // 指向无服务的本机端口：查询立即以 dial refused 失败。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: ORIGIN },
      data: {
        name: 'p2s3-redis-down',
        engine: 'redis',
        host: '127.0.0.1',
        port: 16379,
        redis_db: 0,
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/databases');
    const row = page.locator('tr', { hasText: 'p2s3-redis-down' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: '查询' }).click();

    // 抽屉自身是命令口径：标题「命令查询 · p2s3-redis-down」。
    const drawer = page.locator('.ant-drawer', { hasText: '命令查询 · p2s3-redis-down' });
    await expect(drawer).toBeVisible({ timeout: 20_000 });

    // 初始命令 PING 已就位，直接执行。
    await drawer.getByRole('button', { name: /执行查询/ }).click();
    const alert = drawer.locator('.ant-alert-error');
    await expect(alert).toBeVisible({ timeout: 30_000 });

    // 修复前：标题是「SQL 执行失败（查询失败：dial tcp …）」，与抽屉的
    // 命令控制台定位矛盾。
    await expect(alert.locator('.ant-alert-title')).toContainText('命令执行失败');
    await expect(alert.locator('.ant-alert-title')).toContainText('connection refused');
    await expect(alert.locator('.ant-alert-title')).not.toContainText('SQL 执行失败');
  });
});
