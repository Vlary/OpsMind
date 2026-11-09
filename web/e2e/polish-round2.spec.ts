import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE, SCENARIO_RISKY, conversationIdOf, sendMessage } from './helpers';

// 第 2 轮打磨项的端到端验证：消息接口在首次加载窗口失败不崩页、
// 流式（含审批等待）期间刷新触发原生离开确认、流断开后「已停止/已取消」
// 文案、流式中删除当前会话被 409 拒绝后确认弹窗自动关闭。

test.describe('第 2 轮打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('消息接口在首次加载窗口失败：错误横幅而非整页崩溃', async ({ page }) => {
    // 准备一个有消息的会话。
    await page.goto('/chat');
    await sendMessage(page, '打磨二消息加载失败验证');
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.locator('.chat-message--user').first()).toBeVisible({ timeout: 30_000 });

    // 场景 A（网络闪断）：消息接口直接断连 → 错误横幅 + 可重试。
    await page.route('**/api/conversations/*/messages*', (route) => route.abort());
    await page.reload();

    // 修复前：getNextPageParam 对 undefined 取 has_more 抛错，整树崩进
    // RouteErrorBoundary（「页面出现意外错误」，侧栏一并消失）；
    // 修复后：消息区显示可重试的错误横幅，页面其余部分可用。
    await expect(page.getByText('页面出现意外错误')).toHaveCount(0);
    const errorArea = page.locator('.chat-error-area');
    await expect(errorArea).toBeVisible({ timeout: 10_000 });
    // antd 对纯双字文本按钮自动插空格（“重 试”），用正则兼容。
    await expect(errorArea.getByRole('button', { name: /重\s*试/ })).toBeVisible();
    // 侧栏仍在（整树未崩）。
    await expect(page.locator('.chat-sidebar')).toBeVisible();

    // 场景 B（后端被 kill 的响应形态）：2xx + 空 body。openapi-fetch 此时
    // 返回 { data: undefined } 且无 error，queryFn resolve undefined，
    // pages=[undefined]；修复前 hasNextPage 会在渲染期对 undefined 调
    // getNextPageParam（lastPage.has_more）直接抛错整树崩溃——这是走查
    // 实测「kill 后端落在首次加载窗口」的精确复现路径。
    await page.unroute('**/api/conversations/*/messages*');
    await page.route('**/api/conversations/*/messages*', (route) =>
      route.fulfill({ status: 200, body: '' }),
    );
    await page.reload();

    await expect(page.getByText('页面出现意外错误')).toHaveCount(0);
    await expect(page.locator('.chat-sidebar')).toBeVisible({ timeout: 10_000 });
    // 页面保持可用：输入框可见（未被错误边界整树替换）。
    await expect(page.getByLabel('消息输入框')).toBeVisible();
  });

  test('流式审批等待中刷新触发原生离开确认；坚持离开后显示已停止/已取消', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    // 刷新被 beforeunload 拦下：dismiss（选择留下）后页面不导航、流继续。
    const dialogPromise = page.waitForEvent('dialog');
    const reloadPromise = page.reload().catch(() => undefined);
    const dialog = await dialogPromise;
    expect(dialog.type()).toBe('beforeunload');
    await dialog.dismiss();
    await reloadPromise;
    await expect(page).toHaveURL(new RegExp(`/c/${cid}`));
    await expect(approvalCard.getByText('待处理').first()).toBeVisible();

    // 坚持离开（accept）：刷新作废本轮（后端流断开即取消审批）。
    page.once('dialog', (leave) => {
      void leave.accept();
    });
    await page.reload();

    // API 侧确认后端已把审批落为 expired（note=stream_cancelled），
    // 消除「后端还没处理完断流」与「前端拉取」之间的竞态，再加载最终状态。
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`/api/approvals`, {
            params: { conversation_id: cid, limit: 10 },
          });
          const payload = (await response.json()) as {
            items: Array<{ status: string; note?: string }>;
          };
          return payload.items[0]?.status ?? '';
        },
        { timeout: 20_000 },
      )
      .toBe('expired');
    await page.reload();

    // 审批卡：流取消显示「已取消」而非「已超时」，note 不再直出英文标识。
    const cancelledCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(cancelledCard.getByText('已取消', { exact: true })).toBeVisible();
    await expect(cancelledCard.getByText('本轮已取消（回答流已中断，审批未生效）')).toBeVisible();
    await expect(cancelledCard.getByText('备注：回答流已中断，本轮审批自动取消')).toBeVisible();
    await expect(cancelledCard.getByText('已超时')).toHaveCount(0);

    // assistant 消息：用户侧断流落库为「已停止」，不再标红「生成失败」。
    const stoppedMessage = page.locator('.chat-message--assistant', { hasText: '已停止' }).last();
    await expect(stoppedMessage).toBeVisible();
    await expect(page.getByText('生成失败')).toHaveCount(0);
  });

  test('流式中删除当前会话被 409 拒绝：确认弹窗自动关闭，页面可继续操作', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);
    await expect(page.locator('.chat-approval', { hasText: '命令审批' }).first()).toBeVisible({
      timeout: 30_000,
    });

    // 侧栏当前会话（按 data-conv-id 精确定位）→ 菜单 → 删除 → 确认。
    const item = page.locator(`.chat-conv-item[data-conv-id="${cid}"]`);
    await item.hover();
    await item
      .getByRole('button')
      .filter({ has: page.locator('.anticon-more') })
      .click();
    await page.getByRole('menuitem', { name: '删除' }).click();

    const deleteDialog = page.getByRole('dialog', { name: /删除会话/ });
    await expect(deleteDialog).toBeVisible();
    // antd 对纯双字文本按钮自动插空格（“删 除”），用正则兼容。
    await deleteDialog.getByRole('button', { name: /删\s*除/ }).click();

    // 后端 409 STREAM_ACTIVE：确认弹窗必须自动关闭（遮罩撤掉），
    // 错误 Alert 可见且不再被遮挡。
    await expect(deleteDialog).toBeHidden({ timeout: 10_000 });
    const sidebarError = page.locator('.chat-sidebar-error');
    await expect(sidebarError).toBeVisible();
    await expect(sidebarError.getByText('正在进行的回答')).toBeVisible();

    // 页面未被遮罩锁死：新建会话按钮可点（流式进行中会弹离开确认）。
    await page.getByTitle('新建会话').click();
    const confirm = page.getByRole('dialog', { name: '当前回答仍在进行' });
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: '继续等待' }).click();

    // 收尾：停止流，避免审批悬挂影响其他用例。
    await page
      .getByRole('button', { name: '停止' })
      .click()
      .catch(() => undefined);
  });
});
