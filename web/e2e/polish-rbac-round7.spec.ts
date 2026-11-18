import {
  request as playwrightRequest,
  type APIRequestContext,
  type Locator,
  type Page,
} from '@playwright/test';

import { expect, test } from './fixtures';
import { AUTH_STATE_FILE, OPERATOR_STATE_FILE, REVIEWER_STATE_FILE } from './helpers';

// RBAC 第 7 轮走查（medium）：运维工程师（无 approval:decide）与自审发起人
// 在「批量详情」「诊断修复」「容器操作」三处仍会看到批准/拒绝按钮，点击
// 只会得到 403。修复后三处按 approvals:decide + requested_by != 当前用户
// 收敛：无权限/自审时隐藏决定按钮并显示「等待审批者处理」。
//
// 覆盖：
// 1) operator：批量详情（真实 POST /api/batch/jobs + 详情抽屉）；
// 2) operator：诊断修复（真实 /alerts 手动诊断 → fix → 审批卡）；
// 3) operator：容器操作（容器列表/action route mock，审批对象由 mock 下发）；
// 4-6) admin 自审：同样三处，管理员就是请求发起人。

const E2E_ORIGIN = 'http://localhost:4173';
const API_HEADERS = { Origin: E2E_ORIGIN };

interface ServerItem {
  id: string;
  name: string;
}

async function serverIDByName(page: Page, name: string): Promise<string> {
  const response = await page.request.get('/api/servers');
  expect(response.ok(), await response.text()).toBeTruthy();
  const body = (await response.json()) as { items?: ServerItem[] };
  const server = (body.items ?? []).find((item) => item.name === name);
  expect(server, `server ${name} missing`).toBeTruthy();
  return server?.id ?? '';
}

/** 以当前页面会话创建一台待审批的批量任务（auto_exec=false → safe 也审批）。 */
async function createAwaitingBatch(page: Page, serverID: string, command = 'uptime') {
  const response = await page.request.post('/api/batch/jobs', {
    headers: API_HEADERS,
    data: { command, server_ids: [serverID] },
  });
  expect(response.status(), await response.text()).toBe(201);
  const job = (await response.json()) as { id: string };
  return job.id;
}

/** 打开批量详情并等待待审批行出现。 */
async function openBatchDrawer(page: Page, jobID: string) {
  await page.goto(`/batch?job=${jobID}`);
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  return drawer;
}

/** 确认批量行内没有决定按钮，且「查看」弹窗也不渲染“批准原命令”。 */
async function expectBatchReadOnly(page: Page, drawer: Locator) {
  await expect(drawer.getByText('等待审批者处理')).toBeVisible();
  await expect(drawer.getByRole('button', { name: /批\s*准/ })).toHaveCount(0);
  await expect(drawer.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);

  await drawer
    .getByRole('button', { name: /查\s*看/ })
    .first()
    .click();
  const modal = page.locator('.ant-modal').filter({ hasText: '查看审批命令' });
  await expect(modal).toBeVisible();
  await expect(modal.getByRole('button', { name: '批准原命令' })).toHaveCount(0);
  await expect(modal.getByText('等待审批者处理')).toBeVisible();
}

/**
 * 通过 /alerts 的「AI 诊断」Tab 发起一次真实诊断（假 LLM + e2e-fake-host），
 * 在抽屉里点「执行（需审批）」创建修复审批，返回该审批 ID（用于清理）。
 */
async function createDiagnosisFixApproval(page: Page): Promise<string> {
  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();
  await page.getByRole('tab', { name: 'AI 诊断' }).click();
  await page.getByRole('combobox', { name: '诊断目标服务器' }).click();
  await page
    .locator(
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="e2e-fake-host"]',
    )
    .click();
  await page.getByRole('button', { name: /发起 AI 诊断/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '诊断已发起' })).toBeVisible();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 90_000 });

  const fixResponse = page.waitForResponse(
    (response) =>
      /\/api\/diagnosis\/sessions\/[^/]+\/fix$/.test(response.url()) &&
      response.request().method() === 'POST',
  );
  await drawer.getByRole('button', { name: '执行（需审批）' }).first().click();
  const fixed = await fixResponse;
  expect(fixed.status(), await fixed.text()).toBe(200);
  const body = (await fixed.json()) as { approval?: { id?: string } } | null;
  const approvalID = body?.approval?.id ?? '';
  expect(approvalID).not.toBe('');
  await expect(drawer.getByText('待审批的修复命令')).toBeVisible({ timeout: 15_000 });
  return approvalID;
}

/** 审批者拒绝指定审批（清理 pending，避免悬挂到 TTL）。 */
async function denyAsReviewer(approvalID: string) {
  const reviewer = await playwrightRequest.newContext({
    baseURL: E2E_ORIGIN,
    storageState: REVIEWER_STATE_FILE,
  });
  const denied = await reviewer.post(`/api/approvals/${approvalID}/decide`, {
    headers: API_HEADERS,
    data: { decision: 'deny' },
  });
  expect(denied.status(), await denied.text()).toBe(200);
  await reviewer.dispose();
}

/** 容器操作弹窗的审批对象（mock）：requested_by 由调用方指定。 */
function pendingContainerApproval(serverID: string, requestedBy: string) {
  return {
    id: '3f2b1c9a-7c64-4e0f-9a1d-58e0f4a2b6c8',
    conversation_id: null,
    tool_call_id: null,
    source: 'console',
    server_id: serverID,
    server_name: 'rbac7-container',
    command: 'docker stop c1d2e3f4a5b6',
    risk_level: 'needs_approval',
    rule_id: 'CONTAINER_CHANGE',
    reason: '容器变更需人工批准',
    status: 'pending',
    approval_mode: 'standard',
    requested_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 120_000).toISOString(),
    decided_at: null,
    requested_by: requestedBy,
    requested_by_name: 'requester',
    decided_by: null,
    note: '',
    edited_command: '',
    edited_risk_level: '',
    edited_rule_id: '',
    break_glass: false,
    legacy: false,
  };
}

/**
 * 打开容器页并 mock 列表 + action 响应，进入 pending 态；返回弹窗 body。
 * 服务器由 admin 预创建（operator 无 asset:write）。
 */
async function openContainerPending(
  page: Page,
  serverID: string,
  serverName: string,
  requestedBy: string,
) {
  await page.route(`**/api/servers/${serverID}/containers`, async (route) => {
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: 'c1d2e3f4a5b6',
            name: 'rbac7-container',
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
  await page.route(`**/api/servers/${serverID}/containers/*/action`, async (route) => {
    await route.fulfill({ json: { approval: pendingContainerApproval(serverID, requestedBy) } });
  });

  await page.goto('/containers');
  await expect(page.getByRole('heading', { name: '容器' })).toBeVisible();
  const option = page.locator('.ant-select-item-option', { hasText: serverName }).first();
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();

  await expect(page.getByText('rbac7-container').first()).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: '停止' }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: '创建审批' }).click();
  await expect(dialog.getByText('剩余')).toBeVisible({ timeout: 15_000 });
  return dialog.locator('.ant-modal-body');
}

async function createContainerServer(
  request: APIRequestContext,
  label: string,
): Promise<{ id: string; name: string }> {
  const name = `rbac7-host-${label}-${Date.now()}`;
  const response = await request.post('/api/servers', {
    headers: API_HEADERS,
    data: {
      name,
      host: '127.0.0.1',
      port: 1,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-pass',
      auto_exec_enabled: false,
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  const server = (await response.json()) as { id: string };
  return { id: server.id, name };
}

// ---------------------------------------------------------------------------
// 1. operator：三处入口都不渲染决定按钮
// ---------------------------------------------------------------------------
test.describe('RBAC 第 7 轮：operator 无审批权限', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('批量详情：待审批行隐藏批准/拒绝，查看弹窗只读', async ({ page }) => {
    const serverID = await serverIDByName(page, 'e2e-fake-host');
    const jobID = await createAwaitingBatch(page, serverID);
    try {
      const drawer = await openBatchDrawer(page, jobID);
      await expectBatchReadOnly(page, drawer);
    } finally {
      await page.request.post(`/api/batch/jobs/${jobID}/cancel`, { headers: API_HEADERS });
    }
  });

  test('诊断修复：待审批修复卡隐藏批准执行/拒绝', async ({ page }) => {
    const approvalID = await createDiagnosisFixApproval(page);
    try {
      const drawer = page.getByRole('dialog');
      await expect(drawer.getByText('等待审批者处理')).toBeVisible();
      await expect(drawer.getByRole('button', { name: /批准执行/ })).toHaveCount(0);
      await expect(drawer.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);

      // 查看命令仍可打开，但编辑区不渲染“批准原命令”。
      await drawer.getByRole('button', { name: /查看命令/ }).click();
      await expect(drawer.getByRole('button', { name: '批准原命令' })).toHaveCount(0);
      await expect(drawer.getByText('等待审批者处理').first()).toBeVisible();
    } finally {
      await denyAsReviewer(approvalID);
    }
  });

  test('容器操作：pending 弹窗隐藏批准/拒绝，只读查看', async ({ page }) => {
    const admin = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    const { id: serverID, name: serverName } = await createContainerServer(admin, 'operator');
    await admin.dispose();

    const body = await openContainerPending(
      page,
      serverID,
      serverName,
      '00000000-0000-0000-0000-000000000001',
    );
    await expect(body.getByText('等待审批者处理')).toBeVisible();
    await expect(body.getByRole('button', { name: /批\s*准/ })).toHaveCount(0);
    await expect(body.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);

    await body.getByRole('button', { name: /查看命令/ }).click();
    await expect(body.getByRole('button', { name: '批准原命令' })).toHaveCount(0);
    await expect(body.getByText('等待审批者处理')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 2. admin 自审：管理员就是审批发起人
// ---------------------------------------------------------------------------
test.describe('RBAC 第 7 轮：管理员自审', () => {
  test('批量详情：自己发起的待审批行隐藏批准/拒绝', async ({ browser }) => {
    // 独立 context：不使用 fixtures 的审批者身份注入，保持真实 admin。
    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    const serverID = await serverIDByName(page, 'e2e-fake-host');
    const jobID = await createAwaitingBatch(page, serverID);
    try {
      const drawer = await openBatchDrawer(page, jobID);
      await expect(drawer.getByText('等待审批者处理')).toBeVisible();
      await expect(drawer.getByRole('button', { name: /批\s*准/ })).toHaveCount(0);
      await expect(drawer.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);
    } finally {
      await page.request.post(`/api/batch/jobs/${jobID}/cancel`, { headers: API_HEADERS });
      await context.close();
    }
  });

  test('诊断修复：自己发起的修复审批隐藏批准执行/拒绝', async ({ browser }) => {
    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    const approvalID = await createDiagnosisFixApproval(page);
    try {
      const drawer = page.getByRole('dialog');
      await expect(drawer.getByText('等待审批者处理')).toBeVisible();
      await expect(drawer.getByRole('button', { name: /批准执行/ })).toHaveCount(0);
      await expect(drawer.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);
    } finally {
      await denyAsReviewer(approvalID);
      await context.close();
    }
  });

  test('容器操作：自己发起的待审批弹窗隐藏批准/拒绝', async ({ browser }) => {
    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    const created = await createContainerServer(page.request, 'self');
    const meResponse = await page.request.get('/api/auth/me');
    expect(meResponse.ok()).toBeTruthy();
    const me = (await meResponse.json()) as { user: { id: string } };

    const body = await openContainerPending(page, created.id, created.name, me.user.id);
    await expect(body.getByText('等待审批者处理')).toBeVisible();
    await expect(body.getByRole('button', { name: /批\s*准/ })).toHaveCount(0);
    await expect(body.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);
    await context.close();
  });
});
