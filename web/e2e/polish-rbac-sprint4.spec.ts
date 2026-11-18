import { expect, test } from './fixtures';
import { AUDITOR_STATE_FILE } from './helpers';

// RBAC 走查 sprint 4：审计员的哈希链校验入口（契约 12.1）。
//
// 1) 审计员在 /audit 能直接触发只读 GET /api/audit/verify，不再只能手工调 API；
// 2) 全新库的链必须返回「校验通过」并展示 checked / 链头（走查第 4 轮 high
//    的纳秒精度与 jsonb 明细规范化问题已由后端修复，否则这里会稳定 409）；
// 3) 只读身份不受影响：审计员提示仍在，且校验不产生任何写请求。

test.describe('RBAC 走查 sprint 4：审计员校验哈希链', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('只读校验按钮返回链完整结论并展示 checked/链头', async ({ page }) => {
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('审计员为只读角色')).toBeVisible();

    const verifyResponses: number[] = [];
    const writeRequests: string[] = [];
    page.on('response', (response) => {
      if (new URL(response.url()).pathname === '/api/audit/verify') {
        verifyResponses.push(response.status());
      }
    });
    page.on('request', (request) => {
      if (request.method() !== 'GET' && request.url().includes('/api/audit')) {
        writeRequests.push(`${request.method()} ${request.url()}`);
      }
    });

    const verifyButton = page.getByRole('button', { name: /校验哈希链/ });
    await expect(verifyButton).toBeVisible();
    await verifyButton.click();

    const result = page.getByTestId('audit-verify-result');
    await expect(result).toBeVisible({ timeout: 20_000 });
    await expect(result).toContainText('哈希链校验通过');
    // RBAC 第 2 轮走查：窗口有界时文案带覆盖区间（「已校验 200 条（#485–#684）」），
    // 全链覆盖时才是「已校验 N 条，链头 #N」。两者都必须出现链头 id。
    await expect(result).toContainText(/已校验 \d+ 条(（#\d+–#\d+）)?/);
    await expect(result).toContainText(/链头 #\d+/);
    await expect(result).toContainText('链头哈希');
    // 只读校验：一次 200，且没有对 /api/audit 的写请求。
    expect(verifyResponses).toEqual([200]);
    expect(writeRequests).toEqual([]);
  });
});
