import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 四期冲刺 5 打磨（walk5）：容器操作审批弹窗命令区标签按阶段取文案。
// 修复前 confirm/其余两档的三元让 result 阶段（decide 已完成、同屏 Alert
// 已给出执行结果）仍显示「待执行命令」——文案语义与状态矛盾，用户可能
// 误以为尚未执行。修复后三阶段：confirm「将申请执行的命令」/ pending
// 「待执行命令」/ result「已执行命令」（denied 细分「未执行命令」，命令
// 从未执行，与 outcome「已拒绝，命令未执行」同口径）。全链路 route mock，
// 不触真实执行通道。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';

const CONTAINER_ID = '7a3f9c1d2e4b5a6f7c8d9e0f';

/** 创建独立测试服务器（127.0.0.1:1，SSH 立拒不影响——列表/审批均 mock）。 */
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

function serverPayload(id: string, name: string) {
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
    container_runtime: 'docker',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-10-27T08:00:00Z',
    updated_at: '2026-10-27T08:00:00Z',
  };
}

function pendingApproval(serverId: string) {
  return {
    id: '9d8c7b6a-5e4f-3a2b-1c0d-9e8f7a6b5c4d',
    conversation_id: null,
    tool_call_id: null,
    source: 'console',
    server_id: serverId,
    server_name: 'walk5-docker-host',
    command: `docker restart ${CONTAINER_ID}`,
    risk_level: 'needs_approval',
    rule_id: 'CONTAINER_CHANGE',
    reason: '容器变更需人工批准',
    status: 'pending',
    requested_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 120_000).toISOString(),
    decided_at: null,
    decided_by: null,
    note: '',
  };
}

/** mock 单台 docker 服务器（默认选中）与单容器列表。 */
async function mockSingleContainer(
  page: import('@playwright/test').Page,
  serverId: string,
  serverName: string,
) {
  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({ json: { items: [serverPayload(serverId, serverName)] } });
  });
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: CONTAINER_ID,
            name: 'walk5-web-1',
            image: 'nginx:1.27',
            state: 'running',
            status: 'Up 3 hours (healthy)',
            ports: [],
            created: '2026-10-27T06:00:00Z',
          },
        ],
      },
    });
  });
  await page.route(`**/api/servers/${serverId}/containers/*/action`, async (route) => {
    await route.fulfill({ json: { approval: pendingApproval(serverId) } });
  });
}

test('批准路径：命令区标签随 confirm → pending → result 三阶段切换', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s5-approve');
  await mockSingleContainer(page, serverId, serverName);
  // decide mock：批准 → execution exit 0（walk5 w1-05 同形状）。
  await page.route('**/api/approvals/*/decide', async (route) => {
    await route.fulfill({
      json: {
        approval: {
          ...pendingApproval(serverId),
          status: 'approved',
          decided_at: new Date().toISOString(),
          decided_by: 'admin',
        },
        execution: {
          exit_code: 0,
          output: 'walk5-web-1',
          truncated: false,
          duration_ms: 0,
          error_code: undefined,
        },
      },
    });
  });

  try {
    await page.goto('/containers');
    const row = page.getByRole('row').filter({ hasText: 'walk5-web-1' });
    await expect(row).toBeVisible({ timeout: 20_000 });

    // confirm：审批未创建，预告将申请执行的命令。
    await row.getByRole('button', { name: /重\s*启/ }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText('将申请执行的命令')).toBeVisible();
    await dialog.getByRole('button', { name: '创建审批' }).click();

    // pending：审批已创建待决策，待执行命令。
    await expect(dialog.getByText('待执行命令')).toBeVisible();
    await expect(dialog.getByText('将申请执行的命令')).toHaveCount(0);

    // result（批准 exit 0）：命令已执行完毕，标签必须是已执行命令——
    // 修复前这里仍显示「待执行命令」，与同屏绿色 Alert「执行成功」矛盾。
    await dialog.getByRole('button', { name: /批\s*准/ }).click();
    await expect(dialog.getByText(/执行成功（exit 0/)).toBeVisible();
    await expect(dialog.getByText('已执行命令')).toBeVisible();
    await expect(dialog.getByText('待执行命令')).toHaveCount(0);
    // result 态只剩关闭（回归锁定）。
    const body = dialog.locator('.ant-modal-body');
    await expect(body.getByRole('button', { name: /关\s*闭/ })).toBeVisible();
    await expect(body.getByRole('button', { name: /批\s*准/ })).toHaveCount(0);
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});

test('拒绝路径：result 态标签为未执行命令，与「已拒绝，命令未执行」同口径', async ({ page }) => {
  const { id: serverId, name: serverName } = await createServer(page, 'p4s5-deny');
  await mockSingleContainer(page, serverId, serverName);
  // decide mock：拒绝 → 命令从未执行，execution 恒 null。
  await page.route('**/api/approvals/*/decide', async (route) => {
    await route.fulfill({
      json: {
        approval: {
          ...pendingApproval(serverId),
          status: 'denied',
          decided_at: new Date().toISOString(),
          decided_by: 'admin',
        },
        execution: null,
      },
    });
  });

  try {
    await page.goto('/containers');
    const row = page.getByRole('row').filter({ hasText: 'walk5-web-1' });
    await expect(row).toBeVisible({ timeout: 20_000 });

    await row.getByRole('button', { name: /重\s*启/ }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText('将申请执行的命令')).toBeVisible();
    await dialog.getByRole('button', { name: '创建审批' }).click();
    await expect(dialog.getByText('待执行命令')).toBeVisible();

    await dialog.getByRole('button', { name: /拒\s*绝/ }).click();
    await expect(dialog.getByText('已拒绝，命令未执行')).toBeVisible();
    // 命令从未执行：标签不得宣称「已执行」（也不得停在「待执行」——决策
    // 已完成，不再是待执行状态）。
    await expect(dialog.getByText('未执行命令')).toBeVisible();
    await expect(dialog.getByText('已执行命令')).toHaveCount(0);
    await expect(dialog.getByText('待执行命令')).toHaveCount(0);
  } finally {
    await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
  }
});
