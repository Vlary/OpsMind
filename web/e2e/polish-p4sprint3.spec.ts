import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 四期冲刺 3 打磨（一项）：新探测出 docker 的服务器首次打开容器页后，
// 页头「运行时」徽标停留「未探测」、操作确认弹窗预览命令丢运行时前缀
// （" stop <cid>"，与批准后逐字节执行的 docker stop 不一致）。
// 根因：ContainersPage 探测成功写回（''→docker/podman，契约 2.9 约定 1）
// 路径无 qk.servers 失效——唯一失效入口是 CONTAINER_UNSUPPORTED 错误与
// 手动按钮。修复：列表成功且响应 runtime 与库值缓存不一致时同样失效。
// 场景复现（等价状态）：mock GET /api/servers 首次库值 container_runtime
// =''、失效重取后（第 ≥2 次）返回 'docker'（探测在 containers 请求内
// 同步写回完成，其后的列表请求能读到）；GET /api/servers/:id/containers
// 返回 runtime='docker' 与容器行——即后端探测成功写回后的真实状态。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';

test('探测写回后页头徽标与操作预览同步为 docker（不再停「未探测」/丢前缀）', async ({ page }) => {
  // 真实创建服务器（127.0.0.1:1，SSH 立拒不影响——列表与容器列表均
  // route mock，不触真实通道）。注册在 servers 列表 route 之前，避免
  // 拦截创建请求。
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name: `p4s3-rt-sync-${Date.now()}`,
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
  const serverName = `p4s3-rt-sync-${serverId.slice(0, 8)}`;

  const serverPayload = (runtime: string) => ({
    id: serverId,
    name: serverName,
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
  });

  // servers 列表：第 1 次（页面挂载）库值 ''；失效重取（第 ≥2 次，
  // 即探测写回完成后）返回 'docker'。
  const serversListRequests: string[] = [];
  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    const runtime = serversListRequests.length === 0 ? '' : 'docker';
    serversListRequests.push(runtime);
    await route.fulfill({ json: { items: [serverPayload(runtime)] } });
  });

  // 容器列表：200 成功只可能 docker/podman（后端 containerRuntimeFor 对
  // 其余值一律 400），响应 runtime 即探测写回结果。
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: 'aa11bb22cc33dd44ee55',
            name: 'p4s3-web',
            image: 'nginx:1.27',
            state: 'running',
            status: 'Up 2 hours (healthy)',
            ports: [],
            created: '2026-10-26T12:00:00Z',
          },
        ],
      },
    });
  });

  try {
    await page.goto('/containers');
    await expect(page.getByRole('heading', { name: '容器' })).toBeVisible();

    // 默认选中第一台（mock 列表仅此一台）：容器行渲染 = 列表成功，
    // 探测写回已发生。
    await expect(page.getByText('p4s3-web').first()).toBeVisible({ timeout: 20_000 });

    // 修复核心断言 1（页头徽标）：qk.servers 已失效重取（第 ≥2 次列表
    // 请求），页头「运行时」徽标从「未探测」翻为 Docker。下拉已收起，
    // 全局 Tag 断言不误伤。
    await expect(page.locator('.ant-tag', { hasText: 'Docker' }).first()).toBeVisible();
    await expect(page.locator('.ant-tag', { hasText: '未探测' })).toHaveCount(0);
    // 「正在自动探测一次」提示随 runtime 已知而消失（回归）。
    await expect(page.getByText(/正在自动探测一次/)).toBeHidden();

    // 修复核心断言 2（预览前缀）：停止 → confirm 阶段预览必须带运行时
    // 前缀且无前导空格（修复前 innerText 是 " stop aa11bb22cc33dd44ee55"，
    // 与批准后逐字节执行的 docker stop 不一致）。
    await page.getByRole('button', { name: '停止' }).first().click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText('将申请执行的命令')).toBeVisible();
    await expect(dialog.locator('.opsmind-codeblock')).toHaveText(
      'docker stop aa11bb22cc33dd44ee55',
    );

    // 失效确实发生：列表请求 ≥2 次（挂载 1 次 + 写回失效重取）。
    expect(serversListRequests.length).toBeGreaterThanOrEqual(2);
    expect(serversListRequests[0]).toBe('');
    expect(serversListRequests[serversListRequests.length - 1]).toBe('docker');
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});
