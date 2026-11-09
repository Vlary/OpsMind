import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE, SCENARIO_LIST, SCENARIO_RISKY, conversationIdOf, sendMessage } from './helpers';

// 六期打磨 sprint 4 走查修复的端到端断言（真实后端 + 假 LLM + Postgres；
// 回放数据按契约允许用路由伪造）。
//
// 覆盖 5 条用户可见缺陷：
//  1. 审计页「按动作筛选」能搜到五期/六期动作（设置变更/批量…），且表格
//     动作列不再回落英文原码；
//  2. 回放输出折叠面板里点「复制输出」不再连带开合面板；
//  3. 1280×720 / 1366×768 侧栏末两项（批量/设置）能滚到折叠条上方，点击
//     标题中部正常跳转（此前被 fixed 折叠条吞掉，点击变成折叠侧栏）；
//  4. 审批 TTL 到期的卡片正文与 Tag 同源「已过期」，不再出现「已超时」；
//  5. 回放截断横幅按原因二选一（审批满页不再说「超出 20000 步上限」）。
//
// 项 1（私钥无 passphrase）与项 6（采集不可达消息中文化）是后端修复，
// 分别由 internal/{monitor,inspection,diagnosis} 与 internal/monitor 的
// 单测钉死；此处只覆盖前端可见部分（错误列中文兜底）。

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
  // 回放入口在流式进行中禁用（契约 8.4）；全文可见后 SSE 仍可能未收尾，
  // 先等按钮可用再点（全量并行跑时假 LLM 被两 worker 共享，收尾更慢）。
  const replayButton = page.getByRole('button', { name: '回放' });
  await expect(replayButton).toBeEnabled({ timeout: 30_000 });
  await replayButton.click();
  await expect(page.locator('.replay-steps')).toBeVisible();
}

test.use({ storageState: AUTH_STATE_FILE });

// ---------------------------------------------------------------------------
// 1. 审计页动作筛选与动作列中文（medium）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 4：审计页五期/六期动作可筛选', () => {
  test('动作下拉输入「设置」「批量」能命中，表格显示中文而非原码', async ({ page }) => {
    // 自种两条审计：settings.update（PUT /api/settings）与 batch.created
    // （POST /api/batch/jobs）。走查实测这两类动作此前搜不到、列里是原码。
    const settingsResponse = await page.request.put('/api/settings', {
      headers: { Origin: ORIGIN },
      data: { approval_mode: 'standard' },
    });
    expect(settingsResponse.ok()).toBeTruthy();

    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();
    const rows = page.locator('.ant-table-row');
    await expect(rows.first()).toBeVisible({ timeout: 20_000 });

    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    const dropdown = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)');
    const options = dropdown.locator('.ant-select-item-option');

    /** 打开下拉、键入关键词，返回可见选项文本（antd 的 Select 本体即搜索输入）。 */
    async function matchOptions(keyword: string): Promise<string[]> {
      await actionSelect.click();
      await expect(dropdown).toBeVisible();
      await page.keyboard.press('Control+a');
      await page.keyboard.press('Delete');
      await page.keyboard.type(keyword);
      await expect(options.first()).toBeVisible({ timeout: 10_000 });
      const texts = await options.allInnerTexts();
      await page.keyboard.press('Escape');
      return texts.map((text) => text.trim());
    }

    // 走查实测：修复前「设置」→ []、「批量」→ []（「登录」→ 4 项说明搜索可用）。
    expect(await matchOptions('设置')).toContain('设置变更');
    expect(await matchOptions('批量')).toEqual(
      expect.arrayContaining(['创建批量任务', '取消批量任务']),
    );
    expect((await matchOptions('巡检')).length).toBeGreaterThanOrEqual(2);

    // 选「设置变更」→ 表格动作列显示中文标签，原码只作为副行。
    await actionSelect.click();
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('设置变更');
    await page.locator('.ant-select-item-option', { hasText: '设置变更' }).first().click();
    const settingsRow = rows.filter({ hasText: '设置变更' }).first();
    await expect(settingsRow).toBeVisible({ timeout: 20_000 });
    // 动作列主行是中文标签（修复前主行就是英文原码）；原码只作为副行之一。
    // 注意审计表有展开列，按列序取 td 会偏移，故按文本定位。
    await expect(settingsRow.getByText('设置变更', { exact: true })).toHaveCount(1);
    await expect(
      settingsRow.locator('.opsmind-admin-cell-sub', { hasText: 'settings.update' }),
    ).toHaveCount(1);
    // 目标列资源类型「设置」有中文（资源 ID 为 approval.mode 的副行）。
    await expect(settingsRow).toContainText('设置');
  });
});

// ---------------------------------------------------------------------------
// 2. 折叠面板内的复制按钮不触发开合（medium）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 4：折叠面板复制按钮不连带开合', () => {
  test('回放输出面板：点「复制输出」不改变展开态（连点 3 次）', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000c1',
      truncated: false,
      truncated_reason: '',
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_uptime__',
        }),
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:01Z',
          kind: 'execution',
          tool_call_id: '00000000-0000-0000-0000-0000000000e1',
          name: 'ssh_exec',
          command: 'uptime',
          status: 'succeeded',
          exit_code: 0,
          output: ' 10:00:01 up 3 days,  1 user,  load average: 0.10, 0.20, 0.30',
          duration_ms: 42,
        }),
      ],
    });

    await page.getByRole('button', { name: '下一步' }).click();
    const execStep = page.locator('.replay-step').nth(1);
    await expect(execStep).toHaveAttribute('data-kind', 'execution');

    const collapse = execStep.locator('.ant-collapse-item').first();
    const copy = execStep.getByLabel('复制输出');
    await expect(copy).toBeVisible();
    // 默认收起（sprint 1 起长输出默认折叠）。
    await expect(collapse).not.toHaveClass(/ant-collapse-item-active/);

    // 走查实测：每次点复制都 toggle。连点 3 次必须始终收起。
    for (let round = 1; round <= 3; round += 1) {
      await copy.click();
      await page.waitForTimeout(150);
      await expect(
        collapse,
        `round ${round}: 复制不该展开/收起输出面板`,
      ).not.toHaveClass(/ant-collapse-item-active/);
    }

    // 面板本身仍可正常开合（未因 stopPropagation 变成死区）。
    await collapse.locator('.ant-collapse-header').click();
    await expect(collapse).toHaveClass(/ant-collapse-item-active/);
    await collapse.locator('.ant-collapse-header').click();
    await expect(collapse).not.toHaveClass(/ant-collapse-item-active/);
  });
});

// ---------------------------------------------------------------------------
// 3. 小视口侧栏末项不被折叠条压住（medium）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 4：小视口侧栏末项可点击', () => {
  for (const viewport of [
    { width: 1366, height: 768 },
    { width: 1280, height: 720 },
  ]) {
    test(`${viewport.width}×${viewport.height}：「设置」项文字中部点击跳转设置页`, async ({
      page,
    }) => {
      await page.setViewportSize(viewport);
      await page.goto('/chat');
      const sider = page.locator('.ant-layout-sider');
      await expect(sider).toBeVisible({ timeout: 20_000 });

      // 滚到底，让末项进入可视区（修复前即使滚到底也被 fixed 折叠条覆盖）。
      await sider.evaluate((element) => {
        element.scrollTop = element.scrollHeight;
      });
      const settingsItem = sider
        .locator('.ant-menu-item', { hasText: '设置' })
        .first();
      await expect(settingsItem).toBeVisible();

      // 命中测试：项中心点必须命中该项自身（而不是 .ant-layout-sider-trigger）。
      const hitIsTrigger = await settingsItem.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const hit = document.elementFromPoint(
          box.left + box.width / 2,
          box.top + box.height / 2,
        );
        return Boolean(hit?.closest('.ant-layout-sider-trigger'));
      });
      expect(hitIsTrigger, '末项中心点不得被折叠条覆盖').toBe(false);

      // 真实点击标题中部 → 跳转 /settings，且侧栏未被折叠。
      await settingsItem.click();
      await expect(page).toHaveURL(/\/settings$/, { timeout: 15_000 });
      const collapsed = await sider.evaluate((element) =>
        element.classList.contains('ant-layout-sider-collapsed'),
      );
      expect(collapsed, '点击菜单项不得折叠侧栏').toBe(false);
    });
  }
});

// ---------------------------------------------------------------------------
// 5. 回放截断横幅按原因二选一（low）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 4：回放截断文案与原因一致', () => {
  test('审批满页截断显示「审批记录过多」，不再说「超出 20000 步上限」', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000c2',
      truncated: true,
      truncated_reason: 'approvals',
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_uptime__',
        }),
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:01Z',
          kind: 'approval',
          approval_id: '00000000-0000-0000-0000-0000000000a1',
          source: 'chat',
          server_name: 'wt-host',
          command: 'rm -rf /tmp/aiops-riskcol',
          risk_level: 'needs_approval',
          status: 'pending',
        }),
      ],
    });

    const banner = page.locator('.ant-alert', { hasText: '审批记录过多' });
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('仅显示最近 2 步');
    // 走查实测：修复前横幅写「超出 20000 步上限，仅显示最近 200 步」。
    await expect(page.getByText(/超出 20000 步上限/)).toHaveCount(0);
  });

  test('步数上限截断仍显示「超出 20000 步上限」', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000c3',
      truncated: true,
      truncated_reason: 'steps',
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_uptime__',
        }),
      ],
    });
    await expect(page.getByText('超出 20000 步上限，仅显示最近 1 步')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 4. 审批卡 expired 文案统一（low）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 4：审批卡过期措辞与 Tag 同源', () => {
  test('TTL 到期的审批卡只出现「已过期」，不再出现「已超时」', async ({ page }) => {
    // 复用 chat-coverage 的超时链路：等满 AIOPS_APPROVAL_TTL（45s）后卡片
    // 进入 expired 终态。这里只断言措辞，不重复做工具卡断言。
    test.setTimeout(150_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    await expect
      .poll(
        async () => {
          const response = await page.request.get('/api/approvals', {
            params: { conversation_id: cid, limit: 10 },
          });
          const payload = (await response.json()) as { items: Array<{ status: string }> };
          return payload.items[0]?.status ?? '';
        },
        { timeout: 110_000, intervals: [2000] },
      )
      .toBe('expired');

    // 正文 Alert「审批已过期」与 Tag「已过期」同源；「已超时」彻底消失。
    await expect(approvalCard.getByText('审批已过期', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(approvalCard.getByText('已超时')).toHaveCount(0);
  });
});
