import { expect, test, type Page } from '@playwright/test';
import { AUTH_STATE_FILE, conversationIdOf, sendMessage, SCENARIO_LIST } from './helpers';

// 六期打磨 sprint 3 走查修复的端到端断言（真实后端 + 假 LLM + 假 sshd +
// Postgres；回放数据用路由伪造——与 sprint 2 同口径）。
//
// 覆盖 4 条用户可见缺陷：
//  1. 回放 approval 步的 expired 显示中性「已过期」，不再说「已超时」
//     （同会话对话卡对流断开取消显示「已取消」，两者矛盾）；
//  2. 回放 execution 步无 command（通道在写 result 前失败）时不渲染空白
//     命令块 + 禁用复制按钮，改显「命令未知（通道失败前未下发）」；
//  3. 审批门限下拉有可访问名（会话侧「会话审批门限」、全局侧「审批门限」）；
//  4. 批量详情待审批行有「编辑」入口，可改写命令并保存批准（真实链路，
//     假 sshd 执行改写后命令）。

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
  await page.getByRole('button', { name: '回放' }).click();
  await expect(page.locator('.replay-steps')).toBeVisible();
}

test.use({ storageState: AUTH_STATE_FILE });

// ---------------------------------------------------------------------------
// 1 + 2. 回放：审批取消态中性文案、无命令的失败执行步
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 3：回放审批取消态与通道失败步', () => {
  test('流断开取消的审批在回放里显示「已过期」而不是「已超时」，且不再自称超时', async ({
    page,
  }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000aa',
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_risky__',
        }),
        // 走查实测数据：approval status=expired, note=stream_cancelled
        // （对话卡显示「已取消」+ 回答流已中断）。
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:01Z',
          kind: 'approval',
          approval_id: '00000000-0000-0000-0000-0000000000a1',
          source: 'chat',
          server_name: 'wt-host',
          command: 'rm -rf /tmp/aiops-riskcol',
          risk_level: 'needs_approval',
          status: 'expired',
        }),
      ],
    });

    await page.getByRole('button', { name: '下一步' }).click();
    const approvalStep = page.locator('.replay-step').nth(1);
    await expect(approvalStep).toHaveAttribute('data-kind', 'approval');
    // 中性文案：expired 同时覆盖 TTL 超时与流断开取消。
    await expect(approvalStep.getByText('已过期', { exact: true })).toBeVisible();
    // 修复前是「已超时」，与同会话对话卡的「已取消」矛盾。
    await expect(approvalStep.getByText('已超时', { exact: true })).toHaveCount(0);
  });

  test('无 command 的失败执行步不渲染空命令块，改显「命令未知（通道失败前未下发）」', async ({
    page,
  }) => {
    await page.goto('/chat');
    await openReplayWith(page, {
      conversation_id: '00000000-0000-0000-0000-0000000000ab',
      truncated: false,
      steps: [
        replayStep({
          seq: 1,
          kind: 'user_message',
          message_id: '00000000-0000-0000-0000-000000000001',
          content: '__walk_risky__',
        }),
        // 后端 replayExecutionStep 的「无 result + status=failed」分支：
        // command 为 null、exit_code=-1、output 空串、带 error_code。
        replayStep({
          seq: 2,
          timestamp: '2026-10-30T10:00:02Z',
          kind: 'execution',
          tool_call_id: '00000000-0000-0000-0000-0000000000e1',
          name: 'ssh_exec',
          command: null,
          status: 'failed',
          exit_code: -1,
          output: '',
          error_code: 'SSH_EXEC_FAILED',
        }),
      ],
    });

    await page.getByRole('button', { name: '下一步' }).click();
    const execStep = page.locator('.replay-step').nth(1);
    await expect(execStep).toHaveAttribute('data-kind', 'execution');
    await expect(execStep.getByText('命令未知（通道失败前未下发）')).toBeVisible();
    // 不再有空白「命令」标签 + 空代码块 + 禁用复制按钮。
    await expect(execStep.getByText('命令', { exact: true })).toHaveCount(0);
    await expect(execStep.locator('.opsmind-codeblock')).toHaveCount(0);
    // 失败原因仍可见（error_code 中文）。
    await expect(execStep.getByText('远程命令执行失败。')).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 3. 门限下拉的可访问名
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 3：审批门限下拉可访问名', () => {
  test('会话页顶栏 combobox 名为「会话审批门限」', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_LIST);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByRole('combobox', { name: '会话审批门限' })).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/settings 的全局门限 combobox 名为「审批门限」', async ({ page }) => {
    await page.goto('/settings');
    await expect(page.getByRole('combobox', { name: '审批门限' }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});

// ---------------------------------------------------------------------------
// 4. 批量详情「编辑命令」真实链路
// ---------------------------------------------------------------------------
test.describe('六期打磨 sprint 3：批量详情可编辑命令并批准', () => {
  test('待审批行「编辑」→ 改写为 uptime → 保存并批准：执行改写后命令', async ({ page }) => {
    // 建一台指向假 sshd 的服务器（auto_exec=false → 批量逐台落审批）。
    const name = `e2e-batch-edit-${Date.now()}`;
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
    const serverId = ((await created.json()) as { id: string }).id;

    await page.goto('/batch');
    const commandBox = page.getByPlaceholder('df -h /');
    await expect(commandBox).toBeVisible();
    await commandBox.fill('uptime');
    await page.getByRole('combobox', { name: '目标服务器' }).click();
    const option = page.locator('.ant-select-item-option', { hasText: name }).first();
    for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await option.click();
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: '创建并分派' }).click();

    // 详情抽屉：待审批行 + 「编辑」按钮（六期打磨 sprint 3 新增入口）。
    const drawer = page.getByRole('dialog');
    await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
    await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
    await drawer.getByRole('button', { name: /编\s*辑/ }).first().click();

    // 编辑弹窗：预填原命令，改写为 whoami 后「保存并批准」。
    const modal = page.locator('.ant-modal').filter({ hasText: '编辑命令并批准' });
    await expect(modal).toBeVisible();
    const editor = modal.getByLabel('改写命令');
    await expect(editor).toHaveValue('uptime');
    await editor.fill('whoami');
    await expect(modal.getByText('改写后风险：')).toBeVisible({ timeout: 10_000 });
    await modal.getByRole('button', { name: /保存并批准/ }).click();

    // 审批已按改写后命令执行：假 sshd 拒绝 exec（夹具只服务终端 PTY），
    // 因此 result 是通道失败终态——关键事实是**下发的是 whoami**。
    await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });

    // 假 sshd 控制端：改写后的命令确实被送达通道（execs 是全局聚合——监控
    // 采集器也会对同一台服务器执行采集模板，因此只做正向断言）。
    const statusResponse = await page.request.get('http://127.0.0.1:18084/status');
    expect(statusResponse.ok()).toBeTruthy();
    const fakeSshd = (await statusResponse.json()) as { execs?: string[] };
    // exec 命令可能带 Windows PowerShell 包装前缀（linux 无包装，逐字节即
    // 改写后命令）；用包含断言兼容通道层包装差异。
    expect((fakeSshd.execs ?? []).some((item) => item.includes('whoami'))).toBe(true);

    // API 侧：approval.edited_command=whoami、批量结果落通道失败但规则/级别
    // 来自改写后命令的分类快照。
    const jobsResponse = await page.request.get('/api/batch/jobs', { params: { limit: 50 } });
    expect(jobsResponse.ok()).toBeTruthy();
    const jobs = (await jobsResponse.json()) as {
      items: Array<{ id: string; command: string; server_ids: string[] }>;
    };
    const job = jobs.items.find(
      (item) => item.command === 'uptime' && item.server_ids.includes(serverId),
    );
    expect(job).toBeDefined();
    const detailResponse = await page.request.get(`/api/batch/jobs/${job?.id ?? ''}`);
    expect(detailResponse.ok()).toBeTruthy();
    const detail = (await detailResponse.json()) as {
      results: Array<{
        server_id: string;
        status: string;
        risk_level: string;
        approval_id: string;
        output: string;
        error_code: string;
      }>;
    };
    const row = detail.results.find((item) => item.server_id === serverId);
    expect(row?.status).toBe('failed');
    // 通道失败（exec 被拒）——不是策略拒绝，也不是审批过期。
    expect(row?.error_code).toBe('SSH_EXEC_FAILED');
    // Gap1 回归：审批路径的 risk_level 来自分类快照（uptime 为 safe）。
    expect(row?.risk_level).toBe('safe');
    const approvalResponse = await page.request.get(
      `/api/approvals/${row?.approval_id ?? ''}`,
    );
    expect(approvalResponse.ok()).toBeTruthy();
    const approval = (await approvalResponse.json()) as {
      command: string;
      edited_command: string;
      status: string;
    };
    expect(approval.command).toBe('uptime');
    expect(approval.edited_command).toBe('whoami');
    expect(approval.status).toBe('approved');
  });
});
