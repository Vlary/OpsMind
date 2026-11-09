import { expect, test } from '@playwright/test';
import {
  AUTH_STATE_FILE,
  DEFAULT_FINAL,
  LIST_FINAL_PREFIX,
  LIST_FINAL_SUFFIX,
  RISKY_COMMAND,
  RISKY_FINAL,
  SCENARIO_LIST,
  SCENARIO_RISKY,
  conversationIdOf,
  installProgressSampler,
  readProgressSamples,
  sendMessage,
} from './helpers';

// 聊天主链路：空会话引导、SSE 流式渲染、工具调用、审批、持久化与会话管理。
// 除特别说明外均以 globalSetup 预置的会话 Cookie 直接进入（storageState）。

test.use({ storageState: AUTH_STATE_FILE });

test.describe('聊天与 SSE', () => {
  test('空会话引导：示例与输入框可用', async ({ page }) => {
    await page.goto('/chat');
    await expect(page.getByText('开始新的运维对话')).toBeVisible();
    await expect(page.getByText('列出所有已配置的服务器')).toBeVisible();
    const box = page.getByLabel('消息输入框');
    await expect(box).toBeVisible();
    await expect(box).toBeEnabled();
  });

  test('__scenario_list__：token 渐进上屏、工具卡片、刷新后消息持久化', async ({ page }) => {
    await page.goto('/chat');
    await installProgressSampler(page);
    await sendMessage(page, SCENARIO_LIST);

    // 发送后自动创建会话并跳转 /c/:id。
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // list_servers 工具卡片出现，随后拿到结果标记为成功。
    const listCard = page.locator('.chat-toolcall-card', { hasText: 'list_servers' }).first();
    await expect(listCard).toBeVisible();
    await expect(listCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

    // 最终回答完整上屏（live 流式或持久化消息渲染均可）。文案由假 LLM 按
    // list_servers 工具结果动态生成，断言稳定书挡 + globalSetup 播种的
    // e2e-fake-host（127.0.0.1:1）确实出现在回答里——回答与库中资产一致。
    await expect(page.getByText(LIST_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText(LIST_FINAL_SUFFIX)).toBeVisible();
    const assistantBubble = page.locator('.chat-message--assistant .opsmind-markdown').first();
    await expect(assistantBubble).toContainText('e2e-fake-host（127.0.0.1:1');

    // 渐进上屏：流式期间 assistant 气泡文本长度应出现过多个不同值，
    // 且最大长度等于最终全文长度（动态文案，长度从 DOM 读取）。
    const samples = await readProgressSamples(page);
    const finalText = (await assistantBubble.textContent()) ?? '';
    const finalLength = finalText.trim().length;
    expect(samples.max).toBe(finalLength);
    expect(samples.count).toBeGreaterThanOrEqual(2);

    // 刷新后消息与工具卡片从后端持久化恢复。
    await page.reload();
    await expect(page.getByText(`__scenario_list__`).first()).toBeVisible();
    await expect(page.getByText(LIST_FINAL_PREFIX)).toBeVisible();
    await expect(page.getByText(LIST_FINAL_SUFFIX)).toBeVisible();
    const persistedCard = page.locator('.chat-toolcall-card', { hasText: 'list_servers' }).first();
    await expect(persistedCard).toBeVisible();
    await expect(persistedCard.getByText('成功')).toBeVisible();
  });

  test('__scenario_risky__：审批卡片，批准后出现工具结果与最终回答', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 审批卡片：标题、风险命令、目标服务器。
    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await expect(approvalCard.getByText(RISKY_COMMAND)).toBeVisible();
    await expect(approvalCard.getByText('e2e-fake-host')).toBeVisible();
    await expect(approvalCard.getByText('待处理')).toBeVisible();

    // 批准。（带 icon 的按钮不会被 antd 插空格，名称即 “批准”。）
    await approvalCard.getByRole('button', { name: '批准' }).click();
    // exact：审批卡标题 Space（“命令审批需审批已批准”）会子串命中，必须精确匹配 Alert 标题。
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    // SSH 连不上假服务器属预期：工具卡片失败 + SSH_EXEC_FAILED。
    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    await expect(sshCard.getByText('SSH_EXEC_FAILED').first()).toBeVisible({ timeout: 30_000 });

    // 最终回答（第 3 次假 LLM 调用）。
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });

    // API 侧确认 ssh_exec 的执行结果为连接失败而非真实输出。
    const cid = conversationIdOf(page);
    const response = await page.request.get(`/api/conversations/${cid}/tool-calls`);
    expect(response.ok()).toBeTruthy();
    const payload = (await response.json()) as {
      items: Array<{
        name: string;
        status: string;
        error_code?: string;
        error_message?: string;
        result?: unknown;
      }>;
    };
    const sshCalls = payload.items.filter((item) => item.name === 'ssh_exec');
    expect(sshCalls).toHaveLength(1);
    expect(sshCalls[0].status).toBe('failed');
    expect(sshCalls[0].error_code).toBe('SSH_EXEC_FAILED');
    // 底层失败原因落库（sprint 8 打磨）：与 SSE tool_result.error.message 一致。
    expect(sshCalls[0].error_message).toContain('ssh command failed');
    // 连接失败：没有 exit_code/output 之类的执行结果。
    expect(sshCalls[0].result).toBeNull();

    // 刷新重进会话：历史工具卡显示落库的底层原因，不只显示错误码。
    await page.reload();
    const persistedCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    await expect(persistedCard.getByText('SSH_EXEC_FAILED').first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(persistedCard.getByText(/ssh command failed/).first()).toBeVisible();
  });

  test('__scenario_risky__ 拒绝路径：不产生执行结果', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await expect(approvalCard.getByText(RISKY_COMMAND)).toBeVisible();

    await approvalCard.getByRole('button', { name: '拒绝' }).click();
    // exact：避免子串命中标题 Space（“命令审批需审批已拒绝”）。
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });

    // 工具卡片标记拒绝并给出 APPROVAL_DENIED。
    const sshCard = page.locator('.chat-toolcall-card', { hasText: 'ssh_exec' }).first();
    await expect(sshCard.getByText('APPROVAL_DENIED').first()).toBeVisible({ timeout: 30_000 });
    // 命令从未执行：拒绝终态不展示耗时数字（走查 B3 打磨）。
    await expect(sshCard.getByText(/\d+ ms/)).toHaveCount(0);

    // 拒绝后模型收尾说明。
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });

    // API 侧：拒绝路径没有产生任何执行结果。
    const cid = conversationIdOf(page);
    const response = await page.request.get(`/api/conversations/${cid}/tool-calls`);
    expect(response.ok()).toBeTruthy();
    const payload = (await response.json()) as {
      items: Array<{ name: string; status: string; error_code?: string; result?: unknown }>;
    };
    const sshCalls = payload.items.filter((item) => item.name === 'ssh_exec');
    expect(sshCalls).toHaveLength(1);
    expect(sshCalls[0].status).toBe('denied');
    expect(sshCalls[0].error_code).toBe('APPROVAL_DENIED');
    expect(sshCalls[0].result).toBeNull();

    // 审计中不存在该命令的成功执行记录。
    const auditResponse = await page.request.get('/api/audit', {
      params: { resource_type: 'server', limit: 50 },
    });
    expect(auditResponse.ok()).toBeTruthy();
    const audit = (await auditResponse.json()) as { items: Array<{ action: string }> };
    const executed = audit.items.filter((item) => item.action === 'command.executed');
    expect(executed).toHaveLength(0);
  });

  test('会话切换与重命名', async ({ page }) => {
    await page.goto('/chat');

    // 会话 A：发送一条默认场景消息（回复完成后停留）。
    const titleA = '第一会话定位标记默认场景';
    await sendMessage(page, titleA);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cidA = conversationIdOf(page);
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible({ timeout: 30_000 });

    // 侧栏出现该会话（标题取自首条消息前 30 字符）。
    const itemA = page.locator('.chat-conv-item', { hasText: titleA }).first();
    await expect(itemA).toBeVisible();

    // 新建会话：进入 /chat 草稿页（首条消息发送时才落库），
    // 不再立即创建空「新会话」。
    await page.getByTitle('新建会话').click();
    await expect(page).toHaveURL(/\/chat$/, { timeout: 20_000 });
    await expect(page.getByText('开始新的运维对话')).toBeVisible();
    await expect(page.locator('.chat-conv-item', { hasText: '新会话' })).toHaveCount(0);

    // 切换回 A：URL 与历史消息恢复。
    await itemA.click();
    await expect(page).toHaveURL(new RegExp(`/c/${cidA}`));
    await expect(page.getByText(titleA).first()).toBeVisible();
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible();

    // 重命名 A。
    await itemA.hover();
    await itemA
      .getByRole('button')
      .filter({ has: page.locator('.anticon-more') })
      .click();
    await page.getByRole('menuitem', { name: '重命名' }).click();
    const dialog = page.getByRole('dialog', { name: '重命名会话' });
    await dialog.getByRole('textbox').fill('端到端重命名后的会话');
    // Modal 确认按钮是纯双字文本（“保 存”），antd 自动插空格，用正则兼容。
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeHidden();

    const renamed = page.locator('.chat-conv-item', { hasText: '端到端重命名后的会话' }).first();
    await expect(renamed).toBeVisible();
    await expect(page.locator('.chat-topbar-title')).toHaveText('端到端重命名后的会话');
  });
});

test('流式中经确认弹窗切换会话后，离开确认不失效（leaveInProgressRef 复位）', async ({ page }) => {
  // 回归：/c/a 与 /c/b 命中同一路由不 remount，performLeave 置的
  // leaveInProgressRef=true 若不在到达新会话后复位，此后流式进行中的
  // 离开确认弹窗永久失效（CONTRACT 8.6.8）。
  // 用审批等待期做“流式持续中”的稳定窗口：审批未决时 SSE 一直挂着。
  await page.goto('/chat');

  // 会话 A：进入审批等待（流式进行中）。
  await sendMessage(page, SCENARIO_RISKY);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const cidA = conversationIdOf(page);
  const approvalA = page.locator('.chat-approval', { hasText: '命令审批' }).first();
  await expect(approvalA).toBeVisible({ timeout: 30_000 });

  // 流式中点「新建会话」：必须先弹确认，确认后切换（ref 置 true）。
  await page.getByTitle('新建会话').click();
  const confirm = page.getByRole('dialog', { name: '当前回答仍在进行' });
  await expect(confirm).toBeVisible();
  await confirm.getByRole('button', { name: '停止并切换' }).click();
  await expect(page).toHaveURL(/\/chat|\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  // 会话 B：再次进入审批等待（流式进行中）。
  await sendMessage(page, SCENARIO_RISKY);
  await expect(page.locator('.chat-approval', { hasText: '命令审批' }).first()).toBeVisible({
    timeout: 30_000,
  });

  // 流式中切回 A：修复前 ref 残留 true → useBlocker/requestLeave 不拦，
  // 直接放行；修复后到达 B 时已复位，必须再次弹出确认。
  // 按 data-conv-id 精确定位 A：A 与当前会话 B 同为 __scenario_risky__
  // 开头的标题，按标题前缀 + .first() 会命中 B 自身（当前会话不触发
  // 离开确认），弹窗断言必然失败。
  const itemA = page.locator(`.chat-conv-item[data-conv-id="${cidA}"]`);
  await itemA.click();
  await expect(
    page.getByRole('dialog', { name: '当前回答仍在进行' }),
    '跨会话切换一次后，流式离开确认仍必须生效',
  ).toBeVisible({ timeout: 10_000 });

  // 收尾：取消弹窗并停止 B 的流，避免审批悬挂影响其他用例。
  await page.getByRole('button', { name: '继续等待' }).click();
  await page
    .getByRole('button', { name: '停止' })
    .click()
    .catch(() => undefined);
});
