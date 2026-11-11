import { expect, test, type Page, type Route } from '@playwright/test';
import { AUTH_STATE_FILE, SCENARIO_LIST, conversationIdOf, sendMessage } from './helpers';

// FE-R（八期）端到端：独立回放页 `/replays`（契约 9.4 八期 e2e 清单 ①–⑨）。
//
// 数据策略：会话与审计行走真实链路（vite preview → 后端 → 假 LLM/Postgres），
// 回放载荷与目录按契约允许的路由伪造下发——后端 `/api/replays` 由 BE-R 交付，
// 前端用例不依赖其就绪；`/api/conversations/:id/replay` 本就支持伪造
// （六期 spec 同口径）。

const ORIGIN = 'http://localhost:4173';

/** 回放单步的完整字段集（openapi ReplayStep：未用字段为 null）。 */
function step(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    seq: 1,
    timestamp: '2026-11-10T10:00:00Z',
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
    result_summary: null,
    result_truncated: null,
    affected_servers: null,
    diagnosis_id: null,
    inspection_report_id: null,
    batch_job_id: null,
    ...overrides,
  };
}

/** 目录单行（ReplayCatalogItem 字段全集）。 */
function catalogRow(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    conversation_id: '00000000-0000-0000-0000-0000000000a1',
    title: '磁盘告警排查',
    last_message_at: '2026-11-10T10:00:00Z',
    tool_count: 0,
    action_count: 0,
    approval_count: 0,
    has_actions: false,
    truncated: false,
    ...overrides,
  };
}

/** 六步回放（六类 kind 齐全；含改写审批 + 执行输出）。 */
function sixStepBody(conversationId: string, reason = ''): string {
  return JSON.stringify({
    conversation_id: conversationId,
    steps: [
      step({ seq: 1, kind: 'user_message', content: '帮我检查磁盘' }),
      step({ seq: 2, kind: 'reasoning', content: '先采集事实。', reasoning_duration_ms: 1000 }),
      step({
        seq: 3,
        kind: 'tool_call',
        tool_call_id: 'tc-1',
        name: 'ssh_exec',
        arguments: { server_id: 'srv', command: 'df -h' },
        risk_level: 'needs_approval',
        status: 'succeeded',
        approval_mode: 'standard',
        result_summary: 'exit=0 · 128B',
      }),
      step({
        seq: 4,
        kind: 'approval',
        tool_call_id: 'tc-1',
        approval_id: 'ap-1',
        source: 'chat',
        server_name: 'web-1',
        command: 'df -h',
        original_command: 'rm -rf /tmp/x',
        risk_level: 'needs_approval',
        edited_risk_level: 'safe',
        status: 'approved',
      }),
      step({
        seq: 5,
        kind: 'execution',
        tool_call_id: 'tc-1',
        name: 'ssh_exec',
        command: 'df -h',
        status: 'succeeded',
        exit_code: 0,
        output: 'Filesystem 40G 12G 26G 32%',
        duration_ms: 120,
        result_summary: 'df 输出 1 行',
        result_truncated: false,
        affected_servers: ['web-1'],
      }),
      step({ seq: 6, kind: 'assistant_text', content: '磁盘使用率 32%。' }),
    ],
    truncated: reason !== '',
    truncated_reason: reason,
  });
}

/** 20000 步 fixture（虚拟滚动用例）：全部 user_message，内容短。 */
function hugeBody(conversationId: string, count: number): string {
  const steps: Record<string, unknown>[] = [];
  for (let index = 1; index <= count; index += 1) {
    steps.push(
      step({
        seq: index,
        timestamp: '2026-11-10T10:00:00Z',
        kind: 'user_message',
        content: `第 ${index} 步`,
      }),
    );
  }
  return JSON.stringify({
    conversation_id: conversationId,
    steps,
    truncated: true,
    truncated_reason: 'steps',
  });
}

/** 只匹配 `/api/replays`（不能误伤 `/api/conversations/<id>/replay`）。 */
async function mockCatalog(page: Page, items: Record<string, unknown>[]): Promise<void> {
  await page.route(
    (url) => url.pathname === '/api/replays',
    (route: Route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({ items, has_more: false, next_cursor: null }),
      }),
  );
}

async function mockReplay(page: Page, conversationId: string, body: string): Promise<void> {
  await page.route(`**/api/conversations/${conversationId}/replay`, (route) =>
    route.fulfill({ contentType: 'application/json', body }),
  );
}

test.use({ storageState: AUTH_STATE_FILE });

// ---------------------------------------------------------------------------
// ① 空目录空态
// ---------------------------------------------------------------------------
test.describe('八期：回放页目录空态', () => {
  test('/replays 空目录显示「暂无可回放的会话」与去对话入口', async ({ page }) => {
    await mockCatalog(page, []);
    await page.goto('/replays');
    // 页头与导航同标签（契约 8.3.10 三层一致）。
    await expect(page.getByRole('heading', { name: '回放执行' })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('暂无可回放的会话')).toBeVisible();
    await expect(page.getByRole('button', { name: '去对话页' })).toBeVisible();
    // 未选中会话：右栏提示从左侧选择。
    await expect(page.getByText('从左侧选择一个会话查看执行回放')).toBeVisible();
    await expect(page.locator('.ant-modal')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// ② 目录选会话 → 右栏渲染；徽标与伪造目录一致
// ---------------------------------------------------------------------------
test.describe('八期：目录选择与计数徽标', () => {
  test('点选目录行后播放器渲染，工具/审批徽标与伪造目录一致', async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000b1';
    await mockCatalog(page, [
      catalogRow({
        conversation_id: cid,
        title: '磁盘告警排查',
        tool_count: 7,
        action_count: 3,
        approval_count: 2,
        has_actions: true,
      }),
    ]);
    await mockReplay(page, cid, sixStepBody(cid));

    await page.goto('/replays');
    const row = page.locator('.replay-catalog-item').first();
    await expect(row).toContainText('磁盘告警排查');
    await expect(row).toContainText('工具 7');
    await expect(row).toContainText('审批 2');
    await expect(row).toContainText('有动作');

    await row.click();
    await expect(page).toHaveURL(new RegExp(`/replays/${cid}$`));
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');
    // 选中行高亮（浏览器后退必须能回上一个会话，故用 push）。
    await expect(page.locator('.replay-catalog-item--active')).toHaveCount(1);
    await expect(page.locator('.ant-modal')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// ③ 三处入口统一跳页
// ---------------------------------------------------------------------------
test.describe('八期：三处入口统一跳 /replays/<id>', () => {
  test('导航「回放执行」进入页面', async ({ page }) => {
    await mockCatalog(page, []);
    await page.goto('/chat');
    const nav = page.locator('.ant-menu-item', { hasText: '回放执行' }).first();
    await expect(nav).toBeVisible({ timeout: 20_000 });
    await nav.click();
    await expect(page).toHaveURL(/\/replays$/);
    await expect(page.getByRole('heading', { name: '回放执行' })).toBeVisible();
  });

  test('对话页「回放执行」按钮跳页且不再出现 Modal', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_LIST);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByText('资产清单核对完毕：当前共').first()).toBeVisible({
      timeout: 30_000,
    });
    const cid = conversationIdOf(page);
    await mockReplay(page, cid, sixStepBody(cid));
    await mockCatalog(page, []);

    const button = page.getByRole('button', { name: '回放执行' });
    await expect(button).toBeEnabled({ timeout: 30_000 });
    await button.click();

    await expect(page).toHaveURL(new RegExp(`/replays/${cid}$`));
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.ant-modal')).toHaveCount(0);
  });

  test('审计行「回放」跳页且不再出现 Modal', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_LIST);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByText('资产清单核对完毕：当前共').first()).toBeVisible({
      timeout: 30_000,
    });
    const cid = conversationIdOf(page);
    await mockReplay(page, cid, sixStepBody(cid));
    await mockCatalog(page, []);

    await page.goto('/audit');
    await expect(page.locator('.ant-table')).toBeVisible();
    const filter = page.getByPlaceholder('会话 ID（conversation UUID）');
    await filter.fill(cid);
    await filter.press('Enter');
    const row = page.locator('.ant-table-row').first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.locator('.ant-table-row-expand-icon').click();
    const expanded = page.locator('.ant-table-expanded-row');
    await expect(expanded).toBeVisible();
    await expanded.getByRole('button', { name: '回放' }).click();

    await expect(page).toHaveURL(new RegExp(`/replays/${cid}$`));
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.ant-modal')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// ④ 三组筛选 + 组合筛选
// ---------------------------------------------------------------------------
test.describe('八期：kind / 工具 / 风险筛选', () => {
  const cid = '00000000-0000-0000-0000-0000000000b2';

  test.beforeEach(async ({ page }) => {
    await mockCatalog(page, [catalogRow({ conversation_id: cid, has_actions: true })]);
    await mockReplay(page, cid, sixStepBody(cid));
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();
  });

  test('kind 筛选：只留审批步，计数与进度以可见序列为准', async ({ page }) => {
    await page.getByRole('combobox', { name: '按步骤类型筛选' }).click();
    await page.getByRole('option', { name: '审批', exact: true }).click();
    await page.keyboard.press('Escape');

    await expect(page.getByText('已筛选：显示 1 / 6 步')).toBeVisible();
    await expect(page.locator('.replay-step')).toHaveCount(1);
    await expect(page.locator('.replay-step')).toHaveAttribute('data-kind', 'approval');
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 1 步');
  });

  test('工具筛选：只留 ssh_exec 的 tool_call / execution 步', async ({ page }) => {
    await page.getByRole('combobox', { name: '按工具筛选' }).click();
    await page.getByRole('option', { name: 'ssh_exec', exact: true }).click();
    await page.keyboard.press('Escape');

    await expect(page.getByText('已筛选：显示 2 / 6 步')).toBeVisible();
    const kinds = await page
      .locator('.replay-step')
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-kind')));
    expect(kinds.sort()).toEqual(['execution', 'tool_call']);
  });

  test('风险筛选：needs_approval 只留 tool_call 与 approval 步', async ({ page }) => {
    await page.getByRole('combobox', { name: '按风险级别筛选' }).click();
    await page.getByRole('option', { name: '需审批', exact: true }).click();
    await page.keyboard.press('Escape');

    await expect(page.getByText('已筛选：显示 2 / 6 步')).toBeVisible();
    const kinds = await page
      .locator('.replay-step')
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-kind')));
    expect(kinds.sort()).toEqual(['approval', 'tool_call']);
  });

  test('组合筛选（kind ∩ 工具）：交集为空显示空态与清除入口', async ({ page }) => {
    await page.getByRole('combobox', { name: '按步骤类型筛选' }).click();
    await page.getByRole('option', { name: '回答', exact: true }).click();
    await page.keyboard.press('Escape');
    await page.getByRole('combobox', { name: '按工具筛选' }).click();
    await page.getByRole('option', { name: 'ssh_exec', exact: true }).click();
    await page.keyboard.press('Escape');

    await expect(page.getByText('没有符合筛选条件的步骤')).toBeVisible();
    await page.getByRole('button', { name: '清除筛选' }).first().click();
    await expect(page.getByText('已筛选：显示', { exact: false })).toHaveCount(0);
    await expect(page.locator('.replay-step')).toHaveCount(6);
  });
});

// ---------------------------------------------------------------------------
// ⑤ 搜索命中与「下一个命中」
// ---------------------------------------------------------------------------
test.describe('八期：搜索命中与跳转', () => {
  const cid = '00000000-0000-0000-0000-0000000000b3';

  test('命中高亮、下一个命中跳转并显示第 k / K 个匹配', async ({ page }) => {
    await mockCatalog(page, [catalogRow({ conversation_id: cid, has_actions: true })]);
    // 三步都含 uptime：user_message / execution output / assistant_text。
    const body = JSON.stringify({
      conversation_id: cid,
      steps: [
        step({ seq: 1, kind: 'user_message', content: '跑一下 uptime' }),
        step({
          seq: 2,
          kind: 'execution',
          tool_call_id: 'tc-1',
          name: 'ssh_exec',
          command: 'uptime',
          status: 'succeeded',
          exit_code: 0,
          output: ' 10:00 up 3 days',
          duration_ms: 20,
        }),
        step({ seq: 3, kind: 'assistant_text', content: 'UPTIME 显示已运行 3 天。' }),
      ],
      truncated: false,
      truncated_reason: '',
    });
    await mockReplay(page, cid, body);
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();

    const search = page.getByLabel('搜索步骤');
    await search.fill('uptime');
    // 大小写不敏感：三步都命中（含大写的 UPTIME）。
    await expect(page.getByText('第 1 / 3 个匹配')).toBeVisible();
    await expect(page.locator('.replay-search-hit').first()).toContainText('uptime');
    await expect(page.locator('.replay-search-hit mark').first()).toHaveText('uptime');
    // 输入即跳到第一个命中。
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 3 步');

    await page.getByRole('button', { name: '下一个命中' }).click();
    await expect(page.getByText('第 2 / 3 个匹配')).toBeVisible();
    await expect(page.locator('.replay-counter')).toHaveText('第 2 / 3 步');

    await page.getByRole('button', { name: '上一个命中' }).click();
    await expect(page.getByText('第 1 / 3 个匹配')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// ⑥ 快捷键（含「在搜索框内输入不触发」）
// ---------------------------------------------------------------------------
test.describe('八期：快捷键', () => {
  const cid = '00000000-0000-0000-0000-0000000000b4';

  test.beforeEach(async ({ page }) => {
    await mockCatalog(page, [catalogRow({ conversation_id: cid, has_actions: true })]);
    await mockReplay(page, cid, sixStepBody(cid));
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();
    await page.locator('body').click({ position: { x: 5, y: 5 } });
  });

  test('←/→ 单步、Home/End 首末步、空格播放', async ({ page }) => {
    await page.keyboard.press('ArrowRight');
    await expect(page.locator('.replay-counter')).toHaveText('第 2 / 6 步');
    await page.keyboard.press('ArrowLeft');
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');

    await page.keyboard.press('End');
    await expect(page.locator('.replay-counter')).toHaveText('第 6 / 6 步');
    await page.keyboard.press('Home');
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');

    // 空格 = 播放；到末步自动暂停（终止条件）。
    await page.keyboard.press('Home');
    await page.keyboard.press('Space');
    await expect(page.locator('.replay-counter')).not.toHaveText('第 1 / 6 步', {
      timeout: 10_000,
    });
    await page.keyboard.press('Space');
  });

  test('/ 聚焦搜索、f 聚焦筛选、Esc 清空搜索', async ({ page }) => {
    await page.keyboard.press('/');
    await expect(page.getByLabel('搜索步骤')).toBeFocused();
    await page.keyboard.type('df');
    await expect(page.getByText('第 1 / 3 个匹配')).toBeVisible();

    // 输入即跳到第一个命中（含 command/arguments 的 3 处 df）：第 3 步。
    await expect(page.locator('.replay-counter')).toHaveText('第 3 / 6 步');
    // 搜索框内按 ←/→/Home/End 不推进播放器（控件自身行为，光标移动而已）。
    await page.keyboard.press('End');
    await expect(page.locator('.replay-counter')).toHaveText('第 3 / 6 步');

    // Esc 在输入框内让位给控件（antd Input 的 allowClear 不清空），先移出焦点。
    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await page.keyboard.press('Escape');
    await expect(page.getByLabel('搜索步骤')).toHaveValue('');
    // 清空搜索不改变当前步位置（只清搜索与筛选态）。
    await expect(page.locator('.replay-counter')).toHaveText('第 3 / 6 步');

    await page.keyboard.press('f');
    // 「f」聚焦筛选：antd Select 的 combobox 可访问名即 aria-label。
    await expect(page.getByRole('combobox', { name: '按步骤类型筛选' })).toBeFocused();
  });
});

// ---------------------------------------------------------------------------
// ⑦ 20000 步虚拟滚动
// ---------------------------------------------------------------------------
test.describe('八期：20000 步虚拟滚动', () => {
  test('DOM 中 .replay-step 远小于总步数；滚到底后末步可见', async ({ page }) => {
    test.setTimeout(120_000);
    const cid = '00000000-0000-0000-0000-0000000000b5';
    await mockCatalog(page, [catalogRow({ conversation_id: cid, has_actions: true })]);
    await mockReplay(page, cid, hugeBody(cid, 20000));

    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible({ timeout: 30_000 });
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 20000 步');

    // 首屏 DOM 节点数远小于总步数（契约 8.7 ⑦：< 200）。
    const mounted = await page.locator('.replay-step').count();
    expect(mounted).toBeLessThan(200);
    expect(mounted).toBeGreaterThan(0);

    // End 跳末步 → 末步可见（滚动定位到 20000）。
    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await page.keyboard.press('End');
    await expect(page.locator('.replay-counter')).toHaveText('第 20000 / 20000 步');
    await expect(page.locator('.replay-step--active')).toHaveAttribute('data-seq', '20000');
    await expect(page.locator('.replay-step--active')).toContainText('第 20000 步');

    // 滚到顶后 DOM 节点仍与总步数无关。
    await page.locator('.replay-steps').evaluate((element) => {
      element.scrollTop = 0;
    });
    const afterScroll = await page.locator('.replay-step').count();
    expect(afterScroll).toBeLessThan(200);
  });
});

// ---------------------------------------------------------------------------
// ⑧ truncated_reason 四值文案
// ---------------------------------------------------------------------------
test.describe('八期：截断原因四值文案', () => {
  const cases: Array<{ reason: string; expected: string }> = [
    { reason: 'steps', expected: '超出 20000 步上限，仅显示最近 6 步' },
    {
      reason: 'approvals',
      expected: '审批记录过多，仅显示最近 6 步（更早的审批未纳入回放）',
    },
    {
      reason: 'payload',
      expected: '会话内容过大，部分参数与输出未下发（仅保留最近 6 步的摘要）',
    },
  ];

  for (const item of cases) {
    test(`truncated_reason=${item.reason} 显示专属文案`, async ({ page }) => {
      const cid = `00000000-0000-0000-0000-0000000000c${item.reason.length}`;
      await mockCatalog(page, [catalogRow({ conversation_id: cid })]);
      await mockReplay(page, cid, sixStepBody(cid, item.reason));
      await page.goto(`/replays/${cid}`);
      await expect(page.getByText(item.expected)).toBeVisible();
    });
  }

  test("truncated_reason='' 不显示横幅", async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000c9';
    await mockCatalog(page, [catalogRow({ conversation_id: cid })]);
    await mockReplay(page, cid, sixStepBody(cid));
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.replay-banner')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// ⑨ payload 下 arguments=null 的 tool_call 步
// ---------------------------------------------------------------------------
test.describe('八期：体积预算下的参数缺失提示', () => {
  test('arguments=null 显示「参数未下发（体积预算）」而不是空 JSON 块', async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000d1';
    await mockCatalog(page, [catalogRow({ conversation_id: cid })]);
    const body = JSON.stringify({
      conversation_id: cid,
      steps: [
        step({
          seq: 1,
          kind: 'tool_call',
          tool_call_id: 'tc-1',
          name: 'ssh_exec',
          arguments: null,
          risk_level: 'safe',
          status: 'succeeded',
          result_summary: 'exit=0 · 12B',
        }),
      ],
      truncated: true,
      truncated_reason: 'payload',
    });
    await mockReplay(page, cid, body);
    await page.goto(`/replays/${cid}`);
    await expect(page.getByText('参数未下发（体积预算）')).toBeVisible();
    await expect(page.locator('.replay-steps .opsmind-codeblock')).toHaveCount(0);
    await expect(
      page.getByText('会话内容过大，部分参数与输出未下发（仅保留最近 1 步的摘要）'),
    ).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// ⑩ 八期扩展：action 标注、result_summary、affected_servers、result_truncated、深链
// ---------------------------------------------------------------------------
test.describe('八期：execution 步的工具化展示', () => {
  test('非命令类工具标「动作」、显示摘要/影响主机/截断 Tag 与批量深链', async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000d2';
    await mockCatalog(page, [catalogRow({ conversation_id: cid })]);
    const body = JSON.stringify({
      conversation_id: cid,
      steps: [
        step({
          seq: 1,
          kind: 'execution',
          tool_call_id: 'tc-1',
          name: 'container_list',
          command: 'docker ps -a',
          status: 'succeeded',
          output: '{"containers":[]}',
          duration_ms: 33,
          result_summary: '0 个容器',
          result_truncated: true,
          affected_servers: ['web-1', 'web-2', 'web-3', 'web-4'],
          batch_job_id: '00000000-0000-0000-0000-0000000000e9',
        }),
      ],
      truncated: false,
      truncated_reason: '',
    });
    await mockReplay(page, cid, body);
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();

    const card = page.locator('.replay-step').first();
    await expect(card.getByText('动作', { exact: true })).toBeVisible();
    await expect(card.getByText('0 个容器')).toBeVisible();
    await expect(card.getByText('输出已截断')).toBeVisible();
    // 多目标折叠为「N 台」Tag（Tooltip 内是全量清单）。
    await expect(card.getByText('4 台')).toBeVisible();
    await expect(card.getByRole('link', { name: '查看批量任务' })).toBeVisible();
    await expect(card.getByLabel('复制批量任务 ID')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 独立页刷新复原（URL 即状态）
// ---------------------------------------------------------------------------
test.describe('八期：URL 可复原', () => {
  test('直接打开 /replays/<id> 即定位该会话播放器', async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000d3';
    await mockCatalog(page, [catalogRow({ conversation_id: cid, title: '直链会话' })]);
    await mockReplay(page, cid, sixStepBody(cid));
    await page.goto(`/replays/${cid}`);
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');
    // 左栏目录仍可用且选中行高亮。
    await expect(page.locator('.replay-catalog-item--active')).toContainText('直链会话');
    expect(ORIGIN).toContain('4173');
  });
});
