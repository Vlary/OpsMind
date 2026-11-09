import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { expect, test } from '@playwright/test';

import {
  APPROVAL_TTL_SECONDS,
  AUTH_STATE_FILE,
  DEFAULT_FINAL,
  FAKE_LLM_URL,
  RISKY_FINAL,
  SCENARIO_RISKY,
  SCENARIO_SLOW,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 聊天链路补充覆盖：审批倒计时与超时路径、停止生成后重新生成、
// 服务器/数据库上下文选择随请求发送、长会话「加载更早消息」分页。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

/** 直连 e2e postgres 批量插入消息（长会话分页用例；每轮数据库重建，天然幂等）。 */
async function insertMessages(conversationId: string, count: number) {
  const sql = [
    `INSERT INTO messages (conversation_id, turn_id, role, content, status, created_at)`,
    `SELECT '${conversationId}'::uuid, gen_random_uuid(),`,
    `  CASE WHEN g % 2 = 0 THEN 'user' ELSE 'assistant' END,`,
    `  '长会话消息 #' || g, 'complete',`,
    `  now() - interval '1 second' * (${count} + 10 - g)`,
    `FROM generate_series(1, ${count}) AS g;`,
  ].join(' ');
  await execFileAsync(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      'db',
      'psql',
      '-U',
      'aiops',
      '-d',
      'aiops_e2e',
      '-v',
      'ON_ERROR_STOP=1',
      '-c',
      sql,
    ],
    { cwd: repoRoot },
  );
}

test.describe('聊天链路补充覆盖', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('审批倒计时展示，TTL 到期后已超时、工具 APPROVAL_TIMEOUT、模型收尾', async ({ page }) => {
    // global-setup 以 AIOPS_APPROVAL_TTL=45s 启动后端：等满 TTL 即可触发超时路径。
    test.setTimeout((APPROVAL_TTL_SECONDS + 90) * 1_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    // 倒计时以「剩余 M:SS」展示（45s TTL，卡片渲染 1-5s → 剩余 0:3x-0:4x）。
    await expect(approvalCard.getByText(/剩余 0:[0-4]\d/)).toBeVisible();
    // 决策按钮可用：不做任何决策，等 TTL 到期。
    await expect(approvalCard.getByRole('button', { name: '批准' })).toBeEnabled();

    await expect
      .poll(
        async () => {
          const response = await page.request.get('/api/approvals', {
            params: { conversation_id: cid, limit: 10 },
          });
          const payload = (await response.json()) as { items: Array<{ status: string }> };
          return payload.items[0]?.status ?? '';
        },
        { timeout: (APPROVAL_TTL_SECONDS + 20) * 1_000, intervals: [2_000] },
      )
      .toBe('expired');

    // 卡片终态是「已过期」（TTL 到期），不是流断开的「已取消」。
    // 六期打磨 sprint 4：措辞统一为「已过期」（此前正文写「已超时」而
    // 同卡 Tag 写「已过期」，同一屏两种说法）。
    await expect(approvalCard.getByText('已过期', { exact: true }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(approvalCard.getByText(/已超时/)).toHaveCount(0);
    await expect(approvalCard.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(approvalCard.getByRole('button', { name: '拒绝' })).toHaveCount(0);

    // 工具卡：审批超时 → 状态「已过期」（中文渲染，不显示裸码）。
    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    await expect(sshCard.getByText('已过期').first()).toBeVisible({ timeout: 30_000 });
    // 命令从未执行：卡片不得出现 45s 级「N ms」耗时（走查 B3 打磨）。
    await expect(sshCard.getByText(/\d+ ms/)).toHaveCount(0);

    // 模型收到超时结果后照常收尾（第 3 次假 LLM 调用）。
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });
  });

  test('停止生成：确认后标记已停止，刷新出现重新生成，重发跑通完整回答', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_SLOW);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    // 慢速流式进行中（SLOW_FINAL ≈ 170 字 × 80ms ≈ 14s 窗口）。
    // 等第一段正文上屏再停止：确保 SSE 已建立（user 消息已落库），
    // 停止后后端才会以 CLIENT_DISCONNECTED 落库部分内容。
    await expect(page.locator('.chat-message--assistant .opsmind-markdown').first()).toBeVisible({
      timeout: 30_000,
    });
    await page.locator('.chat-composer').getByRole('button', { name: '停止' }).click();
    // 纯流式（无 pending 审批）单击直停，不再弹「停止当前回答？」二次确认
    // （round 4 打磨：仅 pending 审批会作废时才确认）。
    await expect(page.getByRole('dialog', { name: '停止当前回答？' })).toHaveCount(0);

    // live 回合标记「已停止」（不是红色的「生成失败」）。
    await expect(page.getByText('已停止').first()).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('生成失败')).toHaveCount(0);

    // 后端断流后异步落库部分内容 + error（WithoutCancel 5s 窗口），
    // 轮询消息接口直到 assistant error 消息出现再刷新。
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`/api/conversations/${cid}/messages`, {
            params: { limit: 10 },
          });
          const payload = (await response.json()) as {
            items: Array<{ role: string; status: string; error_code: string }>;
          };
          // messages 按时间升序返回：最后一条才是本轮最新消息。
          const last = payload.items[payload.items.length - 1];
          return last?.role === 'assistant' && last.status === 'error' ? last.error_code : '';
        },
        { timeout: 30_000, intervals: [2_000] },
      )
      .toBe('CLIENT_DISCONNECTED');

    await page.reload();
    const stoppedMessage = page.locator('.chat-message--assistant', { hasText: '已停止' }).last();
    await expect(stoppedMessage).toBeVisible();
    const regenerate = stoppedMessage.getByRole('button', { name: '重新生成' });
    await expect(regenerate).toBeVisible();

    // 重新生成：以该轮 user 原文再发起一轮，慢速流完整跑完（尾句出现 = 全部 token 上屏）。
    await regenerate.click();
    await expect(page.getByText('慢速流的结尾标记：全部 token 发送完毕。').first()).toBeVisible({
      timeout: 45_000,
    });
    // 新一轮成功后，最后一条 assistant 不再是 error，重新生成按钮消失。
    await expect(page.getByRole('button', { name: '重新生成' })).toHaveCount(0);
  });

  test('服务器/数据库上下文选择随请求发送，气泡展示剥离前缀后的正文', async ({ page }) => {
    // 预置一个数据库供上下文选择（不测连接，仅入名单）。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: 'http://localhost:4173' },
      data: {
        name: 'e2e-ctx-db',
        engine: 'postgres',
        host: '127.0.0.1',
        port: 5432,
        database_name: 'aiops',
        username: 'aiops',
        password: 'aiops',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/chat');
    // 顶栏多选（第 1 个是服务器，第 2 个是数据库）。antd v6 虚拟列表里
    // option 对 Playwright 不可见、且 multiple 默认按 value（UUID）过滤：
    // 打开下拉等其真正展开后 Enter 选中高亮首项，以选中 Tag 验证。
    const serverSelect = page.locator('.chat-topbar .ant-select-multiple').first();
    await serverSelect.click();
    // 上一个下拉的离场动画会短暂并存：取最新打开的那个。
    await expect(
      page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)').last(),
    ).toBeVisible();
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(serverSelect).toContainText('e2e-fake-host');

    // 数据库下拉项数随并行用例而变：选中首项后从 Tag 读取实际库名再断言。
    const databaseSelect = page.locator('.chat-topbar .ant-select-multiple').nth(1);
    await databaseSelect.click();
    // 上一个下拉的离场动画会短暂并存：取最新打开的那个。
    await expect(
      page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)').last(),
    ).toBeVisible();
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    const dbTag = await databaseSelect.locator('.ant-select-selection-item').first().textContent();
    expect(dbTag).toBeTruthy();

    const marker = '上下文前缀验证标记';
    await sendMessage(page, `普通巡检 ${marker}`);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // fake-llm 收到的 user 消息里定位本次请求：并行用例的请求会交错
    // 覆盖「最后一条」，从最近 50 条历史中找含 marker 与服务器前缀的那条。
    let captured = '';
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`${FAKE_LLM_URL}/e2e/last-user`);
          const payload = (await response.json()) as { history?: string[] };
          captured =
            payload.history?.find(
              (item) => item.includes(marker) && item.includes('servers: e2e-fake-host'),
            ) ?? '';
          return captured !== '';
        },
        { timeout: 30_000, intervals: [1_000] },
      )
      .toBe(true);
    expect(captured.startsWith('<!--opsmind-context')).toBe(true);
    expect(captured).toContain(`servers: e2e-fake-host (id=`);
    expect(captured).toContain(`databases: ${dbTag?.trim()} (id=`);
    expect(captured.endsWith(`\n普通巡检 ${marker}`)).toBe(true);

    // 用户气泡展示剥离前缀的正文，页面上不出现上下文注释。
    await expect(page.locator('.chat-message--user', { hasText: marker }).first()).toBeVisible();
    await expect(page.getByText('opsmind-context')).toHaveCount(0);
    // 默认场景照常回答。
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible({ timeout: 30_000 });
  });

  test('长会话「加载更早消息」：首屏 100 条，加载后按钮消失、最早消息可见', async ({ page }) => {
    const created = await page.request.post('/api/conversations', {
      headers: { Origin: 'http://localhost:4173' },
      data: { title: '长会话分页验证' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    await insertMessages(id, 150);

    await page.goto(`/c/${id}`);
    // 首次加载最新 100 条（#51..#150），存在更早消息。
    await expect(page.getByRole('button', { name: '加载更早消息' })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('长会话消息 #150', { exact: true })).toBeVisible();
    await expect(page.getByText('长会话消息 #1', { exact: true })).toHaveCount(0);

    // 点击加载第二页（#1..#50）：150 条取完，按钮消失。
    await page.getByRole('button', { name: '加载更早消息' }).click();
    await expect(page.getByRole('button', { name: '加载更早消息' })).toHaveCount(0, {
      timeout: 20_000,
    });

    // 虚拟滚动只渲染视口附近：滚回顶部后最早消息可见，「回到底部」出现。
    await page.locator('.chat-messages').evaluate((element) => {
      element.scrollTop = 0;
    });
    await expect(page.getByText('长会话消息 #1', { exact: true })).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('button', { name: '回到底部' })).toBeVisible();

    await page.getByRole('button', { name: '回到底部' }).click();
    await expect(page.getByRole('button', { name: '回到底部' })).toHaveCount(0, {
      timeout: 10_000,
    });
    await expect(page.getByText('长会话消息 #150', { exact: true })).toBeVisible();
  });
});
