import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 四期冲刺 4 打磨（三项 UI 修复的等价状态复现，均 route mock 不触真实通道）：
// 1) 库值 none 的「重新探测」改走 POST test（连接测试顺带探测写回——容器
//    接口对库值 none 直接 400 不再探测，原按钮 refetch 列表点击后无任何
//    变化）：点击后 POST /api/servers/:id/test 确实发出；探测写回 docker
//    后空态翻为容器表格；仍 none 时给出明确反馈 Alert；test 失败给错误
//    横幅（walk8/walk3）。
// 2) 无名称容器（name=''）操作确认弹窗正文回退 12 位短 ID（与标题/表格/
//    日志抽屉同口径，walk9）。
// 3) exited 容器行「停止」禁用 + Tooltip 说明（注定 CLI 失败，浪费一轮
//    审批）；启动/重启保留（walk8）。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';

/** 创建独立测试服务器（127.0.0.1:1），返回 (id, name)。 */
async function createServer(page: import('@playwright/test').Page, prefix: string) {
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name: `${prefix}-${Date.now()}`,
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
  const { id } = (await created.json()) as { id: string };
  return { id, name: `${prefix}-${id.slice(0, 8)}` };
}

function serverPayload(id: string, name: string, runtime: string) {
  return {
    id,
    name,
    device_kind: 'server',
    os: 'linux',
    netdev_brand: '',
    access_proto: 'ssh',
    host: '127.0.0.1',
    port: 1,
    winrm_port: 5985,
    username: 'e2e',
    auth_method: 'password',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: runtime,
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-10-27T08:00:00Z',
    updated_at: '2026-10-27T08:00:00Z',
  };
}

test('重新探测走 POST test：写回 docker 后空态翻为容器表格', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s4-reprobe');

  // POST test 未发生前库值恒 none（sprint 3 的 CONTAINER_UNSUPPORTED 失效
  // 会重取 servers，此时也必须仍 none，否则空态提前消失）；test 成功后
  // （后端顺带探测写回 docker）翻为 docker。
  let tested = false;
  const serversRuntimes: string[] = [];
  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    const runtime = tested ? 'docker' : 'none';
    serversRuntimes.push(runtime);
    await route.fulfill({ json: { items: [serverPayload(serverId, serverName, runtime)] } });
  });

  // 容器列表：test 前按库值 none 返回 400（后端真实行为）；test 后返回
  // 探测写回成功的列表。
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    if (!tested) {
      await route.fulfill({
        status: 400,
        json: {
          error: {
            code: 'CONTAINER_UNSUPPORTED',
            message: 'no supported container runtime',
            request_id: 'req-p4s4-1',
            details: null,
          },
        },
      });
      return;
    }
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: 'aa11bb22cc33dd44ee55f66f',
            name: 'p4s4-rebooted-web',
            image: 'nginx:1.27',
            state: 'running',
            status: 'Up 5 minutes (healthy)',
            ports: [],
            created: '2026-10-27T07:00:00Z',
          },
        ],
      },
    });
  });

  const testRequests: string[] = [];
  await page.route(`**/api/servers/${serverId}/test`, async (route) => {
    testRequests.push(route.request().method());
    tested = true;
    await route.fulfill({
      json: {
        ok: true,
        latency_ms: 12,
        fingerprint: '',
        first_seen: false,
        server_version: 'Linux 6.x',
      },
    });
  });

  try {
    await page.goto('/containers');
    await expect(page.getByRole('heading', { name: '容器' })).toBeVisible();

    // 七期：库值 none 的服务器不再被默认选中（此前进页即 400 红色横幅）。
    // 空态改为「请先选择支持容器运行时的服务器」，并保留「重新探测」入口；
    // 点击前无任何 test 请求（原实现 refetch 列表不触发探测）。
    await expect(page.getByText(/请先选择支持容器运行时的服务器/).first()).toBeVisible({
      timeout: 20_000,
    });
    const reprobe = page.getByRole('button', { name: '重新探测' });
    await expect(reprobe).toBeVisible();
    expect(testRequests).toHaveLength(0);

    await reprobe.click();

    // 核心断言：真实探测入口被调用（POST，非 GET 列表重取）。
    await expect.poll(() => testRequests.length).toBeGreaterThanOrEqual(1);
    expect(testRequests.every((method) => method === 'POST')).toBe(true);

    // 探测写回 docker：空态翻为表格，容器行渲染；servers 至少重取一次
    // 且最后一次为 docker。
    await expect(page.getByText('p4s4-rebooted-web').first()).toBeVisible();
    expect(serversRuntimes.length).toBeGreaterThanOrEqual(2);
    expect(serversRuntimes[serversRuntimes.length - 1]).toBe('docker');
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});

test('重新探测后仍无运行时给出明确反馈；test 失败展示错误横幅', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s4-still-none');

  // 库值恒 none：POST test 成功但探测仍未发现 docker/podman。
  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({ json: { items: [serverPayload(serverId, serverName, 'none')] } });
  });
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    await route.fulfill({
      status: 400,
      json: {
        error: {
          code: 'CONTAINER_UNSUPPORTED',
          message: 'no supported container runtime',
          request_id: 'req-p4s4-2',
          details: null,
        },
      },
    });
  });

  // POST test：第 1 次成功（连接通、仍无运行时）；第 2 次失败（连接不通）。
  let testCalls = 0;
  await page.route(`**/api/servers/${serverId}/test`, async (route) => {
    testCalls += 1;
    if (testCalls === 1) {
      await route.fulfill({
        json: {
          ok: true,
          latency_ms: 12,
          fingerprint: '',
          first_seen: false,
          server_version: 'Linux 6.x',
        },
      });
      return;
    }
    await route.fulfill({
      status: 502,
      json: {
        error: {
          code: 'SERVER_TEST_FAILED',
          message: 'ssh connection test failed: 无法连接 127.0.0.1:1：连接被拒绝',
          request_id: 'req-p4s4-3',
          details: null,
        },
      },
    });
  });

  try {
    await page.goto('/containers');
    const reprobe = page.getByRole('button', { name: '重新探测' });
    await expect(reprobe).toBeVisible({ timeout: 20_000 });

    // 第 1 次：test 成功、仍 none → 明确反馈 Alert（不再点击后无变化）。
    await reprobe.click();
    await expect(
      page.getByText(/已重新测试连接并探测运行时：仍未检测到受支持的容器运行时/),
    ).toBeVisible();

    // 第 2 次：test 失败 → 错误横幅（describeFailure 按 ssh 前缀本地化为
    // 「SSH 连接测试失败（…）」+ 错误码小字 + request_id）。
    await reprobe.click();
    await expect(page.getByText(/SSH 连接测试失败/).first()).toBeVisible();
    await expect(page.getByText(/req-p4s4-3/).first()).toBeVisible();
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});

test('无名称容器操作确认弹窗正文回退 12 位短 ID（与标题同口径）', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s4-anon');
  const anonId = '1f2e3d4c5b6a7890abcdef12';

  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({ json: { items: [serverPayload(serverId, serverName, 'docker')] } });
  });
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: anonId,
            name: '',
            image: 'redis:7',
            state: 'running',
            status: 'Up 1 hour',
            ports: [],
            created: '2026-10-27T06:00:00Z',
          },
        ],
      },
    });
  });

  try {
    await page.goto('/containers');
    const anonRow = page.getByRole('row').filter({ hasText: '1f2e3d4c5b6a' });
    await expect(anonRow).toBeVisible({ timeout: 20_000 });

    // 打开停止确认弹窗。
    await anonRow.getByRole('button', { name: /停\s*止/ }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText('将申请执行的命令')).toBeVisible();

    // 标题回退（原有行为，回归锁定）。
    await expect(dialog.getByText(`停止容器 · ${anonId.slice(0, 12)}`)).toBeVisible();
    // 正文回退（修复点）：名称位置渲染短 ID，不再空白。
    const body = dialog.getByText(/即将对容器/);
    await expect(body).toContainText(`即将对容器 ${anonId.slice(0, 12)}`);
    await expect(body).toContainText(`（服务器 ${serverName}）`);
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});

test('exited 容器行「停止」禁用并带 Tooltip；启动/重启保留', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s4-exited');
  const runningId = '5f6e7d8c9b0a1a2b3c4d5e6f';
  const exitedId = '99aabbccddeeff0011223344';

  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({ json: { items: [serverPayload(serverId, serverName, 'docker')] } });
  });
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: runningId,
            name: 'p4s4-alive-web',
            image: 'nginx:1.27',
            state: 'running',
            status: 'Up 2 hours (healthy)',
            ports: [],
            created: '2026-10-27T05:00:00Z',
          },
          {
            id: exitedId,
            name: 'p4s4-done-1',
            image: 'busybox:1.36',
            state: 'exited',
            status: 'Exited (0) 3 hours ago',
            ports: [],
            created: '2026-10-26T05:00:00Z',
          },
        ],
      },
    });
  });

  try {
    await page.goto('/containers');
    const exitedRow = page.getByRole('row').filter({ hasText: 'p4s4-done-1' });
    const runningRow = page.getByRole('row').filter({ hasText: 'p4s4-alive-web' });
    await expect(exitedRow).toBeVisible({ timeout: 20_000 });
    await expect(runningRow).toBeVisible();

    // exited 行：停止禁用；启动/重启仍可用（对已停止容器合法）。
    const exitedStop = exitedRow.getByRole('button', { name: /停\s*止/ });
    await expect(exitedStop).toBeDisabled();
    await expect(exitedRow.getByRole('button', { name: /启\s*动/ })).toBeEnabled();
    await expect(exitedRow.getByRole('button', { name: /重\s*启/ })).toBeEnabled();

    // running 行停止不受影响（回归）。
    await expect(runningRow.getByRole('button', { name: /停\s*止/ })).toBeEnabled();

    // 禁用原因经 Tooltip 说明（span 包裹使 disabled 按钮可悬浮触发）。
    await exitedStop.hover();
    await expect(page.getByText('容器已停止，无需停止')).toBeVisible();
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});
