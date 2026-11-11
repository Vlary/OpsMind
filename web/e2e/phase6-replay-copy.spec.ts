import { expect, test, type Page } from '@playwright/test';
import {
  AUTH_STATE_FILE,
  RISKY_COMMAND,
  SCENARIO_LIST,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// FE-P2 六期端到端：执行回放播放器、门限四档切换、审批命令改写执行、
// 关键页复制按钮。除回放用 fake 数据（路由拦截，契约允许）外均走真实
// 链路（vite preview → 后端 → 假 LLM/Postgres）。

const ORIGIN = 'http://localhost:4173';

async function readClipboard(page: Page): Promise<string> {
  return page.evaluate(() => navigator.clipboard.readText());
}

// 六步回放假数据：覆盖全部 kind；approval 步带改写（original_command 非空）、
// execution 步带退出码/输出；truncated=true 验证顶部提示。
function fakeReplayBody(conversationId: string): string {
  const steps = [
    {
      seq: 1,
      timestamp: '2026-10-28T08:00:00Z',
      kind: 'user_message',
      message_id: '00000000-0000-0000-0000-000000000001',
      tool_call_id: null,
      approval_id: null,
      name: null,
      source: null,
      server_name: null,
      content: '帮我列出服务器并检查磁盘',
      command: null,
      original_command: null,
      arguments: null,
      risk_level: null,
      edited_risk_level: null,
      status: null,
      exit_code: null,
      output: null,
      error_code: null,
      duration_ms: null,
      reasoning_duration_ms: null,
    },
    {
      seq: 2,
      timestamp: '2026-10-28T08:00:01Z',
      kind: 'reasoning',
      message_id: '00000000-0000-0000-0000-000000000002',
      tool_call_id: null,
      approval_id: null,
      name: null,
      source: null,
      server_name: null,
      content: '用户要资产清单与磁盘水位，先 list_servers 再对首台执行 df -h。',
      command: null,
      original_command: null,
      arguments: null,
      risk_level: null,
      edited_risk_level: null,
      status: null,
      exit_code: null,
      output: null,
      error_code: null,
      duration_ms: null,
      // 六期打磨 sprint 1：回放 reasoning 步带服务端落库的思考耗时。
      reasoning_duration_ms: 1000,
    },
    {
      seq: 3,
      timestamp: '2026-10-28T08:00:02Z',
      kind: 'tool_call',
      message_id: null,
      tool_call_id: '00000000-0000-0000-0000-000000000003',
      approval_id: null,
      name: 'ssh_exec',
      source: null,
      server_name: null,
      content: null,
      command: null,
      original_command: null,
      arguments: { server_id: 'srv', command: 'rm -rf /tmp/aiops-e2e' },
      risk_level: 'needs_approval',
      edited_risk_level: null,
      status: 'succeeded',
      exit_code: null,
      output: null,
      error_code: null,
      duration_ms: null,
      reasoning_duration_ms: null,
    },
    {
      seq: 4,
      timestamp: '2026-10-28T08:00:03Z',
      kind: 'approval',
      message_id: null,
      tool_call_id: '00000000-0000-0000-0000-000000000003',
      approval_id: '00000000-0000-0000-0000-000000000004',
      name: null,
      source: 'chat',
      server_name: 'e2e-fake-host',
      content: null,
      command: 'df -h',
      original_command: RISKY_COMMAND,
      arguments: null,
      risk_level: 'needs_approval',
      edited_risk_level: 'safe',
      status: 'approved',
      exit_code: null,
      output: null,
      error_code: null,
      duration_ms: null,
      reasoning_duration_ms: null,
    },
    {
      seq: 5,
      timestamp: '2026-10-28T08:00:04Z',
      kind: 'execution',
      message_id: null,
      tool_call_id: '00000000-0000-0000-0000-000000000003',
      approval_id: null,
      name: 'ssh_exec',
      source: null,
      server_name: null,
      content: null,
      command: 'df -h',
      original_command: null,
      arguments: null,
      risk_level: null,
      edited_risk_level: null,
      status: 'succeeded',
      exit_code: 0,
      output: 'Filesystem      Size  Used Avail Use%\n/dev/vda1        40G   12G   26G  32%',
      error_code: null,
      duration_ms: 120,
      reasoning_duration_ms: null,
    },
    {
      seq: 6,
      timestamp: '2026-10-28T08:00:05Z',
      kind: 'assistant_text',
      message_id: '00000000-0000-0000-0000-000000000006',
      tool_call_id: null,
      approval_id: null,
      name: null,
      source: null,
      server_name: null,
      content: '资产清单核对完毕：共 1 台，磁盘使用率 32%。',
      command: null,
      original_command: null,
      arguments: null,
      risk_level: null,
      edited_risk_level: null,
      status: null,
      exit_code: null,
      output: null,
      error_code: null,
      duration_ms: null,
      reasoning_duration_ms: null,
    },
  ];
  // truncated_reason='steps'：步数上限截断（六期打磨 sprint 4 起后端区分原因）。
  return JSON.stringify({
    conversation_id: conversationId,
    steps,
    truncated: true,
    truncated_reason: 'steps',
  });
}

test.use({ storageState: AUTH_STATE_FILE });

test.describe('六期：执行回放播放器', () => {
  test('回放弹窗：kind 分色步骤、单步/自动播放/速度/进度条、truncated 提示', async ({ page }) => {
    // 真链路建一个已完成的会话（有真实 conversationId 可挂 fake 回放）。
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_LIST);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByText('资产清单核对完毕：当前共').first()).toBeVisible({
      timeout: 30_000,
    });
    const cid = conversationIdOf(page);

    // fake 回放数据（契约允许：fake 数据或真实链路）。后端未实现 replay
    // 端点时也能验证播放器行为；实现后同样被本路由覆盖，断言确定性不变。
    await page.route(`**/api/conversations/${cid}/replay`, (route) =>
      route.fulfill({
        contentType: 'application/json',
        body: fakeReplayBody(cid),
      }),
    );

    // 八期 FE-R（契约 2.12 约定 2）：对话页「回放执行」按钮改为 SPA 跳转到
    // `/replays/<id>`（不再开 Modal）。
    await page.getByRole('button', { name: '回放执行' }).click();
    await expect(page).toHaveURL(new RegExp(`/replays/${cid}$`));
    await expect(page.locator('.ant-modal')).toHaveCount(0);
    const steps = page.locator('.replay-steps');
    await expect(steps).toBeVisible();

    // truncated 提示（步数上限）。
    await expect(page.getByText('超出 20000 步上限，仅显示最近 6 步')).toBeVisible();

    // 初始：第 1 步（user_message）高亮。八期起时间轴是完整列表（虚拟滚动
    // 只裁 DOM 数量，不裁逻辑步数），不再只渲染「已播放步」。
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');
    await expect(page.locator('.replay-step--active')).toHaveAttribute('data-kind', 'user_message');
    await expect(page.locator('.replay-step')).toHaveCount(6);

    // 单步推进：reasoning 步复用 ReasoningBlock（默认收起摘要「深度思考」）。
    await page.getByRole('button', { name: '下一步' }).click();
    await expect(page.locator('.replay-counter')).toHaveText('第 2 / 6 步');
    await expect(page.locator('.replay-step').nth(1)).toHaveAttribute('data-kind', 'reasoning');
    // 六期打磨 sprint 1：回放 reasoning 步摘要带服务端耗时（此前恒「—”
    // 因为 ReplayStep 载荷缺 reasoning_duration_ms）。
    await expect(steps.locator('.chat-reasoning-toggle')).toContainText('深度思考 · 耗时 1s');

    // tool_call 步：工具名 + 状态 + 参数 JSON。
    await page.getByRole('button', { name: '下一步' }).click();
    await expect(page.locator('.replay-step').nth(2)).toHaveAttribute('data-kind', 'tool_call');
    await expect(steps.getByText('ssh_exec')).toBeVisible();
    await expect(steps.getByText('成功').first()).toBeVisible();

    // approval 步：批准状态 + 用户改写（原文删除线 + 改写命令）+ 级别。
    // 断言全部限定在当前高亮步内：reasoning 步（收起的 ReasoningBlock，
    // display:none）的文本里同样含 df -h，全局 getByText 会命中隐藏节点。
    await page.getByRole('button', { name: '下一步' }).click();
    const approvalStep = page.locator('.replay-step--active');
    await expect(approvalStep).toHaveAttribute('data-kind', 'approval');
    await expect(approvalStep.getByText('已批准')).toBeVisible();
    await expect(approvalStep.getByText('用户改写')).toBeVisible();
    await expect(approvalStep.locator('.replay-command-original')).toHaveText(RISKY_COMMAND);
    await expect(approvalStep.getByText('df -h').first()).toBeVisible();

    // execution 步：退出码/耗时/输出 + 输出复制（CodeBlock 统一复制按钮）。
    await page.getByRole('button', { name: '下一步' }).click();
    const executionStep = page.locator('.replay-step--active');
    await expect(executionStep).toHaveAttribute('data-kind', 'execution');
    await expect(executionStep.getByText('退出码 0')).toBeVisible();
    // 契约 2.11/8.7「execution 步折叠输出」：长输出默认收起，标题带「输出」
    // 与复制按钮；展开后才渲染 CodeBlock（此前直接平铺把整屏撑满）。
    const outputCollapse = executionStep.locator('.replay-output-collapse');
    await expect(outputCollapse).toBeVisible();
    await expect(outputCollapse.locator('.ant-collapse-item')).not.toHaveClass(
      /ant-collapse-item-active/,
    );
    await expect(outputCollapse.getByText('Filesystem')).toBeHidden();
    await expect(executionStep.getByLabel('复制输出')).toBeVisible();
    await outputCollapse.locator('.ant-collapse-header').click();
    await expect(outputCollapse.getByText('Filesystem').first()).toBeVisible();
    await expect(executionStep.getByLabel('复制代码').first()).toBeVisible();

    // 上一步可回退。
    await page.getByRole('button', { name: '上一步' }).click();
    await expect(page.locator('.replay-counter')).toHaveText('第 4 / 6 步');

    // 速度切 2x 后自动播放到末步自动暂停（终止条件）。
    // 显式等下拉真正展开再点选项（与其他 spec 相同口径：取最新非隐藏
    // dropdown，避开上一个下拉的离场动画）。
    await page.locator('.replay-controls .ant-select').click();
    const speedDropdown = page
      .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)')
      .last();
    await expect(speedDropdown).toBeVisible();
    await speedDropdown.locator('.ant-select-item-option', { hasText: '2x' }).click();
    await expect(page.locator('.replay-controls .ant-select')).toContainText('2x');
    await page.getByRole('button', { name: '播放' }).click();
    await expect(page.locator('.replay-counter')).toHaveText('第 6 / 6 步', { timeout: 10_000 });
    // 到末步自动暂停：播放按钮恢复可再按（从头重放语义）。
    await expect(page.getByRole('button', { name: '播放' })).toBeEnabled();
    // 末步为 assistant_text。
    await expect(page.locator('.replay-step--active')).toHaveAttribute(
      'data-kind',
      'assistant_text',
    );

    // 进度条拖回开头：聚焦滑块后按 Home 归零（antd Slider 手柄支持键盘）。
    const sliderHandle = page.locator('.replay-controls .ant-slider-handle');
    await sliderHandle.click();
    await page.keyboard.press('Home');
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');

    // 八期：没有弹窗可关——离开 `#` 之后步骤卡片不复存在（路由离开即卸载）。
    await expect(page.locator('.ant-modal')).toHaveCount(0);
  });

  test('审计页展开行「回放」链接打开同一播放器', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_LIST);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByText('资产清单核对完毕：当前共').first()).toBeVisible({
      timeout: 30_000,
    });
    const cid = conversationIdOf(page);

    await page.route(`**/api/conversations/${cid}/replay`, (route) =>
      route.fulfill({ contentType: 'application/json', body: fakeReplayBody(cid) }),
    );

    // 审计页按会话 ID 过滤出本轮对话的记录，展开第一行点「回放」。
    await page.goto('/audit');
    const table = page.locator('.ant-table');
    await expect(table).toBeVisible();
    const conversationFilter = page.getByPlaceholder('会话 ID（conversation UUID）');
    await conversationFilter.fill(cid);
    await conversationFilter.press('Enter');
    const row = page.locator('.ant-table-row').first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.locator('.ant-table-row-expand-icon').click();
    const expanded = page.locator('.ant-table-expanded-row');
    await expect(expanded).toBeVisible();
    // 展开态还有会话 ID 复制按钮（契约 8.7 ④）。
    await expect(expanded.getByLabel('复制会话 ID')).toBeVisible();
    // 八期 FE-R：审计行「回放」→ SPA 跳转 `/replays/<id>`（不再开 Modal）。
    await expanded.getByRole('button', { name: '回放' }).click();
    await expect(page).toHaveURL(new RegExp(`/replays/${cid}$`));
    await expect(page.locator('.ant-modal')).toHaveCount(0);
    await expect(page.locator('.replay-steps')).toBeVisible();
    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 6 步');
  });
});

test.describe('六期：门限四档切换（真实链路 /settings）', () => {
  test('四档循环切换并持久化，none 档显示警示，收尾恢复 standard', async ({ page }) => {
    await page.goto('/settings');
    const card = page.locator('.ant-card', { hasText: '审批门限' });
    await expect(card).toBeVisible();

    const select = card.locator('.ant-select').first();
    const switchTo = async (label: string) => {
      await select.click();
      await page.getByRole('option', { name: label, exact: true }).click();
      // 列表项标注「当前 · <档位>」表示保存成功并回显。
      await expect(card.getByText(`当前 · ${label}`)).toBeVisible({ timeout: 10_000 });
    };

    await switchTo('全部审批');
    await switchTo('仅高危审批');
    // none 档：不可跳过警示。
    await switchTo('免审批');
    await expect(
      card.getByText('所有命令将自动执行（含高危），仅剩审计留痕', { exact: false }),
    ).toBeVisible();
    // 恢复默认档（避免影响其他并行审批用例）。
    await switchTo('危险命令审批');
    await expect(
      card.getByText('所有命令将自动执行（含高危），仅剩审计留痕', { exact: false }),
    ).toHaveCount(0);

    // API 侧确认持久化为 standard。
    const response = await page.request.get('/api/settings');
    expect(response.ok()).toBeTruthy();
    const settings = (await response.json()) as { approval_mode?: string };
    expect(settings.approval_mode).toBe('standard');
  });
});

test.describe('六期：审批编辑命令批准后执行改写命令', () => {
  // 审批卡命令复制走统一 CopyButton（六期打磨 sprint 1：删除组件内手写
  // CommandCopyButton），剪贴板断言需要授权。
  test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

  test('改写为 safe 命令批准：审批与执行均按改写后命令落库', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\//, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await expect(approvalCard.getByText(RISKY_COMMAND)).toBeVisible();

    // pending 态「复制命令」（统一 CopyButton）：剪贴板 = 原命令。
    await approvalCard.getByLabel('复制命令').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(RISKY_COMMAND);

    // 编辑命令（契约 8.7 审批卡编辑）：改写为 safe 命令 uptime。
    await approvalCard.getByRole('button', { name: '编辑命令' }).click();
    const editor = page.getByLabel('改写命令');
    await expect(editor).toBeVisible();
    await editor.fill('uptime');
    // 实时风险预览给出改写后级别（safe）。
    await expect(approvalCard.getByText('改写后风险：')).toBeVisible({ timeout: 10_000 });
    await approvalCard.getByRole('button', { name: '保存并批准' }).click();

    // 审批按改写后命令批准：状态 + 「用户改写」标注。
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(approvalCard.getByText('用户改写').first()).toBeVisible();
    await expect(approvalCard.getByText('uptime').first()).toBeVisible();

    // 已决策的历史改写双行：「复制原命令」与「复制改写后命令」（统一
    // CopyButton，两级降级含可选中面板）分别取原文与改写文。
    await approvalCard.getByLabel('复制原命令').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(RISKY_COMMAND);
    await approvalCard.getByLabel('复制改写后命令').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('uptime');

    // 执行的是改写后命令（假服务器 SSH 不可达 → 执行失败，但不是被拒绝）。
    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    await expect(sshCard.getByText('SSH_EXEC_FAILED').first()).toBeVisible({ timeout: 30_000 });

    // API 侧：审批落库 edited_command=uptime 且已批准。
    const cid = conversationIdOf(page);
    const approvalsResponse = await page.request.get('/api/approvals', {
      params: { conversation_id: cid, limit: 10 },
    });
    expect(approvalsResponse.ok()).toBeTruthy();
    const approvals = (await approvalsResponse.json()) as {
      items: Array<{ command: string; edited_command: string; status: string }>;
    };
    const rewritten = approvals.items.find((item) => item.edited_command === 'uptime');
    expect(rewritten).toBeDefined();
    expect(rewritten?.status).toBe('approved');
    // 原 command 不动（审批创建时的历史事实）。
    expect(rewritten?.command).toBe(RISKY_COMMAND);

    // 审计：approval.approved 的 detail 带原文/改写双命令（契约 2.11 约定 3）。
    // 审计 writer 异步批量落库——轮询至 8s 兜底 flush 延迟；按 detail 的
    // original_command 定位本条（conversation_id 恒随 chat 审批落库，双保险）。
    let ours: { detail: Record<string, unknown>; conversation_id: string | null } | undefined;
    for (let attempt = 0; attempt < 16 && ours === undefined; attempt += 1) {
      const auditResponse = await page.request.get('/api/audit', {
        params: { action: 'approval.approved', limit: 20 },
      });
      expect(auditResponse.ok()).toBeTruthy();
      const audit = (await auditResponse.json()) as {
        items: Array<{ detail: Record<string, unknown>; conversation_id: string | null }>;
      };
      ours = audit.items.find(
        (item) => item.conversation_id === cid || item.detail['original_command'] === RISKY_COMMAND,
      );
      if (ours === undefined) {
        await page.waitForTimeout(500);
      }
    }
    expect(ours).toBeDefined();
    expect(ours?.detail['original_command']).toBe(RISKY_COMMAND);
    expect(ours?.detail['edited_command']).toBe('uptime');
  });
});

test.describe('六期：关键页复制按钮', () => {
  test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

  test('服务器/数据库/模型页与审计展开行的 CopyButton 存在且写入剪贴板', async ({ page }) => {
    // 服务器页：ID 与地址组合串复制（seed 的 e2e-fake-host）。
    const serversResponse = await page.request.get('/api/servers');
    expect(serversResponse.ok()).toBeTruthy();
    const servers = (await serversResponse.json()) as {
      items: Array<{ id: string; host: string; port: number }>;
    };
    const seeded = servers.items.find((item) => item.host === '127.0.0.1' && item.port === 1);
    expect(seeded).toBeDefined();

    await page.goto('/servers');
    const serverRow = page.locator('.ant-table-row', { hasText: 'e2e-fake-host' }).first();
    await expect(serverRow).toBeVisible({ timeout: 20_000 });
    await serverRow.getByLabel('复制服务器 ID').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(seeded?.id);

    await serverRow.getByLabel('复制地址').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('127.0.0.1:1');

    // 数据库页：预置一个库，连接信息组合串复制。
    const dbResponse = await page.request.post('/api/databases', {
      headers: { Origin: ORIGIN },
      data: {
        name: 'e2e-copy-db',
        engine: 'postgres',
        host: 'db-copy.example',
        port: 5432,
        database_name: 'e2ecopy',
        username: 'e2e',
        password: 'e2e-pass',
      },
    });
    expect(dbResponse.status()).toBe(201);

    await page.goto('/databases');
    const dbRow = page.locator('.ant-table-row', { hasText: 'e2e-copy-db' }).first();
    await expect(dbRow).toBeVisible({ timeout: 20_000 });
    await dbRow.getByLabel('复制连接信息').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('postgres://db-copy.example:5432/e2ecopy');
    await expect(dbRow.getByLabel('复制数据库 ID')).toBeVisible();

    // 模型页：Base URL 与模型名复制。
    await page.goto('/providers');
    const providerRow = page.locator('.ant-table-row', { hasText: 'e2e-fake-llm' }).first();
    await expect(providerRow).toBeVisible({ timeout: 20_000 });
    await providerRow.getByLabel('复制 Base URL').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toContain('http://127.0.0.1');
    await expect(providerRow.getByLabel('复制模型名')).toBeVisible();

    // 审计页：展开行 detail ID 复制（任意记录的原始 detail CodeBlock 也带复制）。
    await page.goto('/audit');
    const auditTable = page.locator('.ant-table');
    await expect(auditTable).toBeVisible();
    const auditRow = page.locator('.ant-table-row').first();
    await auditRow.locator('.ant-table-row-expand-icon').click();
    const expandedDetail = page.locator('.ant-table-expanded-row');
    await expect(expandedDetail).toBeVisible();
    await expect(expandedDetail.getByLabel('复制代码').first()).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 六期打磨 sprint 1（走查发现）：复制清单 4 处未落位——服务器/数据库的
// 「详情或表单只读态」、模型页「模型名下拉选项与详情」、终端「当前输入行」。
// 本组断言全部走真实链路的编辑弹窗与会话头部（不用 fake 数据）。
// ---------------------------------------------------------------------------
test.describe('六期打磨：复制清单补位', () => {
  test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

  test('服务器编辑弹窗与数据库编辑弹窗的只读态（ID / host:port / 连接串）可复制', async ({
    page,
  }) => {
    // 服务器：编辑 seed 的 e2e-fake-host，弹窗顶部出现 ID 与 host:port 复制。
    await page.goto('/servers');
    const row = page.locator('.ant-table-row', { hasText: 'e2e-fake-host' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /编\s*辑/ }).click();
    const modal = page.getByRole('dialog');
    await expect(modal.getByLabel('复制服务器 ID')).toBeVisible();
    await expect(modal.getByLabel('复制地址')).toBeVisible();
    await modal.getByLabel('复制地址').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('127.0.0.1:1');
    await modal.getByRole('button', { name: /取\s*消/ }).click();

    // 数据库：新建一个库后打开编辑弹窗，只读态出现 ID 与连接信息复制。
    const created = await page.request.post('/api/databases', {
      headers: { Origin: ORIGIN },
      data: {
        name: 'e2e-form-copy-db',
        engine: 'postgres',
        host: 'db-form.example',
        port: 5432,
        database_name: 'e2eform',
        username: 'e2e',
        password: 'e2e-pass',
      },
    });
    expect(created.status()).toBe(201);
    const createdBody = (await created.json()) as { id: string };

    await page.goto('/databases');
    // 列表 pageSize=10：全量并行时库数量超过一页，新建的库可能落在第二页
    // ——先用搜索框过滤到本用例的记录（与服务器页同款做法）。
    await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill('e2e-form-copy-db');
    const dbRow = page.locator('.ant-table-row', { hasText: 'e2e-form-copy-db' }).first();
    await expect(dbRow).toBeVisible({ timeout: 20_000 });
    await dbRow.getByRole('button', { name: /编\s*辑/ }).click();
    const dbModal = page.getByRole('dialog');
    await expect(dbModal.getByLabel('复制数据库 ID')).toBeVisible();
    await dbModal.getByLabel('复制数据库 ID').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(createdBody.id);
    await dbModal.getByLabel('复制连接信息').click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('postgres://db-form.example:5432/e2eform');
    await dbModal.getByRole('button', { name: /取\s*消/ }).click();
  });

  test('模型页：下拉选项与当前值都有模型名复制', async ({ page }) => {
    await page.goto('/providers');
    const row = page.locator('.ant-table-row', { hasText: 'e2e-fake-llm' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: /编\s*辑/ }).click();
    const modal = page.getByRole('dialog');
    // 回显详情：当前默认模型名可复制。
    await expect(modal.getByLabel('复制当前模型名')).toBeVisible();
    // 下拉选项里的模型名也带复制。定位「默认模型」输入框：antd 的 Form.Item
    // label 渲染为 div（getByLabel 不命中）、有值时 placeholder 不可见；
    // 弹窗内 combobox 依次为「预设」「类型」「默认模型」，取最后一个。
    await modal.getByRole('combobox').last().click();
    const dropdown = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)').last();
    await expect(dropdown).toBeVisible();
    await expect(dropdown.getByLabel('复制模型名').first()).toBeVisible();
    await page.keyboard.press('Escape');
  });

  test('终端页：当前输入行复制按钮在真实会话里常显并可写入剪贴板', async ({ page }) => {
    // 真链路：指向假 sshd（18083）建服务器并开会话。
    const name = `e2e-term-copy-${Date.now()}`;
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

    await page.goto('/term');
    await page.getByRole('combobox', { name: '选择服务器' }).click();
    const option = page.locator('.ant-select-item-option', { hasText: name }).first();
    for (let i = 0; i < 60 && !(await option.isVisible()); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await option.click();
    await page.getByRole('button', { name: /打开终端/ }).click();

    // 通道建立（假 sshd banner 上屏）后，头部复制按钮必须在（此前仅
    // !ended 分支渲染，实机 ended 分支下整个消失）。
    await expect(
      page.locator('[data-testid="term-viewport"]').getByText(/fake-sshd ready/),
    ).toBeVisible({ timeout: 20_000 });
    const copy = page.getByLabel('复制当前命令');
    await expect(copy).toBeVisible();
    // 复制取 xterm 当前光标行（cursorLineText：去尾部填充空格后 trim）。
    // 假 sshd 不回显输入（socket 只排空 stdin），光标行停在 banner 末尾的
    // 提示符 `$ `——断言剪贴板恰为该行，证明按钮读到的是真实 buffer 内容。
    await copy.click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe('$');
  });
});
