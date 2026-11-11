import { expect, test, type Page } from '@playwright/test';
import {
  AUTH_STATE_FILE,
  CODE_BLOCK_CONTENT,
  SCENARIO_CODE,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 六期打磨 sprint 6 走查修复的端到端断言（真实后端 + 假 LLM + Postgres；
// 需要伪数据的用例按契约允许走路由 mock）。
//
// 覆盖 8 条用户可见缺陷：
//  1. 监控「网络入/网络出」读数 8 倍虚高（Linux 采集乘 8）——单测锚定，
//     这里在真实页面上断言同一数值的展示口径（MB/s 字节单位）；
//  2. 容器操作弹窗 confirm 态恒称「将创建审批」，而 none/high 档实际直接
//     执行——按门限渲染「确认执行」+「立即执行…并记录审计」；
//  3. 回放 assistant_text 步的 Markdown 代码围栏原样显示——改用与聊天页
//     同源的 LazyMarkdownView（代码块高亮 + 复制代码）；
//  4. 对话拒绝命令的审计行缺 command/risk_level——审计页命令摘要可读；
//  5. 容器弹窗 TTL 到期文案「已超时」→ 全站统一「已过期」；
//  6. 容器 REST 直执行审计补 approval_mode（后端单测锚定，页面不直接展示）；
//  7. 巡检报告详情抽屉平均评分大字恒绿——按分档着色；
//  8. 审批过期审计只有聚合行——逐条明细（approval_id/command）。

const ORIGIN = 'http://localhost:4173';

test.use({ storageState: AUTH_STATE_FILE });

// ---------------------------------------------------------------------------
// 1. 监控网络速率单位（字节/秒，展示 MB/s）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 6：监控网络速率按字节/秒展示', () => {
  test('1,000,000 B/s 显示「1.00 MB/s」，不再是乘 8 的「8.00 MB/s」', async ({ page }) => {
    const created = await page.request.post('/api/servers', {
      headers: { Origin: ORIGIN },
      data: {
        name: `p6s6-net-${Date.now()}`,
        host: '127.0.0.1',
        port: 1,
        os: 'linux',
        auth_method: 'password',
        username: 'e2e',
        password: 'e2e-pass',
        auto_exec_enabled: false,
      },
    });
    expect(created.status()).toBe(201);
    const serverId = ((await created.json()) as { id: string }).id;

    // overview 与 metrics 均 mock 成同一台服务器 + 1,000,000 B/s 的读数
    //（真实 60s 内 /proc/net/dev +60,000,000 字节的解析结果）。
    const serverName = `p6s6-net-${serverId.slice(0, 8)}`;
    await page.route('**/api/monitor/overview', (route) =>
      route.fulfill({
        json: {
          servers: [
            {
              server_id: serverId,
              server_name: serverName,
              created_at: '2026-11-08T10:00:00Z',
              cpu_pct: 12.5,
              mem_pct: 43.1,
              disk_pct: 61,
              load_1: 0.35,
              net_in_bps: 1_000_000,
              net_out_bps: 2_000_000,
              updated_at: '2026-11-08T10:00:00Z',
            },
          ],
          firing: { total: 0, critical: 0, warning: 0, info: 0 },
          last_inspection: null,
          collector: { enabled: true, interval_seconds: 60 },
        },
      }),
    );
    await page.route(`**/api/servers/${serverId}/metrics*`, (route) =>
      route.fulfill({
        json: {
          server_id: serverId,
          points: [
            {
              created_at: '2026-11-08T10:00:00Z',
              cpu_pct: 12.5,
              mem_pct: 43.1,
              disk_pct: 61,
              load_1: 0.35,
              net_in_bps: 1_000_000,
              net_out_bps: 2_000_000,
            },
          ],
          truncated: false,
        },
      }),
    );

    try {
      await page.goto('/monitor');
      const netInCard = page.locator('.ant-card', { hasText: '网络入' }).first();
      await expect(netInCard).toBeVisible({ timeout: 20_000 });
      // 字节/秒口径：1,000,000 B/s = 1.00 MB/s（乘 8 会显示 8.00 MB/s）。
      await expect(netInCard.getByText('当前 1.00 MB/s')).toBeVisible();
      await expect(netInCard.getByText(/8\.00 MB\/s/)).toHaveCount(0);
      // 复制当前值同样带单位（8.7 ⑥）。
      await expect(netInCard.getByLabel('复制网络入当前值')).toBeVisible();
    } finally {
      await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
    }
  });
});

// ---------------------------------------------------------------------------
// 2 + 5. 容器操作弹窗：confirm 态分流文案 + TTL 文案「已过期」
// ---------------------------------------------------------------------------
const CONTAINER_ID = 'c1d2e3f4a5b6';

async function createServer(page: Page, prefix: string, autoExec: boolean) {
  const name = `${prefix}-${Date.now()}`;
  const created = await page.request.post('/api/servers', {
    headers: { Origin: ORIGIN },
    data: {
      name,
      host: '127.0.0.1',
      port: 1,
      os: 'linux',
      auth_method: 'password',
      username: 'e2e',
      password: 'e2e-pass',
      auto_exec_enabled: autoExec,
    },
  });
  expect(created.status()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  return { id, name };
}

async function mockContainerPage(
  page: Page,
  serverId: string,
  serverName: string,
  autoExec: boolean,
) {
  await page.route('**/api/servers', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({
      json: {
        items: [
          {
            id: serverId,
            name: serverName,
            device_kind: 'server',
            os: 'linux',
            netdev_brand: '',
            access_proto: 'ssh',
            host: '127.0.0.1',
            port: 1,
            winrm_port: 5985,
            username: 'e2e',
            auth_method: 'password',
            tags: [],
            auto_exec_enabled: autoExec,
            has_password: true,
            has_private_key: false,
            has_enable_secret: false,
            container_runtime: 'docker',
            host_key_fingerprint: '',
            host_key_verified_at: null,
            created_at: '2026-10-27T08:00:00Z',
            updated_at: '2026-10-27T08:00:00Z',
          },
        ],
      },
    });
  });
  await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({
      json: {
        runtime: 'docker',
        items: [
          {
            id: CONTAINER_ID,
            name: 'p6s6-web',
            image: 'nginx:1.27',
            state: 'running',
            status: 'Up 2 hours',
            ports: [],
            created: '2026-11-08T06:00:00Z',
          },
        ],
      },
    });
  });
}

async function openStopDialog(page: Page) {
  await page.goto('/containers');
  const row = page.getByRole('row').filter({ hasText: 'p6s6-web' });
  await expect(row).toBeVisible({ timeout: 20_000 });
  await row.getByRole('button', { name: /停\s*止/ }).click();
  return page.getByRole('dialog');
}

async function mockSettings(page: Page, mode: 'all' | 'standard' | 'high' | 'none') {
  // 只 mock 读（GET /api/settings），不改真实全局门限——避免与并行 spec
  // 的审批用例互相干扰（settings 是全局键）。
  await page.route('**/api/settings', async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback();
      return;
    }
    await route.fulfill({ json: { approval_mode: mode } });
  });
}

test.describe('六期打磨 sprint 6：容器操作弹窗按门限渲染分流文案', () => {
  test('免审批档：confirm 文案与按钮明示「直接执行」，不再是「创建审批」', async ({ page }) => {
    const { id: serverId, name } = await createServer(page, 'p6s6-none', true);
    await mockContainerPage(page, serverId, name, true);
    await mockSettings(page, 'none');
    // 直执行分支：approval=null + execution（门限判定后同步执行）。
    await page.route(`**/api/servers/${serverId}/containers/*/action`, (route) =>
      route.fulfill({
        json: {
          approval: null,
          execution: { exit_code: 0, output: '', truncated: false, duration_ms: 3 },
        },
      }),
    );

    try {
      const dialog = await openStopDialog(page);
      await expect(
        dialog.getByText(
          '当前审批门限判定为免审批——点击后将立即执行该命令（不经人工批准），并记录审计。',
        ),
      ).toBeVisible({ timeout: 20_000 });
      await expect(dialog.getByText(/将创建一条/)).toHaveCount(0);
      await expect(dialog.getByText('将直接执行的命令')).toBeVisible();
      const body = dialog.locator('.ant-modal-body');
      await expect(body.getByRole('button', { name: '确认执行' })).toBeVisible();
      await expect(body.getByRole('button', { name: '创建审批' })).toHaveCount(0);

      // 点击后直接进入 result 态（无审批等待）。
      await body.getByRole('button', { name: '确认执行' }).click();
      await expect(dialog.getByText(/执行成功（exit 0/)).toBeVisible({ timeout: 20_000 });
      await expect(dialog.getByText('已执行命令')).toBeVisible();
    } finally {
      await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
    }
  });

  test('standard 档：confirm 文案仍为需人工批准的审批（回归）', async ({ page }) => {
    const { id: serverId, name } = await createServer(page, 'p6s6-std', false);
    await mockContainerPage(page, serverId, name, false);
    await mockSettings(page, 'standard');
    await page.route(`**/api/servers/${serverId}/containers/*/action`, (route) =>
      route.fulfill({
        json: {
          approval: {
            id: '9d8c7b6a-5e4f-3a2b-1c0d-9e8f7a6b5c4d',
            conversation_id: null,
            tool_call_id: null,
            source: 'console',
            server_id: serverId,
            server_name: name,
            command: `docker stop ${CONTAINER_ID}`,
            risk_level: 'needs_approval',
            rule_id: 'CONTAINER_CHANGE',
            reason: 'container state change',
            approval_mode: 'standard',
            status: 'pending',
            requested_at: new Date().toISOString(),
            expires_at: new Date(Date.now() + 120_000).toISOString(),
            decided_at: null,
            decided_by: null,
            note: '',
            edited_command: '',
            edited_risk_level: '',
            edited_rule_id: '',
          },
          execution: null,
        },
      }),
    );

    try {
      const dialog = await openStopDialog(page);
      await expect(dialog.getByText(/将创建一条/)).toBeVisible({ timeout: 20_000 });
      await expect(dialog.getByText('将申请执行的命令')).toBeVisible();
      const body = dialog.locator('.ant-modal-body');
      await expect(body.getByRole('button', { name: '创建审批' })).toBeVisible();
      await expect(body.getByRole('button', { name: '确认执行' })).toHaveCount(0);
    } finally {
      await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
    }
  });

  test('TTL 到期后弹窗显示「已过期」，不再出现「已超时」', async ({ page }) => {
    const { id: serverId, name } = await createServer(page, 'p6s6-exp', false);
    await mockContainerPage(page, serverId, name, false);
    await mockSettings(page, 'standard');
    // expires_at 设在过去：弹窗挂载后第一帧即倒计时归零。
    await page.route(`**/api/servers/${serverId}/containers/*/action`, (route) =>
      route.fulfill({
        json: {
          approval: {
            id: '1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d',
            conversation_id: null,
            tool_call_id: null,
            source: 'console',
            server_id: serverId,
            server_name: name,
            command: `docker stop ${CONTAINER_ID}`,
            risk_level: 'needs_approval',
            rule_id: 'CONTAINER_CHANGE',
            reason: 'container state change',
            approval_mode: 'standard',
            status: 'pending',
            requested_at: new Date(Date.now() - 120_000).toISOString(),
            expires_at: new Date(Date.now() - 1_000).toISOString(),
            decided_at: null,
            decided_by: null,
            note: '',
            edited_command: '',
            edited_risk_level: '',
            edited_rule_id: '',
          },
          execution: null,
        },
      }),
    );

    try {
      const dialog = await openStopDialog(page);
      const body = dialog.locator('.ant-modal-body');
      await body.getByRole('button', { name: '创建审批' }).click();
      await expect(dialog.getByText('已过期', { exact: true })).toBeVisible({ timeout: 20_000 });
      await expect(dialog.getByText('已超时')).toHaveCount(0);
      // 过期审批不得给可点假象（批准/拒绝禁用）。
      await expect(body.getByRole('button', { name: /批\s*准/ })).toBeDisabled();
      await expect(body.getByRole('button', { name: /拒\s*绝/ })).toBeDisabled();
    } finally {
      await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
    }
  });
});

// ---------------------------------------------------------------------------
// 3. 回放 assistant_text 步渲染 Markdown（代码块高亮 + 复制代码）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 6：回放正文与聊天页同源 Markdown', () => {
  test('回放 assistant_text 步的 ```bash 渲染成代码块（含复制代码），不再是字面量', async ({
    page,
  }) => {
    // 真链路产一条带代码围栏的回答（__scenario_code__），再 mock 回放。
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_CODE);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.locator('.opsmind-codeblock')).toHaveCount(1, { timeout: 30_000 });
    const cid = conversationIdOf(page);

    const assistantContent =
      '排查磁盘占满时可以按下面的顺序执行：\n\n' +
      '```bash\nsystemctl status nginx\njournalctl -u nginx -n 100 --no-pager\n```\n\n' +
      '先确认服务状态，再看最近的日志。';
    const step = {
      seq: 1,
      timestamp: '2026-11-08T10:00:00Z',
      kind: 'assistant_text',
      message_id: null,
      tool_call_id: null,
      approval_id: null,
      name: null,
      source: null,
      server_name: null,
      content: assistantContent,
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
    };
    await page.route(`**/api/conversations/${cid}/replay`, (route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({
          conversation_id: cid,
          steps: [step],
          truncated: false,
          truncated_reason: '',
        }),
      }),
    );

    // 八期 FE-R：回放已是独立页面，直接进入 `/replays/<id>`（Modal 下线）。
    await page.goto(`/replays/${cid}`);
    const replayStep = page.locator('.replay-step').first();
    await expect(replayStep).toHaveAttribute('data-kind', 'assistant_text');
    // Markdown 渲染：代码围栏变成代码块（此前是 <p> 里的 ```bash 字面量）。
    // 注意 `.opsmind-codeblock` 就是 <pre> 本身（CodeBlock 的 <pre
    // className="opsmind-codeblock">），不能再向下找 pre。
    const codeblock = replayStep.locator('pre.opsmind-codeblock');
    await expect(codeblock).toHaveCount(1, { timeout: 20_000 });
    await expect(codeblock).toContainText('systemctl status nginx');
    await expect(codeblock).toContainText('journalctl -u nginx -n 100 --no-pager');
    // 复制代码按钮与聊天页同源（唯一实现 CopyButton）。
    await expect(replayStep.getByLabel('复制代码')).toBeVisible();
    // 不再出现 Markdown 源码围栏字面量。
    await expect(replayStep.locator('.replay-step-content')).not.toContainText('```bash');
    expect(CODE_BLOCK_CONTENT).toContain('systemctl status nginx');
  });
});

// ---------------------------------------------------------------------------
// 4. 对话拒绝命令的审计行带 command / risk_level
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 6：命令拒绝审计可读', () => {
  test('对话拒绝后 command.denied detail 带 command 与 risk_level', async ({ page }) => {
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
        params: { action: 'command.denied', limit: 30 },
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
    expect(denied, 'command.denied 必须落审计').toBeDefined();
    expect(denied?.detail['command'], '拒绝行必须能看出拒的是哪条命令').toBe(
      'rm -rf /tmp/aiops-e2e',
    );
    expect(denied?.detail['risk_level']).toBe('needs_approval');
  });
});

// ---------------------------------------------------------------------------
// 7. 巡检报告抽屉平均评分按分档着色
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 6：巡检报告平均评分按分档着色', () => {
  // 七期（CONTRACT.md 8.3.9）：色值随主题取值，浅色 #c0272d / 深色 #f38ba8。
  // 用例在两种主题下各跑一次：先写入偏好（应用默认 light，仅 emulateMedia
  // 不会切换），再 emulateMedia 让 color-scheme 与主题一致。
  for (const theme of ['light', 'dark'] as const) {
    test(`30 分报告的 44px 大字为红色（此前恒绿）——${theme}`, async ({ page }) => {
      await page.addInitScript((mode) => {
        window.localStorage.setItem('opsmind.theme', mode);
      }, theme);
      await page.emulateMedia({ colorScheme: theme });
      // 报告列表与详情按契约允许 mock（真实链路的创建/执行由 Go 集成测试与
      // 五期 E2E 覆盖）；这里只验证「同一份数据两处着色同源」。
      const reportId = '6f2b1c9a-7c64-4e0f-9a1d-58e0f4a2b6c8';
      const nowIso = new Date().toISOString();
      const report = {
        id: reportId,
        job_id: null,
        job_name: '即时巡检',
        trigger: 'manual',
        server_count: 3,
        avg_score: 30,
        anomaly_count: 4,
        started_at: nowIso,
        finished_at: nowIso,
        created_at: nowIso,
      };
      await page.route('**/api/inspection/reports?*', (route) =>
        route.fulfill({ json: { items: [report] } }),
      );
      await page.route('**/api/inspection/reports', (route) =>
        route.fulfill({ json: { items: [report] } }),
      );
      await page.route(`**/api/inspection/reports/${reportId}`, (route) =>
        route.fulfill({ json: { report, items: [], previous_scores: null } }),
      );

      await page.goto('/inspections');
      await expect(page.getByRole('heading', { name: '巡检' })).toBeVisible();
      await page.getByRole('tab', { name: '报告' }).click();
      const row = page.locator('.ant-table-row').filter({ hasText: '即时巡检' }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      // 列表 Tag 是红（分档 0-59）。
      await expect(row.locator('.ant-tag').first()).toHaveText('30');
      // antd 对纯双字文本按钮自动插空格（accessible name「详 情」），正则兼容。
      await row.getByRole('button', { name: /详\s*情/ }).click();

      const drawer = page.getByRole('dialog');
      await expect(drawer.getByText('平均评分')).toBeVisible({ timeout: 20_000 });
      const bigScore = drawer.locator('h1').first();
      await expect(bigScore).toHaveText('30');
      // 当前主题必须已是目标主题，再读 --color-danger 解析出的 rgb()。
      expect(await page.evaluate(() => document.documentElement.dataset['theme'])).toBe(theme);
      const dangerRgb = await page.evaluate(() => {
        const probe = document.createElement('span');
        probe.style.color = 'var(--color-danger)';
        document.body.appendChild(probe);
        const value = getComputedStyle(probe).color;
        probe.remove();
        return value;
      });
      expect(dangerRgb, '--color-danger 必须解析为具体色值').toMatch(/^rgb\(/);
      await expect(bigScore).toHaveCSS('color', dangerRgb);
    });
  }
});

// ---------------------------------------------------------------------------
// 8. 审批过期审计逐条明细（approval_id / command）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 6：审批过期审计逐条可定位', () => {
  test('TTL 到期后 approval.expired 既有聚合行也有带 command 的逐条行', async ({ page }) => {
    // 复用 chat-coverage 的超时链路：45s TTL 后 sweeper 过期。
    test.setTimeout(150_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });

    // 等审批落库为 pending，记下 approval id（逐条审计应指向它）。
    let approvalId = '';
    for (let attempt = 0; attempt < 20 && approvalId === ''; attempt += 1) {
      const response = await page.request.get('/api/approvals', {
        params: { conversation_id: cid, limit: 10 },
      });
      const payload = (await response.json()) as { items: Array<{ id: string; status: string }> };
      const pending = payload.items.find((item) => item.status === 'pending');
      approvalId = pending?.id ?? '';
      if (approvalId === '') {
        await page.waitForTimeout(500);
      }
    }
    expect(approvalId, '审批必须已落库').not.toBe('');

    let perRow: { resource_id: string; detail: Record<string, unknown> } | undefined;
    let aggregate: { resource_id: string; detail: Record<string, unknown> } | undefined;
    for (let attempt = 0; attempt < 60 && perRow === undefined; attempt += 1) {
      const response = await page.request.get('/api/audit', {
        params: { action: 'approval.expired', limit: 50 },
      });
      expect(response.ok()).toBeTruthy();
      const body = (await response.json()) as {
        items: Array<{ resource_id: string; detail: Record<string, unknown> }>;
      };
      perRow = body.items.find((item) => item.resource_id === approvalId);
      aggregate = body.items.find((item) => item.resource_id === '');
      if (perRow === undefined) {
        await page.waitForTimeout(2_000);
      }
    }
    expect(perRow, '过期扫描必须逐条落审计（此前只有聚合行）').toBeDefined();
    expect(perRow?.detail['approval_id']).toBe(approvalId);
    expect(perRow?.detail['command']).toBe('rm -rf /tmp/aiops-e2e');
    expect(perRow?.detail['risk_level']).toBe('needs_approval');
    // 聚合行保留（旧口径 count）。
    expect(aggregate, '聚合行必须保留').toBeDefined();

    // 审计页「审批过期」筛选下该行目标与命令摘要可读。
    await page.goto('/audit');
    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    await actionSelect.click();
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('审批过期');
    await page.locator('.ant-select-item-option', { hasText: '审批过期' }).first().click();
    const row = page
      .locator('.ant-table-row', { hasText: '审批过期' })
      .filter({ hasText: 'rm -rf /tmp/aiops-e2e' })
      .first();
    await expect(row).toBeVisible({ timeout: 20_000 });
  });
});
