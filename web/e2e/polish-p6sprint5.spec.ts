import { expect, test, type Page } from './fixtures';
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
  // 八期 FE-R：Modal 下线，直接经 /replays/<id> 打开播放器页。
  await page.goto(`/replays/${cid}`);
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
    await expect(page.locator('.ant-select-item-option', { hasText: '命令预览' })).toHaveCount(0);
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
// 3. 批量详情：00012 起审批禁止改写，执行行恒为原命令
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：批量详情执行原命令（禁止改写）', () => {
  test('批准原命令 uptime 后，详情行显示 uptime 且无「用户改写」标注', async ({ page }) => {
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
    await drawer
      .getByRole('button', { name: /查\s*看/ })
      .first()
      .click();

    const modal = page.locator('.ant-modal').filter({ hasText: '查看命令并批准' });
    await expect(modal).toBeVisible();
    // 00012 定稿：编辑弹窗只读，不再提供改写框。
    await expect(
      modal.getByText('本版本不支持审批时改写命令。如需修改，请拒绝后由发起人重新发起'),
    ).toBeVisible();
    await expect(modal.getByLabel('改写命令')).toHaveCount(0);
    await modal.getByRole('button', { name: '批准原命令' }).click();

    // 假 sshd 拒绝 exec（夹具只服务终端 PTY）→ 行落「执行失败」；执行的是
    // 审批原命令 uptime，不得出现「用户改写」标注。
    await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
    const row = drawer.locator('.ant-table-row', { hasText: name }).first();
    await expect(row).toBeVisible();
    await expect(row.getByText('用户改写')).toHaveCount(0);
    await expect(row.getByText('实际执行')).toHaveCount(0);
    await expect(row.locator('.opsmind-admin-mono')).toContainText('uptime');
    await expect(row).not.toContainText('whoami');
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
// 5. 工具卡命令面板（00012：审批禁止改写）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：工具卡命令面板保持原命令', () => {
  test('无改写入口：批准后命令面板仍为「命令」，无「实际执行」标注', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    // 00012：审批时禁止改写，卡片没有编辑命令/保存并批准入口。
    await expect(approvalCard.getByRole('button', { name: '编辑命令' })).toHaveCount(0);
    await expect(approvalCard.getByLabel('改写命令')).toHaveCount(0);
    await approvalCard.getByRole('button', { name: '批准' }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    // 00012：edited_command 恒为空 → 面板标题保持「命令」，正文是实际执行的
    // 原命令；不再出现「模型原命令 / 实际执行 / 用户改写」。
    const commandPanel = sshCard.locator('.ant-collapse-item').first();
    await expect(commandPanel.getByText('命令', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(commandPanel).toContainText(RISKY_COMMAND);
    await expect(commandPanel.getByText('模型原命令')).toHaveCount(0);
    await expect(commandPanel.getByText('实际执行')).toHaveCount(0);
    await expect(commandPanel.getByText('用户改写')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 6. 设置页免审批警示去字段名
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 5：免审批警示不含数据库字段名', () => {
  test('免审批档警示用「自动执行」开关名，不含 auto_exec_enabled', async ({ page }) => {
    // 00012：单管理员不能把全局门限放宽到 none（需第二人批准），这里用
    // GET /api/settings 的测试替身渲染 none 档 UI，只验证警示文案。
    await page.route('**/api/settings', async (route) => {
      if (route.request().method() !== 'GET') {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ approval_mode: 'none' }),
      });
    });
    await page.goto('/settings');
    const card = page.locator('.ant-card', { hasText: '审批门限' });
    await expect(card).toBeVisible();

    const alert = page.locator('.ant-alert', {
      hasText: '所有命令将自动执行（含高危）',
    });
    await expect(alert).toBeVisible({ timeout: 10_000 });
    await expect(alert).toContainText('关闭了「自动执行」开关的服务器');
    await expect(alert).not.toContainText('auto_exec_enabled');
  });
});
