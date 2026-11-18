// RBAC 第 12 轮走查的端到端回归（真实 Chromium + 真实会话/权限）：
//
// 1) 运维工程师在未配置模型供应商的部署里打开 /chat：三处空态都只显示
//    「请联系管理员配置模型供应商」，页面里没有任何指向 /providers 的链接
//    （此前点「配置模型」落到 403「无权访问「模型」」）；管理员保留原 CTA。
// 2) 运维工程师的 /monitor 空态改为「请联系管理员配置资产与采集」，同页
//    采集卡仍是只读（无 asset:write / monitor:manage）。
// 3) 审计员在回放页：execution 步不渲染 /alerts、/inspections、/batch 深链
//    （只有 ID 文本 + 复制按钮），空目录不渲染「去对话页」，因此不会被带到
//    403 页。
//
// 数据策略：权限/导航走真实 /api/auth/me；空态所需的「无供应商/无指标/
// 空目录」用路由伪造下发（只伪造只读响应，不绕过任何服务端鉴权）。
import { expect, test, type Page, type Route } from './fixtures';
import { AUTH_STATE_FILE, AUDITOR_STATE_FILE, OPERATOR_STATE_FILE } from './helpers';

const REPLAY_CONVERSATION_ID = '04b1fa07-7d94-4adf-b5d2-e8012c3fe504';
const DIAGNOSIS_ID = '11111111-1111-4111-8111-111111111111';
const INSPECTION_ID = '22222222-2222-4222-8222-222222222222';
const BATCH_ID = '33333333-3333-4333-8333-333333333333';

/** 只伪造 GET /api/providers（不误伤 /api/providers/presets 等）。 */
async function mockEmptyProviders(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname === '/api/providers',
    (route: Route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) }),
  );
}

/** 监控空态：无任何已采集服务器。 */
async function mockEmptyMonitor(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname === '/api/monitor/overview',
    (route: Route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({
          servers: [],
          firing: { info: 0, warning: 0, critical: 0, total: 0 },
          last_inspection: null,
          collector: { enabled: true, interval_seconds: 60 },
        }),
      }),
  );
}

/** 回放单步完整字段集（openapi ReplayStep：未用字段为 null）。 */
function replayStep(overrides: Record<string, unknown>): Record<string, unknown> {
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

async function mockEmptyCatalog(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname === '/api/replays',
    (route: Route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({ items: [], has_more: false, next_cursor: null }),
      }),
  );
}

async function mockExecutionReplay(page: Page): Promise<void> {
  const body = JSON.stringify({
    conversation_id: REPLAY_CONVERSATION_ID,
    steps: [
      replayStep({ seq: 1, kind: 'user_message', content: '排查磁盘告警' }),
      replayStep({
        seq: 2,
        kind: 'execution',
        name: 'run_diagnosis',
        command: 'diagnose disk',
        status: 'succeeded',
        result_summary: '诊断完成',
        diagnosis_id: DIAGNOSIS_ID,
        inspection_report_id: INSPECTION_ID,
        batch_job_id: BATCH_ID,
      }),
    ],
    truncated: false,
    truncated_reason: '',
  });
  await page.route(`**/api/conversations/${REPLAY_CONVERSATION_ID}/replay`, (route) =>
    route.fulfill({ contentType: 'application/json', body }),
  );
}

test.describe('第 12 轮：运维工程师的供应商/监控空态', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('/chat 未配置供应商：只显示联系管理员，不渲染 /providers 入口', async ({ page }) => {
    await mockEmptyProviders(page);
    await page.goto('/chat');

    await expect(page.getByText('请联系管理员配置模型供应商。').first()).toBeVisible({
      timeout: 20_000,
    });
    // 三处入口（输入框提示 / 顶栏 / 空态卡）都不再出现可点击的
    // 「配置模型 / 去模型管理」，整页也没有 /providers 链接。
    await expect(page.getByRole('button', { name: '配置模型' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: '去模型管理' })).toHaveCount(0);
    await expect(page.locator('a[href="/providers"]')).toHaveCount(0);
    await expect(page.getByRole('link', { name: '模型管理' })).toHaveCount(0);
    await expect(page.getByText('无权访问')).toHaveCount(0);
  });

  test('/monitor 无指标：联系管理员配置资产与采集（不再指向添加资产）', async ({ page }) => {
    await mockEmptyMonitor(page);
    await page.goto('/monitor');

    await expect(page.getByText('暂无服务器指标：请联系管理员配置资产与采集')).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('添加资产并开启采集')).toHaveCount(0);
    // 同页采集卡对运维工程师仍是只读（与空态文案一致）。
    await expect(page.getByText('当前角色只读，不能修改采集配置')).toBeVisible();
  });
});

test.describe('第 12 轮：管理员保留原有供应商 CTA', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('/chat 未配置供应商：管理员仍可从空态进入模型管理', async ({ page }) => {
    await mockEmptyProviders(page);
    await page.goto('/chat');

    await expect(page.getByRole('button', { name: '去模型管理' })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByRole('button', { name: '配置模型' })).toBeVisible();
    await expect(page.locator('a[href="/providers"]').first()).toBeVisible();
  });
});

test.describe('第 12 轮：审计员回放页的深链收敛', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('execution 深链不渲染，但 ID 文本与复制按钮保留', async ({ page }) => {
    await mockExecutionReplay(page);
    await page.goto(`/replays/${REPLAY_CONVERSATION_ID}`);

    await expect(page.locator('.replay-steps')).toBeVisible({ timeout: 20_000 });
    // 目标页无权限：三个链接都不出现（避免 403）。
    await expect(page.locator('a[href^="/alerts"]')).toHaveCount(0);
    await expect(page.locator('a[href^="/inspections"]')).toHaveCount(0);
    await expect(page.locator('a[href^="/batch"]')).toHaveCount(0);
    // 句柄仍可读、可复制（审计取证的交接材料）。
    await expect(page.getByText(DIAGNOSIS_ID)).toBeVisible();
    await expect(page.getByText(INSPECTION_ID)).toBeVisible();
    await expect(page.getByText(BATCH_ID)).toBeVisible();
    await expect(page.getByRole('button', { name: '复制诊断 ID' })).toBeVisible();
    await expect(page.getByRole('button', { name: '复制巡检报告 ID' })).toBeVisible();
    await expect(page.getByRole('button', { name: '复制批量任务 ID' })).toBeVisible();
    await expect(page.getByText('无权访问')).toHaveCount(0);
  });

  test('空目录不渲染「去对话页」（审计员无 chat:use）', async ({ page }) => {
    await mockEmptyCatalog(page);
    await page.goto('/replays');

    await expect(page.getByText('暂无可回放的会话')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByRole('button', { name: '去对话页' })).toHaveCount(0);
    await expect(page.locator('a[href="/chat"]')).toHaveCount(0);
  });
});
