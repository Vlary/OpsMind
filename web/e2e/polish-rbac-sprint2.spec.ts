import { expect, test } from './fixtures';
import { OPERATOR_STATE_FILE, REVIEWER_STATE_FILE } from './helpers';

// RBAC 走查 sprint 2 端到端验证：
// 1) 审批者能从审批卡打开「查看上下文」抽屉，看到审批前消息与工具参数；
//    上下文只读接口只在点击后请求（不在队列轮询里放大跨用户审计）。
// 2) 创建者自兑换 invite token 时，邀请页显示中文可操作文案与下一步提示，
//    不再直出后端英文原文。
// 3) 受邀者设置密码成功后落到登录页，明确提示「密码已设置，请用新密码
//    登录」，且按钮文案不再承诺「并登录」。

const CONVERSATION_ID = '11111111-1111-4111-8111-111111111111';

// 后端 Approval 的真实字段（api/openapi.yaml Approval）。
const approval = {
  id: 'ap-sprint2',
  conversation_id: CONVERSATION_ID,
  tool_call_id: 'tc-sprint2',
  source: 'chat',
  server_id: '22222222-2222-4222-8222-222222222222',
  server_name: 'web-1',
  command: 'rm -rf /tmp/aiops-e2e',
  risk_level: 'needs_approval',
  rule_id: 'RM_RM_RF',
  reason: '删除操作影响文件系统',
  status: 'pending',
  requested_at: '2026-01-01T00:00:00Z',
  expires_at: '2099-01-01T00:02:00Z',
  decided_at: null,
  decided_by: null,
  note: '',
  approval_mode: 'standard',
  edited_command: '',
  edited_risk_level: '',
  edited_rule_id: '',
  requested_by: '33333333-3333-4333-8333-333333333333',
  requested_by_name: 'operator',
  break_glass: false,
  legacy: false,
};

const approvalContext = {
  approval,
  tool_call: {
    id: 'tc-sprint2',
    seq: 7,
    conversation_id: CONVERSATION_ID,
    turn_id: '44444444-4444-4444-8444-444444444444',
    provider_call_id: 'call_sprint2',
    name: 'ssh_exec',
    arguments: {
      server_id: '22222222-2222-4222-8222-222222222222',
      command: 'rm -rf /tmp/aiops-e2e',
    },
    result: null,
    risk_level: 'needs_approval',
    status: 'awaiting_approval',
    error_code: '',
    error_message: '',
    approval_mode: 'standard',
    started_at: '2026-01-01T00:00:00Z',
    finished_at: null,
  },
  server: {
    id: '22222222-2222-4222-8222-222222222222',
    name: 'web-1',
    device_kind: 'server',
    os: 'linux',
    netdev_brand: '',
    access_proto: 'ssh',
    host: '10.0.0.8',
    port: 22,
    winrm_port: 5985,
    username: 'root',
    auth_method: 'password',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: '',
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  },
  messages: [
    {
      id: '55555555-5555-4555-8555-555555555555',
      seq: 5,
      conversation_id: CONVERSATION_ID,
      turn_id: '44444444-4444-4444-8444-444444444444',
      role: 'user',
      content: '请清理 /tmp/aiops-e2e 目录',
      reasoning: '',
      reasoning_duration_ms: null,
      status: 'complete',
      error_code: '',
      model: 'fake-model',
      prompt_tokens: 0,
      completion_tokens: 0,
      created_at: '2026-01-01T00:00:00Z',
    },
    {
      id: '66666666-6666-4666-8666-666666666666',
      seq: 6,
      conversation_id: CONVERSATION_ID,
      turn_id: '44444444-4444-4444-8444-444444444444',
      role: 'assistant',
      content: '准备执行删除命令，请审批。',
      reasoning: '',
      reasoning_duration_ms: null,
      status: 'complete',
      error_code: '',
      model: 'fake-model',
      prompt_tokens: 0,
      completion_tokens: 0,
      created_at: '2026-01-01T00:00:00Z',
    },
  ],
};

test.describe('RBAC 走查 sprint 2：审批上下文', () => {
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('审批卡可打开上下文抽屉，展示审批前消息与工具参数', async ({ page }) => {
    let contextRequests = 0;
    await page.route(/\/api\/approvals\?/, async (route) => {
      await route.fulfill({ json: { items: [approval] } });
    });
    await page.route(/\/api\/approvals\/ap-sprint2\/context$/, async (route) => {
      contextRequests += 1;
      await route.fulfill({ json: approvalContext });
    });

    await page.goto('/approvals');
    const card = page.locator('.chat-approval', { hasText: 'rm -rf /tmp/aiops-e2e' });
    await expect(card).toBeVisible({ timeout: 20_000 });
    // 未打开抽屉前不请求上下文（跨用户读取会写审计，不能靠轮询放大）。
    expect(contextRequests).toBe(0);

    await card.getByRole('button', { name: '查看上下文' }).click();
    const drawer = page.getByRole('dialog');
    await expect(drawer.getByText('审批上下文')).toBeVisible();
    await expect(drawer.getByText('审批创建前的上下文')).toBeVisible();
    await expect(drawer.getByText('审批前消息（2 条）')).toBeVisible();
    await expect(drawer.getByText('请清理 /tmp/aiops-e2e 目录')).toBeVisible();
    await expect(drawer.getByText('准备执行删除命令，请审批。')).toBeVisible();
    // 工具参数：工具名 + 可复制的 JSON（含实际命令）。
    await expect(drawer.getByText('ssh_exec')).toBeVisible();
    await expect(drawer.getByText('"command": "rm -rf /tmp/aiops-e2e"')).toBeVisible();
    await expect(drawer.getByText('web-1（10.0.0.8）')).toBeVisible();
    expect(contextRequests).toBe(1);

    await drawer.locator('.ant-drawer-close').click();
    await expect(drawer).toBeHidden();
  });

  test('无 approvals:read 的会话不渲染上下文入口（服务端仍为唯一授权点）', async ({ browser }) => {
    const context = await browser.newContext({ storageState: OPERATOR_STATE_FILE });
    const page = await context.newPage();
    await page.route(/\/api\/approvals\?/, async (route) => {
      await route.fulfill({ json: { items: [approval] } });
    });
    // 运维工程师无权访问 /approvals：PermissionGate 渲染 403 页（URL 不变），
    // 页面里不可能出现审批上下文入口。
    await page.goto('/approvals');
    await expect(page.getByText('无权访问').first()).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/没有访问「审批」的权限/)).toBeVisible();
    await expect(page.getByRole('button', { name: '查看上下文' })).toHaveCount(0);
    await context.close();
  });
});

test.describe('RBAC 走查 sprint 2：邀请兑换文案', () => {
  test('创建者自兑换：中文说明 + 下一步提示，不显示后端英文原文', async ({ page }) => {
    await page.route('**/api/auth/invite/accept', async (route) => {
      await route.fulfill({
        status: 403,
        contentType: 'application/json',
        body: JSON.stringify({
          error: {
            code: 'SELF_REDEEM_FORBIDDEN',
            message: 'token cannot be redeemed by its creator',
            request_id: 'req-self-redeem',
          },
        }),
      });
    });

    await page.goto('/invite?token=self-redeem-token&kind=invite');
    await page.getByLabel('新密码', { exact: true }).fill('N3w-passphrase');
    await page.getByLabel('确认新密码', { exact: true }).fill('N3w-passphrase');
    await page.getByRole('button', { name: '设置密码' }).click();

    await expect(
      page.getByText('邀请链接不能由创建者本人兑换，请让受邀者在自己的设备或网络打开'),
    ).toBeVisible();
    await expect(page.getByText('下一步')).toBeVisible();
    await expect(page.getByText(/请把链接转发给受邀者/)).toBeVisible();
    await expect(page.getByText('token cannot be redeemed by its creator')).toHaveCount(0);
    await expect(page.getByText('请求失败')).toHaveCount(0);
  });

  test('受邀者设置密码成功：落到登录页并提示用新密码登录', async ({ page }) => {
    await page.route('**/api/auth/invite/accept', async (route) => {
      await route.fulfill({ status: 204 });
    });

    await page.goto('/invite?token=invitee-token&kind=invite');
    // 按钮不再承诺「并登录」——兑换成功不会自动登录。
    await expect(page.getByRole('button', { name: '设置密码' })).toBeVisible();
    await expect(page.getByRole('button', { name: '接受邀请并登录' })).toHaveCount(0);
    await page.getByLabel('新密码', { exact: true }).fill('N3w-passphrase');
    await page.getByLabel('确认新密码', { exact: true }).fill('N3w-passphrase');
    await page.getByRole('button', { name: '设置密码' }).click();

    await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
    await expect(page.getByText('密码已设置，请用新密码登录')).toBeVisible();
    await expect(page.getByText('为安全起见不会自动登录；请使用刚设置的密码登录。')).toBeVisible();
    // 登录表单仍可用（不是被成功提示卡住的死页）。
    await expect(page.getByPlaceholder('请输入用户名')).toBeVisible();
  });
});
