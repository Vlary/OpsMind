import { expect, test } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  DEFAULT_FINAL,
  RISKY_FINAL,
  SCENARIO_RISKY,
  sendMessage,
} from './helpers';

// 第 4 轮打磨项的端到端验证：
//  - 流式（含审批等待）期间输入框保持可打字，Enter 只提示不发送，草稿保留；
//  - 审批已批准/已拒绝后不再渲染置灰的批准/拒绝按钮组；
//  - 同名服务器创建被 409 拒绝时，错误体 message 与前端弹窗文案都指向重名；
//  - 审计展开行改用 CodeBlock 渲染，提供一键复制。

test.describe('第 4 轮打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('审批等待期间可预输入：Enter 提示回答生成中，草稿保留，流结束后可发送', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 审批卡片出现 = SSE 仍挂着（streaming），这是最长可达 120s 的稳定窗口。
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    // 修复前：textarea 整体 disabled，无法预输入。
    const box = page.getByLabel('消息输入框');
    await expect(box).toBeEnabled();
    const next = '预输入的下一条巡检指令';
    await box.fill(next);

    // Enter 不发送，只提示「回答生成中」；草稿保留。
    await box.press('Enter');
    await expect(page.getByText(/回答生成中/)).toBeVisible();
    await expect(box).toHaveValue(next);

    // 批准并等流结束（假服务器不可达，工具失败后模型收尾 RISKY_FINAL）。
    await approvalCard.getByRole('button', { name: '批准' }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });

    // 流结束后发送按钮恢复，预输入的草稿可直接发出（闭环验证）。
    await expect(box).toHaveValue(next);
    await page.getByRole('button', { name: '发送' }).click();
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible({ timeout: 30_000 });
  });

  test('审批已批准后按钮组卸载，不再渲染置灰的批准/拒绝', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await expect(approvalCard.getByRole('button', { name: '批准' })).toBeVisible();

    await approvalCard.getByRole('button', { name: '拒绝' }).click();
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    // 修复前：两个按钮只是 disabled，仍渲染在卡片上与 Alert 重复。
    await expect(approvalCard.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(approvalCard.getByRole('button', { name: '拒绝' })).toHaveCount(0);
    // 状态行（Alert）仍在，最终状态可读。
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible();
  });

  test('创建同名服务器：409 message 指向重名，弹窗文案展示具体冲突', async ({ page }) => {
    // API 侧：错误体 message 是具体冲突（对齐 providers），不再是 "conflict"。
    const response = await page.request.post('/api/servers', {
      headers: { Origin: 'http://localhost:4173' },
      data: {
        name: 'e2e-fake-host', // globalSetup 已创建的同名服务器
        host: '127.0.0.9',
        port: 22,
        username: 'root',
        auth_method: 'password',
        password: 'not-a-real-password',
      },
    });
    expect(response.status()).toBe(409);
    const body = (await response.json()) as { error: { code: string; message: string } };
    expect(body.error.code).toBe('CONFLICT');
    // p3 sprint 1：message 由英文 "server name already exists" 场景化为
    // 中文（服务器与网络设备同走本分支，对齐数据库侧重名 409 具体化）。
    expect(body.error.message).toBe('服务器名称已存在');

    // UI 侧：表单弹窗的错误条把具体 message 展示给用户。
    await page.goto('/servers');
    await page.getByRole('button', { name: '新增服务器' }).click();
    const dialog = page.getByRole('dialog', { name: '新增服务器' });
    await expect(dialog).toBeVisible();
    await dialog.getByPlaceholder('例如 web-prod-01').fill('e2e-fake-host');
    await dialog.getByPlaceholder('192.0.2.10 或 host.example.com').fill('127.0.0.9');
    await dialog.getByPlaceholder('root').fill('root');
    await dialog.getByPlaceholder('请输入密码').fill('not-a-real-password');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();

    await expect(
      dialog.getByText('资源冲突或当前状态不允许该操作（服务器名称已存在）'),
    ).toBeVisible();
  });

  test.describe('审计展开行 CodeBlock 复制', () => {
    // CodeBlock 走 navigator.clipboard（localhost:4173 是安全上下文），
    // 显式授权避免 headless 下剪贴板写入被拒后走降级路径。
    test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

    test('展开审计行：原始 detail 用 CodeBlock 渲染并带复制按钮', async ({ page }) => {
      await page.goto('/audit');
      const table = page.locator('.ant-table');
      await expect(table).toBeVisible();
      // 审计按 id 倒序分页，前序测试的聊天/审批事件会把最早的 seed 记录
      // 挤出第一页；这里取当前页第一行——「原始 detail」块对任何记录都渲染。
      const row = table.locator('.ant-table-row').first();
      await expect(row).toBeVisible({ timeout: 20_000 });

      // 修复前：展开行内是裸 <pre>，复制按钮数为 0。
      await row.locator('.ant-table-row-expand-icon').click();
      const expanded = page.locator('.ant-table-expanded-row');
      await expect(expanded).toBeVisible();
      const copyButtons = expanded.locator('.opsmind-codeblock-copy');
      await expect(copyButtons.first()).toBeVisible();
      expect(await copyButtons.count()).toBeGreaterThanOrEqual(1);

      // 原始 detail 无条件渲染为 JSON 代码块，点击复制有成功反馈。
      await expect(expanded.getByText('原始 detail')).toBeVisible();
      const blocks = expanded.locator('.opsmind-codeblock');
      expect(await blocks.count()).toBeGreaterThanOrEqual(1);
      await copyButtons.first().click();
      await expect(page.getByText('已复制').first()).toBeVisible();
    });
  });
});
