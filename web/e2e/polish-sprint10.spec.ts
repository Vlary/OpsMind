import { expect, test, type Page } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  RISKY_FINAL,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 冲刺 10 打磨项的端到端验证：
//  1) 改密审计动作（auth.password_changed / auth.password_change_failed）
//     此前不在 AUDIT_ACTIONS 清单：审计页动作列显示英文原串、动作筛选
//     下拉搜不到。修复后下拉可选、行内显示中文标签。
//  2) 服务器列表搜索此前不含端口（地址列渲染 host:port 但按端口搜索
//     恒 0 行）。修复后裸端口与 host:port 组合都参与匹配。
//  3) 非流发起窗口（第二个浏览器窗口打开同一会话）批准审批后，后续
//     工具结果与最终回答此前需手动刷新才可见。修复后 decide 成功即按
//     落库窗口做延迟补偿 invalidate（+2s/+5s/+10s），B 窗自动出现结果。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

interface AuditItem {
  action: string;
  actor_type: string;
}

/** 查询审计列表（带 action 过滤）。 */
async function fetchAudit(page: Page, query: string): Promise<AuditItem[]> {
  const response = await page.request.get(`/api/audit${query}`);
  expect(response.ok()).toBeTruthy();
  const payload = (await response.json()) as { items: AuditItem[] };
  return payload.items;
}

test.describe('冲刺 10 打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('改密失败动作：审计动作下拉可选中且行内显示中文标签', async ({ page }) => {
    test.setTimeout(120_000);

    // 用错误旧密码触发一次改密失败：401 + 审计 auth.password_change_failed。
    // 失败路径不改密码、不吊销当前会话，对并行用例无影响（成功改密才会
    // 吊销全部会话，那属于 auth-password 串行项目，此处绝不能做）。
    const change = await page.request.post('/api/auth/password', {
      headers: ORIGIN_HEADERS,
      data: { old_password: 'e2e-wrong-old-password', new_password: 'e2e-new-password-x' },
    });
    expect(change.status()).toBe(401);

    // 审计是异步批量写入（500ms 批次）：轮询等失败记录落库。
    const deadline = Date.now() + 15_000;
    let items: AuditItem[] = [];
    while (Date.now() < deadline) {
      items = await fetchAudit(page, '?action=auth.password_change_failed&limit=10');
      if (items.length > 0) {
        break;
      }
      await page.waitForTimeout(300);
    }
    expect(items.length).toBeGreaterThan(0);

    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();

    // 动作筛选下拉按 label 过滤：修复前清单无改密动作，搜「修改密码」
    // 恒 0 选项；修复后可搜到改密动作。输入全名「修改密码失败」精确选中
    //（只输「修改密码」会同时命中成功/失败两项，Enter 默认选第一项）。
    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    await actionSelect.click();
    await page.keyboard.type('修改密码失败');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(actionSelect).toContainText('修改密码失败');

    // 列表收窄到该动作后，动作列主行显示中文标签（修复前主行直接显示
    // 英文原串）。副行按设计保留原始动作码便于排查，不作断言。
    const row = page.locator('.ant-table-row').filter({ hasText: '修改密码失败' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    const actionCell = row.locator('td').filter({ hasText: 'auth.password_change_failed' }).first();
    await expect(actionCell.locator('div > div').first()).toHaveText('修改密码失败');
  });

  test('服务器搜索含端口：裸端口与 host:port 组合均命中', async ({ page }) => {
    test.setTimeout(120_000);

    // 造一台 host 与端口都唯一的服务器（10.255.255.99:22299）。
    const name = 'e2e-sprint10-port-search';
    const created = await page.request.post('/api/servers', {
      headers: ORIGIN_HEADERS,
      data: {
        name,
        host: '10.255.255.99',
        port: 22299,
        username: 'root',
        auth_method: 'password',
        password: 'not-a-real-password',
        tags: ['e2e'],
        auto_exec_enabled: false,
      },
    });
    expect(created.status()).toBe(201);

    try {
      await page.goto('/servers');
      const search = page.getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签');
      await expect(search).toBeVisible();
      const counter = page.locator('.opsmind-admin-toolbar').getByText(/共 \d+ 台/);

      // 裸端口命中（修复前：端口不参与匹配，恒 0 行）。
      await search.fill('22299');
      await expect(counter).toHaveText('共 1 台');
      await expect(page.locator('tr', { hasText: name }).first()).toBeVisible();

      // 与地址列渲染一致的 host:port 组合命中。
      await search.fill('10.255.255.99:22299');
      await expect(counter).toHaveText('共 1 台');
      await expect(page.locator('tr', { hasText: name }).first()).toBeVisible();

      // 非本机端口仍正常过滤。
      await search.fill('22298');
      await expect(counter).toHaveText('共 0 台');
      await expect(page.locator('.ant-table-row')).toHaveCount(0);

      // 名称命中不受影响（既有行为回归）。
      await search.fill(name);
      await expect(counter).toHaveText('共 1 台');
    } finally {
      const list = await page.request.get('/api/servers');
      const payload = (await list.json()) as { items: Array<{ id: string; name: string }> };
      const target = payload.items.find((item) => item.name === name);
      if (target) {
        const removed = await page.request.delete(`/api/servers/${target.id}`, {
          headers: ORIGIN_HEADERS,
        });
        expect(removed.status()).toBeLessThan(300);
      }
    }
  });

  test('双窗口：B 窗批准审批后无需刷新，工具结果与最终回答自动出现', async ({ page, context }) => {
    test.setTimeout(150_000);

    // A 窗（流发起窗口）发送 risky 场景，等待审批卡。
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    const approvalCardA = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCardA.getByText('待处理')).toBeVisible({ timeout: 30_000 });

    // B 窗：同一会话的第二个浏览器窗口（无 SSE 流连接，审批卡来自
    // approvals 列表查询渲染）。
    const pageB = await context.newPage();
    try {
      await pageB.goto(`/c/${cid}`);
      const approvalCardB = pageB.locator('.chat-approval', { hasText: '命令审批' }).first();
      await expect(approvalCardB.getByText('待处理')).toBeVisible({ timeout: 30_000 });

      // B 窗批准：decide 成功后走延迟补偿 invalidate（+2s/+5s/+10s）。
      await approvalCardB.getByRole('button', { name: '批准' }).click();
      await expect(approvalCardB.getByText('审批已批准', { exact: true })).toBeVisible({
        timeout: 20_000,
      });

      // B 窗不刷新页面：工具卡与最终回答经补偿 refetch 自动出现
      //（修复前：卡片终态同步，但消息区需手动刷新才能看到结果）。
      const sshCardB = pageB.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
      await expect(sshCardB).toBeVisible({ timeout: 40_000 });
      await expect(sshCardB.getByText('SSH_EXEC_FAILED').first()).toBeVisible({
        timeout: 40_000,
      });
      await expect(pageB.getByText(RISKY_FINAL)).toBeVisible({ timeout: 40_000 });

      // A 窗（流发起窗口）经 SSE done 帧照常收到最终回答，路径互不影响。
      await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });
    } finally {
      await pageB.close();
    }
  });
});
