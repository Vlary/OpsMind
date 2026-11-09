import { expect, test, type Page } from '@playwright/test';
import {
  AUTH_STATE_FILE,
  RISKY_COMMAND,
  SCENARIO_LIST,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 六期打磨 sprint 5 走查修复的端到端断言（真实后端 + 假 LLM + 假 sshd +
// Postgres；回放数据按契约允许用路由伪造）。
//
// 覆盖 8 条用户可见缺陷：
//  1. 审计页「命令批准」筛选项此前永远 0 行（后端 0 处写 command.approved）
//     ——本轮后端补写，选「命令批准」能命中真实审批行；「命令预览」是
//     永空死选项（预览统一落 risk.preview），已从下拉移除；
//  2. 审计页「审批通过 / 审批拒绝」行的「命令摘要」列此前恒「—」
//     ——本轮 detail 无条件带 command，行列可读；
//  3. 批量详情已执行行显示「实际执行」改写命令 + 「用户改写」Tag；
//  4. 回放审批步来源中文化（chat → 对话），不再全站唯一中英混排；
//  5. 工具卡改写后命令面板拆「模型原命令 / 实际执行」；
//  6. 设置页免审批警示不再出现 `auto_exec_enabled=false` 字段名；
//  7. 对话页顶栏标题有 title（完整标题可 hover 查看）；
//  8. /chat 有页头「对话」，nav / 页头 / 标签页 title 三方一致。

const ORIGIN = 'http://localhost:4173';

/** 回放单步的完整字段集（openapi ReplayStep：未用字段为 null）。 */
function replayStep(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    seq: 1,
    timestamp: '2026-10-30T10:00:00Z',
    kind: 'user_message',
    message_id: null,
    tool_call_id: null,
    approval_id: null,
    name: null,
    source: null,
    server_name: null,
    content: null,
    reasoning_duration_ms: null,
    command: null,
    original_command: null,
    arguments: null,
    risk_level: null,
    edited_risk_level: null,
    status: null,
    exit_code: null,
    output: null,
    error_code: null,
    approval_mode: null,
    duration_ms: null,
    ...overrides,
  };
}

async function openReplayWith(page: Page, body: unknown): Promise<void> {
  await sendMessage(page, SCENARIO_LIST);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  await expect(page.getByText('资产清单核对完毕：当前共').first()).toBeVisible({
    timeout: 30_000,
  });
  const cid = conversationIdOf(page);
  await page.route(`**/api/conversations/${cid}/replay`, (route) =>
    route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) }),
  );
  const replayButton = page.getByRole('button', { name: '回放' });
  await expect(replayButton).toBeEnabled({ timeout: 30_000 });
  await replayButton.click();
  await expect(page.locator('.replay-steps')).toBeVisible();
}

test.use({ storageState: AUTH_STATE_FILE });

// ---------------------------------------------------------------------------
// 1 + 2. 审计：命令批准可筛选、审批行命令摘要可读
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：审计命令批准与审批行命令摘要', () => {
  test('审批通过后「命令批准」可筛选，审批行「命令摘要」不再恒「—」', async ({ page }) => {
    // 真链路产一条审批：risky 场景 → 审批卡 → 直接「批准」（不改写）。
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: /批\s*准/ }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    // 本会话 ID 精确定位（并行 spec 也会产生 command.approved 行——例如
    // 批量改写用例的 uptime，取 items[0] 会串台）。
    const cid = conversationIdOf(page);
    let approvedRow:
      | { action: string; detail: Record<string, unknown>; conversation_id: string | null }
      | undefined;
    for (let attempt = 0; attempt < 20 && approvedRow === undefined; attempt += 1) {
      const response = await page.request.get('/api/audit', {
        params: { action: 'command.approved', limit: 50 },
      });
      expect(response.ok()).toBeTruthy();
      const body = (await response.json()) as {
        items: Array<{
          action: string;
          detail: Record<string, unknown>;
          conversation_id: string | null;
        }>;
      };
      approvedRow = body.items.find((item) => item.conversation_id === cid);
      if (approvedRow === undefined) {
        await page.waitForTimeout(500);
      }
    }
    expect(approvedRow, 'command.approved 必须有真实写入点（此前后端 0 条）').toBeDefined();
    expect(approvedRow?.detail['command']).toBe(RISKY_COMMAND);

    // 审计页：选「命令批准」后表格有行，且「命令摘要」列显示命令全文。
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();
    await expect(page.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    await actionSelect.click();
    const dropdown = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)');
    await expect(dropdown).toBeVisible();
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('命令批准');
    const option = page.locator('.ant-select-item-option', { hasText: '命令批准' }).first();
    await expect(option).toBeVisible({ timeout: 10_000 });
    await option.click();

    const rows = page.locator('.ant-table-row');
    await expect(rows.first()).toBeVisible({ timeout: 20_000 });
    const approvedRowInTable = rows.filter({ hasText: '命令批准' }).first();
    await expect(approvedRowInTable).toBeVisible();
    // 「命令摘要」列不再恒「—」：该行含原命令文本。
    await expect(approvedRowInTable).toContainText(RISKY_COMMAND);

    // 「命令预览」不再出现在下拉里（永空死选项，六期打磨 sprint 5）。
    await actionSelect.click();
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('命令预览');
    await expect(
      page.locator('.ant-select-item-option', { hasText: '命令预览' }),
    ).toHaveCount(0);
    await page.keyboard.press('Escape');
  });

  test('审批拒绝行展开后有「完整命令」（chat 拒绝带 command）', async ({ page }) => {
    // 真链路：risky 场景 → 审批卡 → 「拒绝」。断言 approval.denied 的
    // detail.command（审计页「命令摘要」列与展开「完整命令」的来源）。
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: /拒\s*绝/ }).click();
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    let denied: { detail: Record<string, unknown>; conversation_id: string | null } | undefined;
    for (let attempt = 0; attempt < 20 && denied === undefined; attempt += 1) {
      const response = await page.request.get('/api/audit', {
        params: { action: 'approval.denied', limit: 20 },
      });
      expect(response.ok()).toBeTruthy();
      const body = (await response.json()) as {
        items: Array<{ detail: Record<string, unknown>; conversation_id: string | null }>;
      };
      denied = body.items.find((item) => item.conversation_id === cid);
      if (denied === undefined) {
        await page.waitForTimeout(500);
      }
    }
    expect(denied, 'approval.denied 必须落审计').toBeDefined();
    expect(denied?.detail['command'], '拒绝行 detail 必须带原命令').toBe(RISKY_COMMAND);

    // 审计页该行「命令摘要」列可读（不再恒「—」）。
    await page.goto('/audit');
    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    await actionSelect.click();
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('审批拒绝');
    await page.locator('.ant-select-item-option', { hasText: '审批拒绝' }).first().click();
    const row = page
      .locator('.ant-table-row', { hasText: '审批拒绝' })
      .filter({ hasText: RISKY_COMMAND })
      .first();
    await expect(row).toBeVisible({ timeout: 20_000 });
  });
});

// ---------------------------------------------------------------------------
// 3. 批量详情：已执行行显示实际执行的改写命令
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：批量详情显示实际执行命令', () => {
  test('改写为 uptime 批准并执行后，详情行显示「实际执行 uptime」+ 用户改写', async ({
    page,
  }) => {
    const name = `e2e-batch-p6s5-${Date.now()}`;
    const created = await page.request.post('/api/servers', {
      headers: { Origin: ORIGIN },
      data: {
        name,
        host: '127.0.0.1',
        port: 18083,
        os: 'linux',
        auth_method: 'password',
        username: 'e2e',
        password: 'e2e-ssh-pass',
        auto_exec_enabled: false,
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/batch');
    await expect(page.getByPlaceholder('df -h /')).toBeVisible();
    await page.getByPlaceholder('df -h /').fill('uptime');
    await page.getByRole('combobox', { name: '目标服务器' }).click();
    const option = page.locator('.ant-select-item-option', { hasText: name }).first();
    for (let i = 0; i < 120 && !(await option.isVisible()); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await option.click();
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: '创建并分派' }).click();

    const drawer = page.getByRole('dialog');
    await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
    await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
    await drawer.getByRole('button', { name: /编\s*辑/ }).first().click();

    const modal = page.locator('.ant-modal').filter({ hasText: '编辑命令并批准' });
    await expect(modal).toBeVisible();
    const editor = modal.getByLabel('改写命令');
    await expect(editor).toHaveValue('uptime');
    await editor.fill('whoami');
    await expect(modal.getByText('改写后风险：')).toBeVisible({ timeout: 10_000 });
    await modal.getByRole('button', { name: /保存并批准/ }).click();

    // 假 sshd 拒绝 exec（夹具只服务终端 PTY）→ 行落「执行失败」，但
    // 命令列必须显示实际下发的 whoami + 「用户改写」。
    await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
    const row = drawer.locator('.ant-table-row', { hasText: name }).first();
    await expect(row).toBeVisible();
    await expect(row.getByText('实际执行')).toBeVisible();
    await expect(row.getByText('用户改写')).toBeVisible();
    await expect(row).toContainText('whoami');
    // 不再把 job 原文命令当作这一行实际执行的内容。
    await expect(row.locator('.opsmind-admin-mono')).toContainText('whoami');
  });
});

// ---------------------------------------------------------------------------
// 4. 回放审批来源中文化
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：回放审批来源中文化', () => {
  test('审批步头部显示「对话」而非 chat', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000bb',
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '把危险命令改写后再执行',
        }),
        replayStep({
          seq: 2,
          kind: 'approval',
          approval_id: '00000000-0000-0000-0000-000000000002',
          tool_call_id: '00000000-0000-0000-0000-000000000003',
          source: 'chat',
          server_name: 'e2e-fake-host',
          command: 'uptime',
          original_command: RISKY_COMMAND,
          risk_level: 'needs_approval',
          edited_risk_level: 'safe',
          status: 'approved',
        }),
      ],
    });
    await page.getByRole('button', { name: '下一步' }).click();
    const approvalStep = page.locator('.replay-step--active');
    await expect(approvalStep).toHaveAttribute('data-kind', 'approval');
    // 中文来源 Tag；不得再直出 chat。
    await expect(approvalStep.getByText('对话', { exact: true })).toBeVisible();
    await expect(approvalStep.getByText('chat', { exact: true })).toHaveCount(0);
    // 同一步其它中文标签仍在（无中英混排）。
    await expect(approvalStep.getByText('已批准')).toBeVisible();
    await expect(approvalStep.getByText('用户改写')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 5. 工具卡改写后命令面板
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：工具卡显示模型原命令与实际执行', () => {
  test('改写为 uptime 批准后，命令面板标题为「模型原命令」并补「实际执行」', async ({
    page,
  }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: '编辑命令' }).click();
    const editor = page.getByLabel('改写命令');
    await expect(editor).toBeVisible();
    await editor.fill('uptime');
    await expect(approvalCard.getByText('改写后风险：')).toBeVisible({ timeout: 10_000 });
    await approvalCard.getByRole('button', { name: '保存并批准' }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    // 命令面板标题变「模型原命令」，正文仍是模型原始参数（审计事实）。
    const commandPanel = sshCard.locator('.ant-collapse-item').first();
    await expect(commandPanel.getByText('模型原命令')).toBeVisible({ timeout: 20_000 });
    await expect(commandPanel).toContainText(RISKY_COMMAND);
    // 同一面板内补「实际执行 uptime」+「用户改写」。
    await expect(commandPanel.getByText('实际执行')).toBeVisible();
    await expect(commandPanel.getByText('用户改写')).toBeVisible();
    await expect(commandPanel).toContainText('uptime');
  });
});

// ---------------------------------------------------------------------------
// 6. 设置页免审批警示去字段名
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：免审批警示不含数据库字段名', () => {
  test('切到「免审批」后警示用「自动执行」开关名，不含 auto_exec_enabled', async ({ page }) => {
    await page.goto('/settings');
    const card = page.locator('.ant-card', { hasText: '审批门限' });
    await expect(card).toBeVisible();
    const select = card.locator('.ant-select').first();

    await select.click();
    await page.getByRole('option', { name: '免审批', exact: true }).click();
    const alert = page.locator('.ant-alert', {
      hasText: '所有命令将自动执行（含高危）',
    });
    await expect(alert).toBeVisible({ timeout: 10_000 });
    await expect(alert).toContainText('关闭了「自动执行」开关的服务器');
    await expect(alert).not.toContainText('auto_exec_enabled');

    // 收尾恢复默认档（避免影响其他并行审批用例）。
    await select.click();
    await page.getByRole('option', { name: '危险命令审批', exact: true }).click();
    await expect(card.getByText('当前 · 危险命令审批')).toBeVisible({ timeout: 10_000 });
  });
});

// ---------------------------------------------------------------------------
// 7 + 8. 对话页顶栏 tooltip 与页头三方一致
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：对话页标题', () => {
  test('顶栏标题有 title 属性；页头「对话」与 nav/标签页一致', async ({ page }) => {
    const longTitle = `WALK_EXEC-p6sprint5-${Date.now()}-${'x'.repeat(40)}`;
    const created = await page.request.post('/api/conversations', {
      headers: { Origin: ORIGIN },
      data: { title: longTitle },
    });
    expect(created.status()).toBe(201);
    const cid = ((await created.json()) as { id: string }).id;

    await page.goto(`/c/${cid}`);
    const titleEl = page.locator('.chat-topbar-title');
    await expect(titleEl).toBeVisible({ timeout: 20_000 });
    // 7：超长标题被 CSS 截断，但 title 属性给完整值（侧栏同名条目同口径）。
    await expect(titleEl).toHaveAttribute('title', longTitle);
    const box = await titleEl.boundingBox();
    expect(box?.width ?? 0).toBeLessThanOrEqual(260);

    // 8：页头「对话」= 导航当前项 = 标签页 title（全站唯一原缺页头的页面）。
    await expect(page.getByRole('heading', { name: '对话', exact: true })).toBeVisible();
    await expect(page).toHaveTitle('对话 · 云枢 OpsMind');
    const selectedNav = page.locator('.ant-menu-item-selected').first();
    await expect(selectedNav).toContainText('对话');

    // 空态页（/chat 新会话）同样有页头「对话」，但会话标题显示「新会话」。
    await page.goto('/chat');
    await expect(page.getByRole('heading', { name: '对话', exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.locator('.chat-topbar-title')).toHaveText('新会话');
    await expect(page.getByText('开始新的运维对话').first()).toBeVisible();
  });
});
