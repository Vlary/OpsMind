import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE, DEFAULT_FINAL, SCENARIO_RISKY, sendMessage } from './helpers';

// 第 4 轮（新序列）打磨项的端到端验证：
//  - 对话页输入框自动聚焦：登录后进 /chat、建会话跳转 /c/:id、点导航
//    切回对话后 activeElement 都是输入框（此前一直是 BODY/菜单项）；
//  - ConfigProvider 全局 zh_CN：资产页分页器 pageSize 切换器显示
//    「N 条/页」，不再出现英文「/ page」；
//  - 停止按钮：存在 pending 审批时仍弹二次确认（作废审批不可恢复），
//    可「继续」无损取消；纯流式单击直停的行为在 chat-coverage.spec.ts
//    的 slow 用例中验证；
//  - 服务器页搜索框与会话侧栏搜索框的本地过滤行为。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

test.describe('第 4 轮（新序列）打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('输入框自动聚焦：进入 /chat、建会话跳转、导航切回对话后焦点都在输入框', async ({ page }) => {
    await page.goto('/chat');
    const box = page.getByLabel('消息输入框');
    await expect(box).toBeVisible();
    // providers 加载期间输入框 disabled，就绪后 effect 聚焦（poll 兜住加载窗口）。
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? ''))
      .toBe('消息输入框');

    // 发送首条消息创建会话 → 跳转 /c/:id（ChatPage remount），仍自动聚焦。
    await sendMessage(page, '聚焦验证巡检');
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? ''))
      .toBe('消息输入框');
    // 等流式结束再切走（流式中切换会弹离开确认，与聚焦断言无关）。
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible({ timeout: 30_000 });

    // 走查原场景：从服务器页点导航切回对话（此前焦点停在菜单项 LI 上）。
    await page.goto('/servers');
    await expect(page.locator('.ant-table')).toBeVisible();
    await page.getByRole('menuitem', { name: '对话' }).click();
    await expect(page).toHaveURL(/\/chat$/);
    await expect
      .poll(() => page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? ''))
      .toBe('消息输入框');
  });

  test('资产页分页器中文化：pageSize 切换器显示「条/页」，服务器搜索框可过滤', async ({ page }) => {
    // 造 11 台唯一名服务器（连同 global-setup 的 e2e-fake-host 共 12 台 → 2 页，
    // 分页器才渲染）。finally 里全部删除，不影响并行用例。
    const createdIds: string[] = [];
    for (let index = 1; index <= 11; index += 1) {
      const response = await page.request.post('/api/servers', {
        headers: ORIGIN_HEADERS,
        data: {
          name: `e2e-locale-srv-${String(index).padStart(2, '0')}`,
          host: '127.0.0.9',
          port: 22,
          username: 'root',
          auth_method: 'password',
          password: 'not-a-real-password',
        },
      });
      expect(response.status()).toBe(201);
      createdIds.push(((await response.json()) as { id: string }).id);
    }

    try {
      await page.goto('/servers');
      const pagination = page.locator('.ant-pagination');
      await expect(pagination).toBeVisible();
      // 修复前：pageSize 切换器显示英文「10 / page」。
      await expect(pagination.locator('.ant-pagination-options')).toContainText('条/页');
      await expect(pagination.locator('.ant-pagination-options')).not.toContainText('/ page');

      // 服务器页搜索框（本地过滤）：唯一名只命中 1 行。
      const search = page.getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签');
      await search.fill('e2e-locale-srv-07');
      await expect(page.locator('.ant-table-row')).toHaveCount(1);
      await expect(page.locator('.ant-table-row').first()).toContainText('e2e-locale-srv-07');

      // 清空恢复：表格回到当前页满页（pageSize=10，12 台分 2 页，
      // .ant-table-row 只渲染当前页），不再只剩 1 行，分页器仍在。
      await search.fill('');
      await expect(page.locator('.ant-table-row')).toHaveCount(10);
      await expect(pagination).toBeVisible();
    } finally {
      for (const id of createdIds) {
        await page.request.delete(`/api/servers/${id}`, { headers: ORIGIN_HEADERS });
      }
    }
  });

  test('审批等待中停止仍需二次确认：可无损继续，确认后流停止', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    // pending 审批存在：点停止必须弹确认（作废审批不可恢复，保留防护）。
    await page.locator('.chat-composer').getByRole('button', { name: '停止' }).click();
    const confirm = page.getByRole('dialog', { name: '停止当前回答？' });
    await expect(confirm).toBeVisible();
    // 「继续」无损取消：弹窗关闭，审批卡片仍在等待决策。
    await confirm.getByRole('button', { name: /继\s*续/ }).click();
    await expect(confirm).toBeHidden();
    await expect(approvalCard.getByRole('button', { name: '批准' })).toBeEnabled();

    // 再次停止并确认：弹窗关闭、流结束（停止按钮回到发送）。
    await page.locator('.chat-composer').getByRole('button', { name: '停止' }).click();
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: /停\s*止/ }).click();
    await expect(confirm).toBeHidden();
    await expect(page.getByRole('button', { name: '发送' })).toBeVisible({ timeout: 20_000 });
  });

  test('会话侧栏搜索：按标题过滤当前列表，无匹配给出空态', async ({ page }) => {
    // 建一个唯一标题会话（不发消息，无流式副作用），结束后删除。
    const created = await page.request.post('/api/conversations', {
      headers: ORIGIN_HEADERS,
      data: { title: 'e2e侧栏搜索唯一标题会话' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };

    try {
      await page.goto('/chat');
      const search = page.getByLabel('搜索会话');
      await expect(search).toBeVisible({ timeout: 20_000 });

      // 唯一关键词只命中目标会话（并行用例的会话标题不含该关键词）。
      await search.fill('e2e侧栏搜索唯一标题');
      await expect(page.locator('.chat-conv-item')).toHaveCount(1);
      await expect(page.locator('.chat-conv-item').first()).toContainText(
        'e2e侧栏搜索唯一标题会话',
      );

      // 无匹配：空态文案，列表清空。
      await search.fill('不存在的会话关键词xyz');
      await expect(page.getByText('没有匹配的会话')).toBeVisible();
      await expect(page.locator('.chat-conv-item')).toHaveCount(0);

      // 清空恢复列表。
      await search.fill('');
      await expect(page.locator('.chat-conv-item').first()).toBeVisible();
    } finally {
      await page.request.delete(`/api/conversations/${id}`, { headers: ORIGIN_HEADERS });
    }
  });
});
