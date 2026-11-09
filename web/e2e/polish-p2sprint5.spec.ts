import { expect, test, type Page } from '@playwright/test';
import { AUTH_STATE_FILE } from './helpers';

// 二期冲刺第 5 轮打磨的端到端验证（真链路：真后端 + 假 sshd，无 mock）：
//
//  1) PTY 尺寸同步（w5-pty1）：修复前 TermLive 挂载即 applyFit 上报视口
//     真实尺寸，此时 WS 尚在 CONNECTING，ws.ts 的 send 对非 OPEN 静默
//     丢弃；ResizeObserver 首回调 fit 结果相同、controller 数值未变不再
//     发送——此后服务端 PTY 永远停在创建时的 120×40（走查实测零 resize
//     帧、全部会话 cols=120 rows=40，本地 xterm 实际 25 行左右，换行
//     错位、vim/htop 全屏 TUI 必乱）。修复：CONNECTING 期帧排队、onopen
//     后 flush（api/ws.ts）。断言链：浏览器 xterm 行数 == 假 sshd 记录的
//     PTY 最终尺寸（window-change 实际到达）≠ 创建默认 120×40；改视口
//     后 PTY 跟随。
//
//  2) 断开后「录制大小」占位（w5-size2）：修复前乐观结束行保留缓存里的
//     旧 bytes_written（建会话时为 0），列表显示误导性的「0 B」2-3 秒。
//     修复：乐观行 bytes_written 置 BYTES_FINALIZING 占位，渲染「封盘中…」，
//     延迟 invalidate 拉回真实大小后衔接。
//
// 假 sshd 由 globalSetup 拉起（cmd/e2esshd）：SSH 端口 18083 供后端 sshx
// 拨号，控制 HTTP 端口 18084 供本文件查询 PTY 尺寸记录。会话尺寸不落库
// （resize 只更新 termx 内存态），真值断言以假 sshd 的 PTY 记录为准。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';
const FAKE_SSH_PORT = 18083;
const FAKE_SSH_HTTP = 'http://127.0.0.1:18084';
const SSH_PASSWORD = 'e2e-ssh-pass';

interface WindowSize {
  cols: number;
  rows: number;
}

interface SshdStatus {
  initial: WindowSize | null;
  changes: WindowSize[];
  final: WindowSize | null;
  shells: number;
}

async function sshdStatus(): Promise<SshdStatus> {
  const response = await fetch(`${FAKE_SSH_HTTP}/status`);
  return (await response.json()) as SshdStatus;
}

async function createSshServer(page: Page, name: string): Promise<{ id: string }> {
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name,
      host: '127.0.0.1',
      port: FAKE_SSH_PORT,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: SSH_PASSWORD,
      auto_exec_enabled: false,
    },
  });
  expect(created.status()).toBe(201);
  return (await created.json()) as { id: string };
}

/** 显式选中本用例的服务器（默认第一台 SSH 服务器是 seed 的假地址机）。 */
async function selectServer(page: Page, name: string) {
  await page.goto('/term');
  await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  await page.locator('.ant-select-item-option', { hasText: name }).first().click();
}

/** 浏览器 xterm 的实际行数（.xterm-rows 的渲染行数）。 */
function termRows(page: Page) {
  return page.evaluate(
    () => document.querySelectorAll('[data-testid="term-viewport"] .xterm-rows > div').length,
  );
}

/** 轮询假 sshd 直到 PTY 最终尺寸等于期望行数（同步链路完成）。 */
async function waitForPtyRows(page: Page, expectedRows: number, timeout = 15_000) {
  const deadline = Date.now() + timeout;
  let status: SshdStatus | null = null;
  while (Date.now() < deadline) {
    status = await sshdStatus();
    if (status.final && status.final.rows === expectedRows) {
      return status;
    }
    await page.waitForTimeout(200);
  }
  throw new Error(
    `PTY rows never synced to ${expectedRows} within ${timeout}ms; last status: ${JSON.stringify(status)}`,
  );
}

/** 轮询 DOM 行数直到满足谓词（ResizeObserver → fit 是异步的）。 */
async function waitForTermRows(
  page: Page,
  predicate: (rows: number) => boolean,
  timeout = 10_000,
): Promise<number> {
  const deadline = Date.now() + timeout;
  let rows = 0;
  while (Date.now() < deadline) {
    rows = await termRows(page);
    if (predicate(rows)) {
      return rows;
    }
    await page.waitForTimeout(150);
  }
  throw new Error(`xterm rows never satisfied predicate within ${timeout}ms (last=${rows})`);
}

test.describe('终端 PTY 尺寸同步（真链路：假 sshd）', () => {
  test('挂载即上报视口尺寸；改窗口后 PTY 跟随（修复前永远停在 120×40）', async ({ page }) => {
    test.setTimeout(120_000);
    await createSshServer(page, 'p2s5-sshd-sync');
    await fetch(`${FAKE_SSH_HTTP}/reset`, { method: 'POST' });

    await selectServer(page, 'p2s5-sshd-sync');
    await page.getByRole('button', { name: /打开终端/ }).click();

    // 假 sshd 的 banner 出现在 xterm：WS→PTY 双向链路建立（此前 mock
    // 用例从未到达过真实 PTY）。
    await expect(page.locator('[data-testid="term-viewport"] .xterm')).toBeVisible({
      timeout: 20_000,
    });
    await expect(
      page.locator('[data-testid="term-viewport"]').getByText(/fake-sshd ready/),
    ).toBeVisible({
      timeout: 20_000,
    });

    // 核心断言：PTY 最终尺寸 == 浏览器 xterm 实际行数，且确有 resize 帧
    // 到达（changes ≥ 1）——修复前 initial 恒为创建默认 120×40 且零
    // window-change，final.rows=40 ≠ 本地 xterm 行数。
    const domRows = await waitForTermRows(page, (rows) => rows > 0 && rows !== 40);
    const synced = await waitForPtyRows(page, domRows);
    expect(synced.final?.rows).toBe(domRows);
    expect(synced.changes.length).toBeGreaterThanOrEqual(1);
    expect(synced.initial).toEqual({ cols: 120, rows: 40 }); // 创建默认（缺陷形态的对照）

    // 改视口尺寸：本地 fit → resize 帧 → PTY window-change 跟随。
    await page.setViewportSize({ width: 1000, height: 520 });
    const domRowsAfter = await waitForTermRows(page, (rows) => rows !== domRows);
    const resynced = await waitForPtyRows(page, domRowsAfter);
    expect(resynced.final?.rows).toBe(domRowsAfter);
    expect(resynced.changes.length).toBeGreaterThan(synced.changes.length);

    // 手动断开收尾（不遗留活动会话影响后续用例）。
    await page.getByRole('button', { name: /断\s*开/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '已结束' }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});

test.describe('断开后会话列表录制大小占位（真链路）', () => {
  test('乐观行先显示「封盘中…」，延迟刷新后被真实大小覆盖（不再误导性显示 0 B）', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    await createSshServer(page, 'p2s5-sshd-size');
    await selectServer(page, 'p2s5-sshd-size');
    await page.getByRole('button', { name: /打开终端/ }).click();

    // banner 到达 = PTY 建立 = 录制里必然有输出字节。
    await expect(
      page.locator('[data-testid="term-viewport"]').getByText(/fake-sshd ready/),
    ).toBeVisible({
      timeout: 20_000,
    });
    const row = page.locator('.ant-table-row', { hasText: 'p2s5-sshd-size' }).first();
    await expect(row).toContainText('进行中');
    await expect(row).toContainText('录制中…');

    // 断开：乐观行立即「已结束」+「封盘中…」（修复前：误导性的「0 B」，
    // 2-3 秒后才被真实大小覆盖）。
    await page.getByRole('button', { name: /断\s*开/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '已结束' }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(row).toContainText('已结束');
    await expect(row).toContainText('封盘中…');
    await expect(row).not.toContainText('0 B');

    // 延迟 invalidate（SESSION_END_REFETCH_DELAY_MS=3s）拉回真实大小：
    // banner 保证 bytes_written > 0，格式化为「N B / N.N KB」。
    await expect(row).toContainText(/(\d+ B|\d+(\.\d+)? KB)/, { timeout: 15_000 });
    await expect(row).not.toContainText('封盘中…');
  });
});
