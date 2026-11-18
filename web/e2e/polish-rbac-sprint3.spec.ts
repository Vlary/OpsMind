import { expect, test } from './fixtures';
import {
  AUDITOR_STATE_FILE,
  OPERATOR_STATE_FILE,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// RBAC 走查 sprint 3 端到端验证：
// 1) 运维工程师（无 approvals:read）在会话页全程不请求 GET /api/approvals
//    （该角色对该端点 403 是契约 12.2 的有意权限矩阵），刷新后页面顶部没有
//    「无权访问」错误条、也没有 403 响应。
// 2) 刷新会中断回答流、审批被连带取消；无审批读权限的运维只在工具卡上看到
//    「回答流已中断，本轮审批已取消，请重新发起」，信息不再只剩一个「已过期」。
// 3) 审计员（auditor）的 /recordings 与 /audit、/replays 一致，顶部渲染
//    「审计员为只读角色」提示。

test.describe('RBAC 走查 sprint 3：会话页审批查询按 approvals:read 收敛', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('刷新后不请求 /api/approvals、无 403 错误条，工具卡保留取消说明', async ({ page }) => {
    const approvalRequests: string[] = [];
    const approvalForbidden: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/approvals?')) {
        approvalRequests.push(request.url());
      }
    });
    page.on('response', (response) => {
      if (response.url().includes('/api/approvals?') && response.status() === 403) {
        approvalForbidden.push(response.url());
      }
    });

    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 实时审批卡经 SSE 出现（无 approvals:read 也不影响发起者看到等待卡片）。
    const liveCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(liveCard).toBeVisible({ timeout: 30_000 });
    await expect(liveCard.getByText('等待审批者处理')).toBeVisible();
    // 刷新前有常驻可见提示（原生 beforeunload 之外的第二道告知）：
    // 刷新/离开会中断回答流并取消本轮审批。
    await expect(page.getByTestId('pending-approval-leave-hint')).toContainText(
      '刷新或离开页面会中断回答流并取消审批',
    );
    // 整个过程（含发送审批后）一次审批列表请求都没有。
    expect(approvalRequests).toEqual([]);
    expect(conversationIdOf(page)).toMatch(/[0-9a-f-]+/);

    // 刷新 = 中断回答流。服务端把审批取消、tool_call 收敛为
    // expired/APPROVAL_CANCELLED；给后端留出落库时间后重新加载。
    // 提示出现前最多重载 3 次（有限重试，防后端收敛晚于首个请求的窗口）。
    const notice = page.getByTestId('approval-cancelled-notice');
    for (let attempt = 0; attempt < 3; attempt += 1) {
      await page.waitForTimeout(attempt === 0 ? 300 : 1200);
      await page.reload();
      await expect(page.locator('.chat-toolcall').first()).toBeVisible({ timeout: 20_000 });
      if ((await notice.count()) > 0) {
        break;
      }
    }

    await expect(notice).toBeVisible({ timeout: 20_000 });
    await expect(notice).toContainText('回答流已中断，本轮审批已取消，请重新发起');

    // 页面级错误横幅不存在；审批卡（需 approvals:read）也不再出现。
    await expect(page.getByText('无权访问')).toHaveCount(0);
    await expect(page.locator('.chat-error-area')).toHaveCount(0);
    await expect(page.locator('.chat-approval')).toHaveCount(0);

    // 刷新链路同样零审批请求、零 403。
    expect(approvalRequests).toEqual([]);
    expect(approvalForbidden).toEqual([]);
  });
});

test.describe('RBAC 走查 sprint 3：审计员录制页只读提示', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('/recordings 顶部展示「审计员为只读角色」', async ({ page }) => {
    await page.goto('/recordings');
    await expect(page.getByRole('heading', { name: '录制' })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('审计员为只读角色')).toBeVisible();
    await expect(
      page.getByText('可查看审计、回放与录制；任何写操作都会被服务端拒绝。'),
    ).toBeVisible();
  });
});
