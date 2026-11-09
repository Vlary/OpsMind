import { expect, test, type Page } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  CODE_BLOCK_CONTENT,
  CODE_FINAL,
  ERROR_FINAL,
  SCENARIO_CODE,
  SCENARIO_ERROR,
  SCENARIO_RISKY,
  sendMessage,
} from './helpers';

// 第 1 轮打磨项的端到端验证：消息/代码块复制、审批卡中文风险说明、
// 历史失败消息的重新生成。

// 回答的收尾文本（CODE_FINAL 最后一行）：出现即代表该轮流式输出完成，
// 之后点击复制才不会复制到半截内容。
const CODE_TAIL = CODE_FINAL.split('\n').at(-1) ?? '';

async function readClipboard(page: Page): Promise<string> {
  return page.evaluate(() => navigator.clipboard.readText());
}

test.describe('复制与重新生成', () => {
  // clipboard-read/write 让断言可以直接读剪贴板（localhost 是安全上下文）。
  test.use({ storageState: AUTH_STATE_FILE, permissions: ['clipboard-read', 'clipboard-write'] });

  test('代码块右上角复制按钮把代码写入剪贴板', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_CODE);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const codeblock = page.locator('.opsmind-codeblock-wrap', {
      hasText: 'journalctl -u nginx',
    });
    await expect(codeblock).toBeVisible({ timeout: 30_000 });
    // 等这轮流式输出完成（收尾文本上屏）再复制，否则会复制到半截代码。
    await expect(page.getByText(CODE_TAIL)).toBeVisible({ timeout: 30_000 });

    await codeblock.locator('.opsmind-codeblock-copy').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(CODE_BLOCK_CONTENT);
  });

  test('悬停 user/assistant 气泡出现复制按钮并复制正文', async ({ page }) => {
    await page.goto('/chat');
    const marker = '复制正文验证消息标记';
    await sendMessage(page, `${SCENARIO_CODE} ${marker}`);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.locator('.chat-message--assistant .opsmind-markdown').first()).toBeVisible({
      timeout: 30_000,
    });
    // assistant 正文是逐 token 上屏的，等收尾文本出现再复制，断言才稳定。
    await expect(page.getByText(CODE_TAIL)).toBeVisible({ timeout: 30_000 });

    // user 气泡：悬停后复制按钮可见，复制内容为展示正文。
    const userBubble = page.locator('.chat-message--user', { hasText: marker }).first();
    await userBubble.hover();
    await userBubble.locator('.chat-copy-button').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toContain(marker);

    // assistant 气泡：复制 markdown 原文。
    const assistantBubble = page.locator('.chat-message--assistant').last();
    await assistantBubble.hover();
    await assistantBubble.locator('.chat-copy-button').click();
    const copied = await readClipboard(page);
    expect(copied).toContain('```bash');
    expect(copied).toContain('journalctl -u nginx');
  });

  test('审批卡风险原因显示中文说明，原始 reason 折叠在规则详情', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    // rm -rf /tmp/... 命中 RM_ANY：主文案是中文说明，不再是英文短语。
    await expect(approvalCard.getByText('删除文件', { exact: true })).toBeVisible();

    // 原始英文 reason 折叠在「规则详情」里，展开后可见。
    const detail = approvalCard.locator('.chat-approval-rule-detail');
    await detail.getByText('规则详情（RM_ANY）').click();
    await expect(detail.getByText('recursive/forced delete')).toBeVisible();

    // 收尾：拒绝审批，避免悬挂 pending 影响其他用例。
    await approvalCard.getByRole('button', { name: '拒绝' }).click();
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
  });

  test('历史失败消息刷新后出现重新生成按钮，点击后新一轮成功', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_ERROR);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 首次调用：假 LLM 返回 500，本轮失败（生成失败标签可见）。
    await expect(page.getByText('生成失败').first()).toBeVisible({ timeout: 30_000 });

    // 刷新后只剩持久化的 error 消息：重新生成按钮必须出现。
    await page.reload();
    const failedMessage = page.locator('.chat-message--assistant', { hasText: '生成失败' }).last();
    await expect(failedMessage).toBeVisible();
    const regenerate = failedMessage.getByRole('button', { name: '重新生成' });
    await expect(regenerate).toBeVisible();

    // 点击后以该轮 user 原文重新发起一轮：假 LLM 本次恢复正常文本。
    await regenerate.click();
    await expect(page.getByText(ERROR_FINAL).first()).toBeVisible({ timeout: 30_000 });

    // 新一轮成功后，重新生成按钮不再出现（最后一条 assistant 不再是 error）。
    await expect(regenerate).toHaveCount(0);
  });
});
