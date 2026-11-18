import { AUTH_STATE_FILE, sendMessage } from './helpers';
import { expect, test } from './fixtures';

// RBAC 第 3 轮走查的两项前端回归：
//
//  1. [medium] done 帧到达即视为回答结束：正文已完整上屏后点「新建会话」
//     不得被弹「当前回答仍在进行」。旧实现要等 for-await 读完流并在
//     finally 置 idle，空载 30–45ms、负载下秒级的窗口内点击必被拦截。
//  2. [medium] 单管理员降级时审批卡在点「批准」之前就置灰并给出中文说明，
//     而不是让审批人每次撞一个英文 409 NO_SECOND_APPROVER。
//
// 降级态用 GET /api/settings 的只读字段（single_admin_degraded）渲染；
// 这里用路由替身模拟降级部署，不改真实全局门限、不影响并行 spec。

test.use({ storageState: AUTH_STATE_FILE });

test('正文完整上屏后立即点「新建会话」不被离开守卫拦截（done 帧即结束）', async ({ page }) => {
  await page.goto('/chat');
  await sendMessage(page, '流式结束后立刻切换会话');
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  // 在「最后一段正文落地」的那一刻直接点击：用 MutationObserver 在 DOM
  // 变更的微任务里 resolve，随后立刻点击——这正是旧实现会弹窗的时序窗口。
  const assistant = page.locator('.chat-message--assistant .opsmind-markdown').first();
  await expect(assistant).toBeVisible({ timeout: 30_000 });
  await page.evaluate(async () => {
    const target = document.querySelector('.chat-message--assistant .opsmind-markdown');
    if (!target) {
      throw new Error('assistant bubble missing');
    }
    // 等到 assistant 文本停止增长（连续两次 MutationObserver 静默 80ms）。
    await new Promise<void>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | null = null;
      const observer = new MutationObserver(() => {
        if (timer) {
          clearTimeout(timer);
        }
        timer = setTimeout(() => {
          observer.disconnect();
          resolve();
        }, 80);
      });
      observer.observe(target, { childList: true, subtree: true, characterData: true });
      // 已经停止增长（无新变更）时兜底 resolve。
      timer = setTimeout(() => {
        observer.disconnect();
        resolve();
      }, 3000);
    });
  });

  // 同一帧内点击「新建会话」：不得出现「当前回答仍在进行」确认弹窗。
  await page.getByTitle('新建会话').click();
  await expect(page).toHaveURL(/\/chat$/, { timeout: 10_000 });
  await expect(page.getByText('开始新的运维对话')).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('单管理员降级：审批页顶部中文说明 + 批准按钮置灰（不再撞英文 409）', async ({ page }) => {
  // 用 API 造一条真实的 pending 审批（终端授权，无 SSE 流——直接 goto 到
  // 审批页不会像离开聊天流那样把审批连带取消为 expired）。
  const serversResponse = await page.request.get('/api/servers');
  expect(serversResponse.ok()).toBeTruthy();
  const servers = (await serversResponse.json()) as { items?: Array<{ id: string }> };
  const serverId = servers.items?.[0]?.id;
  expect(serverId, 'e2e 需要至少一台播种服务器').toBeTruthy();
  const grantResponse = await page.request.post('/api/term/grants', {
    headers: { Origin: 'http://localhost:4173' },
    data: { server_id: serverId },
  });
  expect(grantResponse.status(), await grantResponse.text()).toBe(202);

  // 模拟降级部署：可承担第二人职责的启用账号只剩 1 个。
  await page.route('**/api/settings', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.continue();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        approval_mode: 'standard',
        single_admin_degraded: true,
        enabled_approvers: 1,
      }),
    });
  });

  await page.goto('/approvals');
  const banner = page.getByTestId('degraded-mode-banner');
  await expect(banner).toBeVisible({ timeout: 20_000 });
  await expect(banner).toContainText('可审批账号不足降级模式');
  await expect(banner).toContainText('创建至少一个启用的审批者');

  const card = page.locator('.chat-approval', { hasText: '终端授权' }).first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  // 中文降级说明在卡片内可见，批准按钮禁用、拒绝仍可用。
  await expect(card.getByText('当前无法批准：系统缺少第二个可审批的启用账号')).toBeVisible();
  await expect(card.getByRole('button', { name: '批准' })).toBeDisabled();
  await expect(card.getByRole('button', { name: '拒绝' })).toBeEnabled();
});
