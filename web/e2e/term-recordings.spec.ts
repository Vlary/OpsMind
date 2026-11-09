import { expect, test, type Page } from '@playwright/test';
import { AUTH_STATE_FILE } from './helpers';

// F1 二期 E2E（必做两条）：
// 1) 终端页能打开并出现终端与断开按钮；
// 2) 回放页列表能渲染。
//
// p2 sprint 5 起终端第 1 条改真链路：preview 代理已对 /api/term 放行
// WS 升级，后端经 globalSetup 拉起的假 sshd（cmd/e2esshd）建立真实
// PTY——此前 REST 全 mock 且 WS 必然握手失败，只能验证「异常断开时
// 保留空终端」的兜底渲染（头注释自认后端集成后应去 mock，本期兑现）。
// 回放相关用例仍按内容 mock（.cast 由 page.route 下发，不涉 WS）。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';
const FAKE_SSH_PORT = 18083;

async function createSshServer(page: Page, name: string): Promise<string> {
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name,
      host: '127.0.0.1',
      port: FAKE_SSH_PORT,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-ssh-pass',
      auto_exec_enabled: false,
    },
  });
  expect(created.status()).toBe(201);
  const body = (await created.json()) as { id: string };
  return body.id;
}

test('终端页能打开并出现终端与断开按钮（真链路）', async ({ page }) => {
  const name = 'p2s5-term-basic';
  await createSshServer(page, name);
  await page.goto('/term');
  await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();

  // 显式选中本用例的假 sshd 服务器（默认第一台 SSH 服务器是 seed 的
  // 假地址机，拨不通 PTY）。
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  // 下拉为 antd 虚拟滚动（只渲染视口内选项）：全量并行时其它 spec 累积
  // 的 e2e-* 服务器会把本用例新建的服务器（列表按 created_at 升序）排
  // 到视口之外，直接 locator 点击等不到 DOM 超时（p3 sprint 6 实测 flaky，
  // 同族 4207cd8 的分页溢出）。方向键导航驱动虚拟列表滚到目标项再点选。
  const option = page.locator('.ant-select-item-option', { hasText: name }).first();
  for (let i = 0; i < 60 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();
  await page.getByRole('button', { name: /打开终端/ }).click();

  // 真链路：PTY 建立，假 sshd 的 banner 渲染进 xterm。
  const terminal = page.locator('[data-testid="term-viewport"] .xterm');
  await expect(terminal).toBeVisible({ timeout: 20_000 });
  await expect(terminal.locator('.xterm-viewport')).toBeVisible();
  await expect(
    page.locator('[data-testid="term-viewport"]').getByText(/fake-sshd ready/),
  ).toBeVisible({ timeout: 20_000 });

  const disconnect = page.getByRole('button', { name: /断\s*开/ });
  await expect(disconnect).toBeVisible();
  await expect(disconnect).toBeEnabled();

  // 页头标识：录制中 + 会话时长（契约：页头显示会话时长与「录制中」）。
  await expect(page.getByText('● 录制中')).toBeVisible();
  await expect(page.getByText(/时长 \d{1,2}:\d{2}/)).toBeVisible();

  // 会话列表出现该活动会话（进行中）。
  const rows = page.locator('.ant-table-row');
  const row = rows.filter({ hasText: name }).first();
  await expect(row).toBeVisible();
  await expect(row).toContainText('进行中');
  // meta 文案（p2 sprint 2）：username 是发起者（契约 v1 恒为 admin），
  // 与服务器登录用户无关——「Linux SSH · 由 admin 发起」消除「以 admin
  // 登录服务器」的误读。
  await expect(row).toContainText('Linux SSH · 由 admin 发起');

  // 收尾：断开，不遗留活动会话（同服务器单会话约束）。
  await disconnect.click();
  await expect(page.locator('.ant-tag', { hasText: '已结束' }).first()).toBeVisible({
    timeout: 20_000,
  });
});

test('回放页列表能渲染', async ({ page }) => {
  await page.route('**/api/term/recordings**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });
  await page.route('**/api/rdp/recordings**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });

  await page.goto('/recordings');
  await expect(page.getByRole('heading', { name: '回放' })).toBeVisible();

  // 终端录制列表：空态明确。
  await expect(page.getByTestId('term-recordings-tab')).toBeVisible();
  await expect(page.getByText('暂无终端录制')).toBeVisible({ timeout: 20_000 });

  // RDP 录制列表：元数据区 + 「浏览器内播放 v1 不支持」标注 + 空态。
  await page.getByTestId('rdp-recordings-tab').click();
  await expect(
    page.getByText('RDP 录像为 guacd 原生 .guac 格式，浏览器内播放 v1 不支持'),
  ).toBeVisible();
  await expect(page.getByText('暂无 RDP 录制')).toBeVisible({ timeout: 20_000 });
});

// ---------------------------------------------------------------------------
// p2 sprint 2 打磨（走查 w1/w2/w5b）：回放静默段进度插值、meta 发起者
// 文案、RDP 网关握手失败的可见提示。
// ---------------------------------------------------------------------------

const RECORDING_ID = 'e2e-term-0000-4000-8000-0000000000ff';

function terminalRecording() {
  const now = new Date().toISOString();
  return {
    id: RECORDING_ID,
    server_id: 'seed-server',
    server_name: 'e2e-fake-host',
    os: 'linux',
    username: 'admin',
    bytes_written: 2048,
    started_at: now,
    ended_at: now,
    created_at: now,
    updated_at: now,
  };
}

/**
 * 带 10s 静默段的 .cast（走查 w2 实测形态：t≈0.04 一批输出后直到
 * t≈10.1 才有下一批）——修复前播放 2.5s 时间仍冻结在 0:00/0:10。
 */
const QUIET_CAST = [
  JSON.stringify({ version: 2, width: 120, height: 40 }),
  JSON.stringify([0.04, 'o', '$ whoami\r\n']),
  JSON.stringify([10.1, 'o', 'done\r\n']),
].join('\n');

test('回放播放中静默段进度连续推进（插值心跳）', async ({ page }) => {
  await page.route('**/api/term/recordings**', async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === `/api/term/recordings/${RECORDING_ID}`) {
      await route.fulfill({
        status: 200,
        contentType: 'application/octet-stream',
        body: QUIET_CAST,
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [terminalRecording()] }),
    });
  });
  await page.route('**/api/rdp/recordings**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });

  await page.goto('/recordings');
  // 录制列表 meta：发起者文案（同会话列表）。
  const rows = page.locator('.ant-table-row');
  await expect(rows.filter({ hasText: 'Linux SSH · 由 admin 发起' }).first()).toBeVisible({
    timeout: 20_000,
  });

  // 打开回放 → 播放。
  await page.getByRole('button', { name: '回放' }).click();
  const time = page.locator('.opsmind-player-time');
  await expect(time).toBeVisible({ timeout: 20_000 });
  await expect(time).toHaveText('0:00 / 0:10');
  await page.getByRole('button', { name: '播放' }).click();

  // 静默段（0.04 → 10.1 无事件）：插值心跳（4 次/秒）让时间推进出
  // 0:00——修复前 position 只随事件落地更新，2.5s 后仍显示 0:00。
  await expect(time).toHaveText(/^0:0[1-9] \/ 0:10$/, { timeout: 15_000 });
});

// ---------------------------------------------------------------------------
// p2 sprint 5：短录制（全部事件集中在开头几毫秒）不再出现可拖的
// 「0:00 / 0:00」进度条——修复前 duration≈0.002≠0 使 Slider 不禁用，
// step=0.1 下任何拖拽都提交 seek(0)，reset 后 t≤0 无事件可重放、屏幕
// 永久空白（走查实测 nonEmpty 7→0 不恢复）。修复：时长小于步长（0.1s）
// 视为空录制——时长显示「—」、禁用拖拽；播放仍可用。
// ---------------------------------------------------------------------------

const DEGENERATE_CAST = [
  JSON.stringify({ version: 2, width: 120, height: 40 }),
  JSON.stringify([0.002, 'o', 'fake-sshd ready (e2e terminal link)\r\n$ ']),
].join('\n');

test('短录制回放：时长显示「—」、进度条禁用；播放仍能重放内容', async ({ page }) => {
  await page.route('**/api/term/recordings**', async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === `/api/term/recordings/${RECORDING_ID}`) {
      await route.fulfill({
        status: 200,
        contentType: 'application/octet-stream',
        body: DEGENERATE_CAST,
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [terminalRecording()] }),
    });
  });
  await page.route('**/api/rdp/recordings**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [] }),
    });
  });

  await page.goto('/recordings');
  await page.getByRole('button', { name: '回放' }).click();

  const time = page.locator('.opsmind-player-time');
  await expect(time).toBeVisible({ timeout: 20_000 });
  await expect(time).toHaveText('—'); // 修复前：「0:00 / 0:00」
  await expect(page.locator('.opsmind-player-slider')).toHaveClass(/ant-slider-disabled/);

  // 播放不受影响：事件立即重放，屏幕出现内容而非空白。
  await page.getByRole('button', { name: '播放' }).click();
  await expect(page.locator('[data-testid="player-viewport"] .xterm-rows')).toContainText(
    'fake-sshd ready',
    { timeout: 10_000 },
  );
});

test('RDP WS 握手失败给出失败提示（不再永久停在正在建立连接）', async ({ page }) => {
  // 一台 windows 服务器 + 会话创建 201（REST mock）。
  await page.route('**/api/servers**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        items: [
          {
            id: 'e2e-win-0000-4000-8000-000000000001',
            name: 'win-e2e',
            host: '127.0.0.1',
            port: 3389,
            os: 'windows',
            auth_method: 'password',
            status: 'unknown',
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
        ],
      }),
    });
  });
  const rdpSession = {
    id: 'e2e-rdp-0000-4000-8000-000000000001',
    server_id: 'e2e-win-0000-4000-8000-000000000001',
    server_name: 'win-e2e',
    width: 1280,
    height: 800,
    dpi: 96,
    status: 'active',
    ended_reason: '',
    has_recording: false,
    bytes_written: 0,
    started_at: new Date().toISOString(),
    last_activity_at: new Date().toISOString(),
    ended_at: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  await page.route('**/api/rdp/sessions**', async (route) => {
    if (route.request().method() === 'POST') {
      await route.fulfill({
        status: 201,
        contentType: 'application/json',
        body: JSON.stringify({
          session: rdpSession,
          ws_url: `/api/rdp/sessions/${rdpSession.id}/ws`,
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

  await page.goto('/rdp');
  await expect(page.getByRole('heading', { name: '桌面', exact: true })).toBeVisible();
  await page.getByRole('button', { name: /连\s*接/ }).click();

  // guacamole WebSocketTunnel 发起 ws:// 升级——preview 的 /api 代理
  // ws:false，握手必然失败（走查 w5b 同形态）。tunnel.onerror/
  // onstatechange(CLOSED) 兜底 → failed：页面给出「连接失败」提示，
  // 不再永久停留在「正在建立 RDP 连接」。
  await expect(page.getByText(/连接失败/).first()).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText('正在建立 RDP 连接')).toBeHidden();
});
