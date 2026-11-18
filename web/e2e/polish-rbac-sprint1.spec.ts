import { request as playwrightRequest } from '@playwright/test';

import { expect, test } from './fixtures';
import {
  AUDITOR_STATE_FILE,
  AUTH_STATE_FILE,
  OPERATOR_STATE_FILE,
  REVIEWER_STATE_FILE,
  RISKY_COMMAND,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// RBAC 走查 sprint 1 端到端验证：
// 1) 运维执行者（operator）：数据库/告警页只保留有权限的入口；对话审批卡不再
//    渲染批准/拒绝（无 approvals:decide），改为「等待审批者处理」。
// 2) 审批者（approver）：审批队列能看到并可批准他人发起的请求。
// 3) 管理员自审：有审批权限但是发起人，卡片提示「不能审批自己发起的请求」。
// 4) 审计员（auditor）：/audit 与 /replays 页头明确只读。
// 5) 会话失效：业务查询 401 经 queryClient 兜底跳登录页，带 reason=session_expired。

const E2E_ORIGIN = 'http://localhost:4173';

test.describe('RBAC 走查 sprint 1：运维执行者入口收敛', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('数据库页只保留查询/测试，告警页不渲染新建规则/渠道', async ({ page }) => {
    const admin = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    const name = `rbac-op-db-${Date.now()}`;
    const created = await admin.post('/api/databases', {
      headers: { Origin: E2E_ORIGIN },
      data: {
        name,
        engine: 'sqlite',
        sqlite_path: `/tmp/${name}.db`,
      },
    });
    expect(created.status(), await created.text()).toBe(201);
    await admin.dispose();

    await page.goto('/databases');
    await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill(name);
    const row = page.locator('tr', { hasText: name }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });

    // operator 拥有 assets:read + connection:test + execute:read：
    // 查询/测试保留，新增/编辑/删除按 assets:write 收敛。
    await expect(row.getByRole('button', { name: '查询' })).toBeVisible();
    await expect(row.getByRole('button', { name: '测试' })).toBeVisible();
    await expect(row.getByRole('button', { name: '编辑' })).toHaveCount(0);
    await expect(row.getByRole('button', { name: '删除' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: '新增数据库' })).toHaveCount(0);

    await page.goto('/alerts');
    await expect(page.getByRole('button', { name: '新建规则' })).toHaveCount(0);
    await expect(page.getByText('当前角色只读，不能新建或修改规则')).toBeVisible();
    await page.getByRole('tab', { name: '通知渠道' }).click();
    await expect(page.getByRole('button', { name: '新建渠道' })).toHaveCount(0);
    await expect(page.getByText('当前角色只读，不能新建或修改通知渠道')).toBeVisible();
  });

  test('对话审批卡不渲染批准/拒绝，显示等待审批者处理', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const card = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(card.getByText('等待审批者处理')).toBeVisible();
    await expect(card.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(card.getByRole('button', { name: '拒绝' })).toHaveCount(0);

    // 清理：由审批者拒绝这条审批，避免 pending 悬挂。
    const cid = conversationIdOf(page);
    const reviewer = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: REVIEWER_STATE_FILE,
    });
    const list = await reviewer.get(`/api/approvals?conversation_id=${cid}&limit=10`);
    const body = (await list.json()) as {
      items: Array<{ id: string; command: string; status: string }>;
    };
    const pending = body.items.find(
      (item) => item.command === RISKY_COMMAND && item.status === 'pending',
    );
    expect(pending?.id).toBeTruthy();
    const denied = await reviewer.post(`/api/approvals/${pending?.id}/decide`, {
      headers: { Origin: E2E_ORIGIN },
      data: { decision: 'deny' },
    });
    expect(denied.status(), await denied.text()).toBe(200);
    await reviewer.dispose();
  });
});

test.describe('RBAC 走查 sprint 1：审批者视图', () => {
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('审批队列显示批准/拒绝，并成功批准运维发起的请求', async ({ page, browser }) => {
    // 运维工程师（真实 operator 会话）发起一条需审批命令；使用独立
    // context 以避免 fixtures 的 admin 视角注入。
    const operatorContext = await browser.newContext({ storageState: OPERATOR_STATE_FILE });
    const operatorPage = await operatorContext.newPage();
    await operatorPage.goto('/chat');
    await sendMessage(operatorPage, SCENARIO_RISKY);
    await expect(operatorPage).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const operatorCard = operatorPage.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(operatorCard).toBeVisible({ timeout: 30_000 });
    await expect(operatorCard.getByText('等待审批者处理')).toBeVisible();

    await page.goto('/approvals');
    // 审批队列可能有并行用例创建的 admin 审批；按「发起人：operator」+
    // 风险命令锁定本用例卡片。
    const wrapper = page
      .locator('div')
      .filter({ hasText: '发起人：operator' })
      .filter({ has: page.locator('.chat-approval', { hasText: RISKY_COMMAND }) })
      .last();
    const card = wrapper.locator('.chat-approval').first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card.getByRole('button', { name: '批准' })).toBeVisible();
    await card.getByRole('button', { name: '批准' }).click();

    // 审批队列只列 pending：决策成功后卡片随 refetch 从队列移除。
    await expect(card).toHaveCount(0, { timeout: 20_000 });

    // 发起人（operator）页面通过 SSE 收到决策：卡片翻转为已批准。
    await expect(operatorCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await operatorContext.close();
  });
});

test.describe('RBAC 走查 sprint 1：管理员自审', () => {
  test('管理员是发起人时不渲染批准/拒绝，提示不能审批自己发起的请求', async ({ browser }) => {
    // 独立 context：不使用 fixtures 的审批者视角注入，保持真实 admin 身份。
    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const card = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(card.getByText('不能审批自己发起的请求')).toBeVisible();
    await expect(card.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(card.getByRole('button', { name: '拒绝' })).toHaveCount(0);

    // 清理：仍由审批者拒绝（admin 自审会被服务端拒绝）。
    const cid = conversationIdOf(page);
    const reviewer = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: REVIEWER_STATE_FILE,
    });
    const list = await reviewer.get(`/api/approvals?conversation_id=${cid}&limit=10`);
    const body = (await list.json()) as {
      items: Array<{ id: string; command: string; status: string }>;
    };
    const pending = body.items.find(
      (item) => item.command === RISKY_COMMAND && item.status === 'pending',
    );
    expect(pending?.id).toBeTruthy();
    const denied = await reviewer.post(`/api/approvals/${pending?.id}/decide`, {
      headers: { Origin: E2E_ORIGIN },
      data: { decision: 'deny' },
    });
    expect(denied.status(), await denied.text()).toBe(200);
    await reviewer.dispose();
    await context.close();
  });
});

test.describe('RBAC 走查 sprint 1：审计员只读提示', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('/audit 与 /replays 页头明确说明只读', async ({ page }) => {
    await page.goto('/audit');
    await expect(page.getByText('审计员为只读角色')).toBeVisible({ timeout: 20_000 });
    await expect(
      page.getByText('可查看审计、回放与录制；任何写操作都会被服务端拒绝。'),
    ).toBeVisible();

    await page.goto('/replays');
    await expect(page.getByText('审计员为只读角色')).toBeVisible({ timeout: 20_000 });
  });
});

test.describe('RBAC 走查 sprint 1：会话失效跳转', () => {
  test('业务查询 401 经全局兜底跳登录页并提示登录已失效', async ({ browser }) => {
    const admin = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    const meResponse = await admin.get('/api/auth/me');
    const meBody = await meResponse.text();
    await admin.dispose();

    const context = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const page = await context.newPage();
    let meCalls = 0;
    await page.route('**/api/auth/me', async (route) => {
      meCalls += 1;
      if (meCalls === 1) {
        // 首屏 me 成功（页面渲染、权限门通过）；随后 session 被吊销。
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: meBody,
        });
        return;
      }
      await route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({
          error: { code: 'AUTH_REQUIRED', message: 'authentication required' },
        }),
      });
    });
    // 审计查询返回 401：走 queryClient 的全局兜底（而非 RequireAuth 的 me 401）。
    await page.route('**/api/audit**', async (route) => {
      await route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({
          error: { code: 'AUTH_REQUIRED', message: 'authentication required' },
        }),
      });
    });

    await page.goto('/audit');
    // 修复前：/login?next=%2Faudit（无 reason），登录页无任何提示。
    await expect(page).toHaveURL(/\/login\?.*reason=session_expired/, { timeout: 20_000 });
    await expect(
      page.getByText('登录已失效（会话过期或角色/权限已变更），请重新登录。'),
    ).toBeVisible();
    await context.close();
  });
});
