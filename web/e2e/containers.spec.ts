import { expect, test, type Page } from '@playwright/test';
import { execSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { AUTH_STATE_FILE } from './helpers';

// C2 四期 E2E（必做两条）：
// 1) 容器页能打开并列出空态 / 无运行时提示（真链路：seed 的
//    e2e-fake-host 指向 127.0.0.1:1，SSH 立即被拒——容器接口先同步探测，
//    通道错误按 none 返回 400 CONTAINER_UNSUPPORTED 但不写回（评审修订），
//    前端展示错误横幅（「本次探测失败」文案），下拉不禁用、可重探，
//    契约 2.9 约定 1/8.7）。
// 2) 容器日志抽屉骨架（容器列表与日志按内容 mock，不依赖真实容器/SSH）：
//    tail 选择（缺省 200 行）、刷新、复制、按行渲染与 truncated 截断提示。
// 3) p4 sprint 6（走查 D）：库值 runtime=docker 但 SSH 不可达——横幅报
//    「无法连接服务器执行容器命令」（SSH_EXEC_FAILED），不再误报
//    「daemon 未运行」（CONTAINER_CLI_FAILED）。

test.use({ storageState: AUTH_STATE_FILE });

// 用例 2 的复制反馈需要剪贴板权限（headless chromium 默认拒绝）。
test.use({ permissions: ['clipboard-write'] });

const ORIGIN = 'http://localhost:4173';

/** antd Select 虚拟滚动：方向键滚到目标项再点选（同 term spec 先例）。 */
async function selectServer(page: Page, name: string) {
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  const option = page.locator('.ant-select-item-option', { hasText: name }).first();
  for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();
}

test('容器页能打开：无运行时服务器空态说明与下拉禁用提示（真链路）', async ({ page }) => {
  await page.goto('/containers');
  await expect(page.getByRole('heading', { name: '容器' })).toBeVisible();

  // seed 的假服务器（127.0.0.1:1，SSH 立拒）：runtime='' → 选中触发探测。
  await selectServer(page, 'e2e-fake-host');

  // 探测通道错误按 none 返回 400 CONTAINER_UNSUPPORTED（不写回）→ 错误
  // 横幅（契约 8.7 固定文案；探测确认写回 none 后的 EmptyState 呈现同
  // 基础文案）。
  await expect(page.getByText(/该服务器未探测到受支持的容器运行时/).first()).toBeVisible({
    timeout: 30_000,
  });

  // 探测已结束（isError）：进行时 Alert「正在自动探测一次」不滞留；横幅
  // 对通道错误场景（未写回，库值仍 ''）给「本次探测失败」语义，不断言与
  // 库中状态矛盾的「当前运行时：none」（四期走查 T1）。
  await expect(page.getByText(/正在自动探测一次/)).toBeHidden();
  await expect(page.getByText(/本次探测失败，按无容器处理/).first()).toBeVisible();
  await expect(page.getByText(/当前运行时：none/)).toBeHidden();

  // 探测通道错误（SSH 立拒）不持久化（四期评审修订：一次 15s 超时/拨号
  // 失败曾把服务器钉死为 none，仅 POST /test 可救）——servers 失效重取
  // 后仍是「未探测」态：不出现「无容器」徽标，下拉不禁用（可重新选择
  // 触发重探）；响应侧仍按 none 返回 400（上方空态文案断言）。
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  // 只断言 e2e-fake-host 自身状态（并行全量跑下其他 spec 残留的服务器
  // 可能真实写回 none 并显示「无容器」徽标，全局 hidden 断言会误伤）。
  const option = page.locator('.ant-select-item-option', { hasText: 'e2e-fake-host' }).first();
  await expect(option).toBeVisible();
  await expect(option).not.toHaveClass(/ant-select-item-option-disabled/);
  await expect(option).not.toContainText('无容器');
  await page.keyboard.press('Escape');
});

test('容器日志抽屉骨架：tail 选择、刷新、复制与按行渲染（mock 列表与日志）', async ({ page }) => {
  // 独立服务器（127.0.0.1:1）：runtime=''（用例 1 的写回只影响
  // e2e-fake-host），可选——列表与日志由 route mock 下发。
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name: `p4-cont-logs-${Date.now()}`,
      host: '127.0.0.1',
      port: 1,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-pass',
      auto_exec_enabled: false,
    },
  });
  expect(created.status()).toBe(201);
  const serverId = ((await created.json()) as { id: string }).id;

  const containersPayload = {
    runtime: 'docker',
    items: [
      {
        id: 'c1d2e3f4a5b6',
        name: 'web-1',
        image: 'nginx:1.27',
        state: 'running',
        status: 'Up 2 hours (healthy)',
        ports: ['0.0.0.0:8080->80/tcp'],
        created: '2026-10-26T12:00:00Z',
      },
      {
        id: 'd4c3b2a1f0e9',
        name: 'pg-1',
        image: 'postgres:16',
        state: 'exited',
        status: 'Exited (0) 3 hours ago',
        ports: [],
        created: '2026-10-25T08:30:00Z',
      },
    ],
  };

  const logTailRequests: number[] = [];
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    await route.fulfill({ json: containersPayload });
  });
  await page.route(`**/api/servers/${serverId}/containers/*/logs*`, async (route) => {
    const tail = Number(new URL(route.request().url()).searchParams.get('tail') ?? '200');
    logTailRequests.push(tail);
    await route.fulfill({
      json: {
        server_id: serverId,
        container_id: 'c1d2e3f4a5b6',
        tail,
        output: 'line-1\nline-2\nline-3',
        truncated: true,
      },
    });
  });

  await page.goto('/containers');
  const option = page.locator('.ant-select-item-option', { hasText: 'p4-cont-logs-' }).first();
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();

  // mock 列表渲染：容器名与状态原文。
  await expect(page.getByText('web-1').first()).toBeVisible();
  await expect(page.getByText('nginx:1.27').first()).toBeVisible();

  // 打开日志抽屉。
  await page.getByRole('button', { name: '日志' }).first().click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('容器日志 · web-1')).toBeVisible();

  // 骨架：tail 缺省 200 行、刷新、复制、按行渲染、truncated 截断提示
  //（「刷新」限定在抽屉内——页头还有同名按钮）。
  await expect(drawer.getByText('200 行')).toBeVisible();
  await expect(drawer.getByRole('button', { name: '刷新' })).toBeVisible();
  // 六期打磨 sprint 2：日志复制收敛为统一 CopyButton（此前是文本「复制」
  // 按钮 + 手写 toast，实测 aria-label 不含「复制」）。
  await expect(drawer.getByRole('button', { name: '复制日志' })).toBeVisible();
  await expect(drawer.getByText('line-1')).toBeVisible();
  await expect(drawer.getByText('line-3')).toBeVisible();
  await expect(drawer.getByText(/日志过长已截断/)).toBeVisible();

  // 复制成功反馈（headless chromium 需 clipboard-write 权限）；统一组件的
  // 成功文案是「已复制」。
  await drawer.getByRole('button', { name: '复制日志' }).click();
  await expect(page.getByText('已复制').first()).toBeVisible();

  // 切换 tail 到 500 行：重新按需请求（key 变化），mock 收到 tail=500。
  await drawer.getByText('200 行').click();
  await page.locator('.ant-select-item-option', { hasText: '500 行' }).click();
  await expect.poll(() => logTailRequests[logTailRequests.length - 1]).toBe(500);
});

test('库值 docker 但 SSH 不可达：横幅报服务器连接失败而非 daemon（真链路，走查 D）', async ({
  page,
}) => {
  // 走查 D 复现：runtime 只能由探测写回，SSH 立拒探不回去，用 psql 直写
  // 库值 docker（同走查手法）；此后列表不再探测、直接执行 docker CLI →
  // 拨号 127.0.0.1:1 立拒。修复前误报 CONTAINER_CLI_FAILED（「daemon 未
  // 运行」），p4 sprint 6 起通道错误映射 SSH_EXEC_FAILED。
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name: `p4-cont-chan-${Date.now()}`,
      host: '127.0.0.1',
      port: 1,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-pass',
      auto_exec_enabled: false,
    },
  });
  expect(created.status()).toBe(201);
  const serverId = ((await created.json()) as { id: string }).id;

  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  execSync(
    `docker compose exec -T db psql -U aiops -d aiops_e2e -v ON_ERROR_STOP=1` +
      ` -c "UPDATE servers SET container_runtime='docker' WHERE id='${serverId}'"`,
    { cwd: repoRoot, stdio: ['ignore', 'pipe', 'ignore'] },
  );

  await page.goto('/containers');
  const option = page.locator('.ant-select-item-option', { hasText: 'p4-cont-chan-' }).first();
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();

  // 拨号立拒（connection refused）即时返回：横幅文案指向服务器连接，
  // 错误码 SSH_EXEC_FAILED（description 小字），不再出现「daemon 未运行」。
  await expect(page.getByText(/无法连接服务器执行容器命令/).first()).toBeVisible({
    timeout: 30_000,
  });
  await expect(page.getByText(/SSH_EXEC_FAILED/).first()).toBeVisible();
  await expect(page.getByText(/daemon 未运行/)).toBeHidden();
});
