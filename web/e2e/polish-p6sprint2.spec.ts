import { expect, test, type Page } from '@playwright/test';
import {
  AUTH_STATE_FILE,
  RISKY_COMMAND,
  SCENARIO_LIST,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 六期打磨 sprint 2 走查修复的端到端断言（真实后端 + 假 LLM/假 sshd/Postgres）。
//
// 覆盖 5 条用户可见缺陷：
//  1. 审批卡编辑态清空命令后「保存并批准」必须保持禁用，且绝不按原命令
//     批准执行（曾执行 `env`，泄漏 SSH_CONNECTION/PATH 等环境变量）；
//  2. 回放 tool_call 步的失败状态显示中文「失败」，并带 error_code 与
//     execution 步（失败原因可见）；
//  3. 回放 user_message 不展示 `<!--opsmind-context … -->` 内部上下文前缀；
//  4. 免审批档直执行的 tool_call 步在回放里显示「高风险 · 免审批执行」
//     而不是「需审批」；
//  5. 单步会话的进度条不出现 NaN（handle 带 aria-valuenow）。
//
// 回放侧数据用路由伪造（契约允许：fake 数据或真实链路）——真实链路拿不到
// 「1 步会话」「无 result 的失败调用」这类确定性数据。

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
// 1. 审批卡编辑态空命令（high）
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 2：审批编辑态空命令不得批准', () => {
  test('清空「改写命令」后「保存并批准」禁用，不产生任何 approve/执行', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\//, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await expect(approvalCard.getByText(RISKY_COMMAND)).toBeVisible();

    await approvalCard.getByRole('button', { name: '编辑命令' }).click();
    const editor = page.getByLabel('改写命令');
    await expect(editor).toBeVisible();
    // 走查复现步骤：清空命令（此前 disabled=false，点击后按原命令批准执行）。
    await editor.fill('');

    const saveButton = approvalCard.getByRole('button', { name: '保存并批准' });
    await expect(saveButton).toBeDisabled();
    // 内联提示给出正确出口：取消编辑（定位 Alert 而不是按钮文案——两者
    // 都含「取消编辑」，getByText 在 strict mode 下会命中两个元素）。
    await expect(approvalCard.getByText(/命令不能为空/)).toHaveText(
      '命令不能为空，如需放弃修改请点「取消编辑」。',
    );

    // 纯空白同样拦截（trim 后为空）。
    await editor.fill('   \t  ');
    await expect(saveButton).toBeDisabled();

    // 点一次（禁用态点击无效）：审批仍 pending、没有执行。
    await saveButton.click({ force: true }).catch(() => undefined);
    await page.waitForTimeout(500);
    const cid = conversationIdOf(page);
    const approvalsResponse = await page.request.get('/api/approvals', {
      params: { conversation_id: cid, limit: 10 },
    });
    expect(approvalsResponse.ok()).toBeTruthy();
    const approvals = (await approvalsResponse.json()) as {
      items: Array<{ command: string; edited_command: string; status: string }>;
    };
    const risky = approvals.items.find((item) => item.command === RISKY_COMMAND);
    expect(risky).toBeDefined();
    expect(risky?.status).toBe('pending');
    expect(risky?.edited_command).toBe('');
    // 原命令绝未被批准执行：回放/工具调用里不存在 ssh_exec 的成功或失败执行。
    const toolCallsResponse = await page.request.get(`/api/conversations/${cid}/tool-calls`, {
      params: { limit: 20 },
    });
    expect(toolCallsResponse.ok()).toBeTruthy();
    const toolCalls = (await toolCallsResponse.json()) as {
      items: Array<{ name: string; status: string; result: unknown }>;
    };
    const sshCalls = toolCalls.items.filter((item) => item.name === 'ssh_exec');
    expect(sshCalls.every((item) => item.status === 'awaiting_approval')).toBe(true);

    // 恢复非空命令后按钮重新可用（编辑态没有卡死）。
    await editor.fill('uptime');
    await expect(saveButton).toBeEnabled({ timeout: 10_000 });
    // 清理：拒绝这条审批，避免悬挂影响后续用例。
    await approvalCard.getByRole('button', { name: '取消编辑' }).click();
    await approvalCard.getByRole('button', { name: '拒绝' }).click();
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
  });
});

// ---------------------------------------------------------------------------
// 2/3/4. 回放载荷：失败中文化 + error_code/execution 步 + 上下文前缀剥离 +
//        免审批徽标
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 2：回放载荷修复', () => {
  const FAILED_CALL_ID = '00000000-0000-0000-0000-0000000000f1';

  function failedReplayBody(conversationId: string): unknown {
    return {
      conversation_id: conversationId,
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          // 真实回放此前原样展示这段内部上下文前缀。
          content:
            '<!--opsmind-context\nservers: walk-ssh (id=bf590bd6-0000-0000-0000-000000000000)\n-->\n__walk_uptime__',
        }),
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:01Z',
          kind: 'tool_call',
          tool_call_id: FAILED_CALL_ID,
          name: 'ssh_exec',
          arguments: { server_id: 'srv', command: 'uptime' },
          risk_level: 'needs_approval',
          status: 'failed',
          error_code: 'SSH_EXEC_FAILED',
          approval_mode: 'standard',
        }),
        replayStep({
          seq: 3,
          timestamp: '2026-10-30T10:00:02Z',
          kind: 'execution',
          tool_call_id: FAILED_CALL_ID,
          name: 'ssh_exec',
          status: 'failed',
          exit_code: -1,
          output: '',
          error_code: 'SSH_EXEC_FAILED',
        }),
      ],
    };
  }

  test('失败调用：中文「失败」+ error_code + execution 步；上下文前缀不展示', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, failedReplayBody('00000000-0000-0000-0000-0000000000aa'));

    // 第 1 步：user_message 只显示正文，不显示内部上下文。
    const userStep = page.locator('.replay-step').first();
    await expect(userStep).toHaveAttribute('data-kind', 'user_message');
    await expect(userStep.locator('.replay-step-content')).toHaveText('__walk_uptime__');
    // 控制条摘要行同口径（此前正文已剥离、摘要仍显示原始上下文前缀）。
    await expect(page.locator('.replay-summary')).toHaveText('用户 · __walk_uptime__');
    await expect(page.getByText('opsmind-context')).toHaveCount(0);
    await expect(page.getByText('walk-ssh')).toHaveCount(0);

    // 第 2 步：tool_call 失败中文化 + 错误 Tag（error_code tooltip）。
    await page.getByRole('button', { name: '下一步' }).click();
    const toolStep = page.locator('.replay-step').nth(1);
    await expect(toolStep).toHaveAttribute('data-kind', 'tool_call');
    await expect(toolStep.getByText('失败', { exact: true })).toBeVisible();
    await expect(toolStep.getByText('failed', { exact: true })).toHaveCount(0);
    // error_code 通过「错误」Tag 的 Tooltip 呈现（antd Tooltip 不写静态
    // title 属性，hover 后浮层出现）。
    await toolStep.getByText('错误').hover();
    await expect(page.getByRole('tooltip').getByText('SSH_EXEC_FAILED')).toBeVisible();

    // 第 3 步：execution 步存在（status=failed + exit_code=-1 + 错误原因）。
    await page.getByRole('button', { name: '下一步' }).click();
    const execStep = page.locator('.replay-step').nth(2);
    await expect(execStep).toHaveAttribute('data-kind', 'execution');
    await expect(execStep.getByText('失败', { exact: true })).toBeVisible();
    await expect(execStep.getByText('退出码 -1')).toBeVisible();
    // 失败原因走 describeError 的中文映射（SSH_EXEC_FAILED → 远程命令执行失败。）。
    await expect(execStep.getByText('远程命令执行失败。')).toBeVisible();
  });

  test('免审批档直执行：回放显示「高风险 · 免审批执行」而非「需审批」', async ({ page }) => {
    const cid = '00000000-0000-0000-0000-0000000000bb';
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: cid,
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_uptime__',
        }),
        // none 档 + needs_approval + 无 approval 步 → 免审批直执行。
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:01Z',
          kind: 'tool_call',
          tool_call_id: '00000000-0000-0000-0000-0000000000e1',
          name: 'ssh_exec',
          arguments: { server_id: 'srv', command: 'uptime' },
          risk_level: 'needs_approval',
          status: 'succeeded',
          approval_mode: 'none',
        }),
      ],
    });

    await page.getByRole('button', { name: '下一步' }).click();
    const toolStep = page.locator('.replay-step').nth(1);
    await expect(toolStep).toHaveAttribute('data-kind', 'tool_call');
    await expect(toolStep.getByText('高风险 · 免审批执行')).toBeVisible();
    await expect(toolStep.getByText('需审批', { exact: true })).toHaveCount(0);
    await expect(toolStep.getByText('成功', { exact: true })).toBeVisible();
  });

  test('单步会话：进度条 handle 带 aria-valuenow（不再 NaN）', async ({ page }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000cc',
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '只有一步',
        }),
      ],
    });

    await expect(page.locator('.replay-counter')).toHaveText('第 1 / 1 步');
    const handle = page.locator('.replay-controls .ant-slider-handle');
    await expect(handle).toBeVisible();
    const valueNow = await handle.getAttribute('aria-valuenow');
    expect(valueNow).not.toBeNull();
    expect(valueNow).not.toBe('NaN');
    expect(Number.isNaN(Number(valueNow))).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// 5. 统一 CopyButton：消息气泡与容器日志抽屉
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 2：复制收敛统一 CopyButton', () => {
  test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

  test('消息气泡复制按钮是统一组件（aria-label + 1.5s 冷却）', async ({ page }) => {
    await page.goto('/chat');
    const marker = '统一复制按钮验证标记';
    await sendMessage(page, `${SCENARIO_LIST} ${marker}`);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.locator('.chat-message--assistant .opsmind-markdown').first()).toBeVisible({
      timeout: 30_000,
    });

    const userBubble = page.locator('.chat-message--user', { hasText: marker }).first();
    await userBubble.hover();
    // 统一 CopyButton 的两项结构特征：aria-label 文案 + .opsmind-copy-button 包裹。
    const copy = userBubble.getByLabel('复制消息内容');
    await expect(copy).toBeVisible();
    await expect(userBubble.locator('.opsmind-copy-button').first()).toBeVisible();
    await copy.click();
    await expect(page.getByText('已复制').first()).toBeVisible();
  });

  test('容器日志抽屉的复制是统一 CopyButton（可降级、带 aria-label）', async ({ page }) => {
    // 真链路建服务器（SSH 指向不可达端口），列表/日志用路由 mock。
    const created = await page.request.post('/api/servers', {
      headers: { Origin: ORIGIN },
      data: {
        name: `e2e-copybtn-${Date.now()}`,
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
    const server = (await created.json()) as { id: string; name: string };

    await page.route(`**/api/servers/${server.id}/containers`, (route) =>
      route.fulfill({
        json: {
          server_id: server.id,
          runtime: 'docker',
          items: [
            {
              id: 'abcdef0123456789',
              name: 'web-1',
              image: 'nginx:1.27',
              state: 'running',
              status: 'Up 2 hours (healthy)',
              ports: ['0.0.0.0:8080->80/tcp'],
              created: '2026-10-30T09:00:00Z',
            },
          ],
        },
      }),
    );
    await page.route(`**/api/servers/${server.id}/containers/*/logs*`, (route) =>
      route.fulfill({
        json: {
          server_id: server.id,
          container_id: 'abcdef0123456789',
          tail: 200,
          output: 'line-1\nline-2\nline-3',
          truncated: false,
        },
      }),
    );

    await page.goto('/containers');
    await page.getByRole('combobox', { name: '选择服务器' }).click();
    const option = page.locator('.ant-select-item-option', { hasText: server.name }).first();
    for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await option.click();
    await expect(page.getByText('web-1').first()).toBeVisible({ timeout: 20_000 });
    await page.getByRole('button', { name: '日志' }).first().click();
    const drawer = page.getByRole('dialog');
    await expect(drawer.getByText('容器日志 · web-1')).toBeVisible();

    const copy = drawer.getByRole('button', { name: '复制日志' });
    await expect(copy).toBeVisible();
    await expect(drawer.locator('.opsmind-copy-button').first()).toBeVisible();
    await copy.click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    // 复制内容确为完整日志。
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
      'line-1\nline-2\nline-3',
    );
  });
});
