import { execFile } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import {
  expect,
  request as playwrightRequest,
  test,
  type APIResponse,
  type Browser,
  type Page,
} from '@playwright/test';

import {
  ADMIN_PASSWORD,
  AUDITOR_STATE_FILE,
  AUTH_STATE_FILE,
  OPERATOR_STATE_FILE,
  REVIEWER_STATE_FILE,
  RISKY_COMMAND,
  SCENARIO_RISKY,
  conversationIdOf,
  sendMessage,
} from './helpers';

// RBAC 四角色真实浏览器端到端（docs/CONTRACT.md 12.1 角色 / 12.2 权限矩阵 /
// 12.3 不变量）。与 polish-rbac-sprint{1..4} 互补：那几支按「走查问题」逐点
// 断言，本文件按「角色 × 页面 × 动作」做矩阵化覆盖。
//
// 账号来源（重要）：POST /api/users 在未配置 SMTP 时按设计返回 503
// DELIVERY_UNAVAILABLE（cmd/aiops/main.go:367-370 的告警 + users handler 的
// CanDeliver 检查），e2e 环境没有投递通道，无法用该接口创建可登录账号。
// 因此：
//   - admin / operator / reviewer(approver) / auditor 复用 global-setup.ts
//     seedRoleUser() 直接 SQL 播种的账号（同一 bcrypt hash）与 sessions 行；
//   - 本文件自己的临时账号（会话失效用例）同样直连 psql 插入 users+sessions，
//     复用 admin 的 password_hash，token 规则与 internal/auth/token.go 一致；
//   - 「POST /api/users 返回 503」本身有一条断言，避免这个设计前提被悄悄改掉。
//
// 导航断言用真实点击器：登录表单 + 左侧 antd Menu（NAV_ITEMS 经
// lib/rbac.ts:filterNavItems 过滤后的渲染结果），不是对纯函数下结论。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const E2E_ORIGIN = 'http://localhost:4173';
// global-setup.ts:43 的 E2E_DATABASE；psql 直连仅用于「无法经 API 创建账号」
// 这一明确前提（见文件头）。
const E2E_DATABASE = 'aiops_e2e';

// psql 输出上限 1MB：这些查询只取单行 id/状态，任何意外放大都会立刻暴露，
// 而不是把整库内容读进测试进程（防 OOM）。
const PSQL_MAX_BUFFER = 1024 * 1024;

async function psql(sql: string): Promise<string> {
  const { stdout } = await execFileAsync(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      'db',
      'psql',
      '-U',
      'aiops',
      '-d',
      E2E_DATABASE,
      '-v',
      'ON_ERROR_STOP=1',
      '-A',
      '-t',
      '-c',
      sql,
    ],
    { cwd: repoRoot, maxBuffer: PSQL_MAX_BUFFER },
  );
  return stdout.trim();
}

/** 从一个 psql 输出里取第一段 UUID（容忍 RETURNING 之外的命令标签行）。 */
function firstUuid(output: string): string {
  const match = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/.exec(output);
  return match?.[0] ?? '';
}

interface SessionCookie {
  name: string;
  value: string;
  domain: string;
  path: string;
  expires: number;
  httpOnly: boolean;
  secure: boolean;
  sameSite: 'Lax';
}

interface SeededSession {
  username: string;
  userId: string;
  cookie: SessionCookie;
}

/**
 * 直连数据库创建「临时角色账号 + 一个已登录会话」。
 *
 * 为什么不用 POST /api/users：见文件头（无 SMTP → 503，拿不到最终密码）。
 * password_hash 复用 admin 行，因此该账号可用 ADMIN_PASSWORD 登录。
 */
async function seedUserWithSession(username: string, role: string): Promise<SeededSession> {
  const value = randomBytes(32).toString('base64url');
  const tokenHash = createHash('sha256').update(value).digest('hex');
  const output = await psql(`
    WITH u AS (
      INSERT INTO users (username, password_hash, role, enabled, display_name)
      SELECT '${username}', password_hash, '${role}', true, '${username}'
      FROM users WHERE username = 'admin'
      RETURNING id
    ), s AS (
      INSERT INTO sessions (user_id, token_hash, expires_at, last_seen_at)
      SELECT id, decode('${tokenHash}', 'hex'), now() + interval '12 hours', now() FROM u
      RETURNING user_id
    )
    SELECT id::text FROM u;`);
  // psql 在 RETURNING 之外还会打印数据修改语句的命令标签（INSERT 0 1），
  // 这里只取第一段 UUID。
  const id = firstUuid(output);
  if (id === '') {
    throw new Error(`seedUserWithSession(${username}) returned unexpected id: ${output}`);
  }
  return {
    username,
    userId: id,
    cookie: {
      name: 'opsmind_session',
      value,
      domain: 'localhost',
      path: '/',
      expires: -1,
      httpOnly: true,
      secure: false,
      sameSite: 'Lax',
    },
  };
}

async function deleteSeededUser(username: string): Promise<void> {
  await psql(
    `DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE username = '${username}');
     DELETE FROM users WHERE username = '${username}';`,
  );
}

async function newSessionContext(browser: Browser, username: string, role: string) {
  const seeded = await seedUserWithSession(username, role);
  const context = await browser.newContext({
    storageState: { cookies: [seeded.cookie], origins: [] },
  });
  return { seeded, context };
}

/** 登录表单登录任意账号（username 可输入，00012 起）。 */
async function loginAs(page: Page, username: string, password: string): Promise<void> {
  await page.goto('/login');
  await page.getByPlaceholder('请输入用户名').fill(username);
  const input = page.getByPlaceholder('请输入密码');
  await expect(input).toBeVisible();
  await input.fill(password);
  // antd 对双字按钮插入空格（accessible name「登 录」），正则兼容。
  await page.getByRole('button', { name: /登\s*录/ }).click();
}

/** 左侧导航（NAV_ITEMS 渲染结果）的可见文案集合，顺序与菜单一致。 */
async function visibleNavLabels(page: Page): Promise<string[]> {
  await expect(page.locator('.ant-menu-item').first()).toBeVisible();
  const raw = await page.locator('.ant-menu-item').allInnerTexts();
  return raw.map((text) => text.trim()).filter((text) => text.length > 0);
}

interface RoleCase {
  username: string;
  password: string;
  roleLabel: string;
  /** 登录响应里 permissions 推出的默认落地页（lib/rbac.ts defaultLandingPath）。 */
  landing: string;
  nav: string[];
  /** 直达这些路径必须渲染 403 页，而不是空白/转圈/静默重定向。 */
  forbidden: Array<{ path: string; label: string }>;
}

// 期望值来自 CONTRACT.md 12.2 + web/src/components/navItems.ts 的 permissions
// 声明（任一命中即保留），不含任何服务端实现细节的猜测。
const ROLE_CASES: RoleCase[] = [
  {
    username: 'admin',
    password: ADMIN_PASSWORD,
    roleLabel: '管理员',
    landing: '/chat',
    nav: [
      '对话',
      '审批',
      '服务器',
      '数据库',
      '模型',
      '审计',
      '终端',
      '录制',
      '桌面',
      '容器',
      '监控',
      '告警',
      '巡检',
      '批量',
      '回放执行',
      '设置',
      '用户',
    ],
    forbidden: [],
  },
  {
    username: 'operator',
    password: ADMIN_PASSWORD,
    roleLabel: '运维工程师',
    landing: '/chat',
    // chat:use / asset:read / exec:read / exec:write / monitor:read /
    // inspection:manage / terminal:use → 无 /approvals /providers /audit
    // /settings /users。
    nav: [
      '对话',
      '服务器',
      '数据库',
      '终端',
      '录制',
      '桌面',
      '容器',
      '监控',
      '告警',
      '巡检',
      '批量',
      '回放执行',
    ],
    forbidden: [
      { path: '/users', label: '用户' },
      { path: '/audit', label: '审计' },
      { path: '/approvals', label: '审批' },
      { path: '/providers', label: '模型' },
      { path: '/settings', label: '设置' },
    ],
  },
  {
    username: 'reviewer',
    password: ADMIN_PASSWORD,
    roleLabel: '审批者',
    landing: '/approvals',
    // 只有 approval:read + asset:read（12.2）。
    nav: ['审批', '服务器', '数据库'],
    forbidden: [
      { path: '/chat', label: '对话' },
      { path: '/audit', label: '审计' },
      { path: '/users', label: '用户' },
      { path: '/term', label: '终端' },
      { path: '/monitor', label: '监控' },
      { path: '/settings', label: '设置' },
    ],
  },
  {
    username: 'auditor',
    password: ADMIN_PASSWORD,
    roleLabel: '审计员',
    landing: '/audit',
    // audit:read → 审计、跨用户录制、回放执行（navItems.ts:89-93 / 136-141）。
    nav: ['审计', '录制', '回放执行'],
    forbidden: [
      { path: '/chat', label: '对话' },
      { path: '/servers', label: '服务器' },
      { path: '/databases', label: '数据库' },
      { path: '/approvals', label: '审批' },
      { path: '/users', label: '用户' },
      { path: '/settings', label: '设置' },
      { path: '/monitor', label: '监控' },
      { path: '/term', label: '终端' },
    ],
  },
];

const caseOf = (username: string): RoleCase => {
  const found = ROLE_CASES.find((item) => item.username === username);
  if (!found) {
    throw new Error(`role case missing for ${username}`);
  }
  return found;
};

test.describe.configure({ mode: 'serial' });

test.describe('RBAC 四角色：登录表单 → 导航项集合（CONTRACT 12.2）', () => {
  for (const role of ROLE_CASES) {
    test(`${role.username}（${role.roleLabel}）登录后导航项与契约一致`, async ({ browser }) => {
      const context = await browser.newContext();
      const page = await context.newPage();
      try {
        await loginAs(page, role.username, role.password);

        // 登录响应里的 permissions 决定默认落地页（lib/rbac.ts:
        // defaultLandingPath）：审批者/审计员不应被送到 /chat 再吃 403。
        await page.waitForURL((url) => url.pathname === role.landing, { timeout: 30_000 });
        await expect(page.getByTitle('当前角色')).toHaveText(role.roleLabel);

        expect(await visibleNavLabels(page)).toEqual(role.nav);

        // 反向断言：矩阵外的入口一个都不渲染（不是「被隐藏但仍在 DOM」）。
        const labels = await visibleNavLabels(page);
        for (const forbiddenNav of ['审批', '审计', '用户', '设置', '模型']) {
          if (!role.nav.includes(forbiddenNav)) {
            expect(labels, `${role.username} 不应看到「${forbiddenNav}」`).not.toContain(
              forbiddenNav,
            );
          }
        }
      } finally {
        await context.close();
      }
    });
  }
});

// admin 的会话来自 global-setup 播种（登录限流 5 次/12s，见
// global-setup.ts:341-342）；上面 4 次真实表单登录 + setup 的 1 次刚好用满
// 一轮配额，其余用例复用已播种会话。
test.describe('RBAC 四角色：直达无权路由渲染 403 页（浏览器输入 URL）', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('operator 直达 /users /audit /approvals /providers /settings 均 403 且可读', async ({
    page,
  }) => {
    for (const target of caseOf('operator').forbidden) {
      await assertForbiddenPage(page, target.path, '运维工程师', target.label);
    }
  });
});

test.describe('RBAC 四角色：审批者 403', () => {
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('approver 直达发起/执行/审计页均 403 且可读', async ({ page }) => {
    for (const target of caseOf('reviewer').forbidden) {
      await assertForbiddenPage(page, target.path, '审批者', target.label);
    }
  });
});

test.describe('RBAC 四角色：审计员 403', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('auditor 直达执行/管理页均 403 且可读', async ({ page }) => {
    for (const target of caseOf('auditor').forbidden) {
      await assertForbiddenPage(page, target.path, '审计员', target.label);
    }
  });
});

test.describe('RBAC 四角色：admin 全量直达', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('admin 直达 /users /audit /settings /approvals 不出现 403 页', async ({ page }) => {
    for (const target of ['/users', '/audit', '/settings', '/approvals', '/providers']) {
      await page.goto(target);
      await expect(page.locator('.opsmind-spinner')).toHaveCount(0, { timeout: 30_000 });
      await expect(page.getByText('无权访问', { exact: true })).toHaveCount(0);
      expect(new URL(page.url()).pathname).toBe(target);
    }
  });
});

/**
 * 403 页断言（web/src/features/ForbiddenPage.tsx）：
 * - 标题「无权访问」+ 角色名 + 目标页面名（文案可读，不是错误码/白屏）；
 * - URL 保持原样（PermissionGate 渲染而非重定向）；
 * - 无加载指示器残留（不是无限转圈），也不是空白容器。
 */
async function assertForbiddenPage(
  page: Page,
  target: string,
  roleLabel: string,
  pageLabel: string,
): Promise<void> {
  const response = await page.goto(target);
  expect(response?.status(), `goto ${target}`).toBe(200);

  await expect(page.getByText('无权访问', { exact: true }).first()).toBeVisible({
    timeout: 30_000,
  });
  await expect(
    page.getByText(`当前账号的角色是「${roleLabel}」，没有访问「${pageLabel}」的权限。`),
  ).toBeVisible();
  // 说明文字与可操作出口（不是只有一句标题）。
  await expect(
    // JSX 里的换行会被折成一个空格，用正则容忍。
    page.getByText(
      /如需使用该功能，请联系管理员调整角色；界面只展示当前角色可用的入口，\s*越权请求会被服务端拒绝并记录审计。/,
    ),
  ).toBeVisible();
  await expect(page.getByRole('button', { name: '返回上一页' })).toBeVisible();

  // 不是无限转圈：403 文案出现后不得再有加载指示器。
  await expect(page.locator('.opsmind-spinner')).toHaveCount(0);

  // 不是静默重定向：地址栏仍是用户输入的路径。
  expect(new URL(page.url()).pathname).toBe(target);
  // 不是白屏：403 结果容器有实际尺寸。
  const box = await page.getByText('无权访问', { exact: true }).first().boundingBox();
  expect(box?.height ?? 0).toBeGreaterThan(0);
}

test.describe('RBAC 账号来源：未配置 SMTP 时 POST /api/users 返回 503', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('创建用户返回 503 DELIVERY_UNAVAILABLE（因此用例直连数据库播种）', async ({ page }) => {
    const response = await page.request.post('/api/users', {
      headers: { Origin: E2E_ORIGIN },
      data: {
        username: `rbac-invite-${Date.now()}`,
        email: 'rbac-invite@example.com',
        role: 'operator',
      },
    });
    const body = (await response.json()) as { error?: { code?: string } };
    expect(response.status(), JSON.stringify(body)).toBe(503);
    expect(body.error?.code).toBe('DELIVERY_UNAVAILABLE');
  });
});

test.describe('RBAC 审批者工作流（approver）', () => {
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('待审批列表 → 上下文 → 批准成功，且不能审批自己发起的那条', async ({ browser, page }) => {
    // 真实链路：运算符（operator）在真实页面发起需审批命令，审批者（本页）
    // 在审批队列看到它。审批 TTL 被 global-setup 收紧到 45s，本用例的决策
    // 都在数秒内完成。
    const operator = await browser.newContext({ storageState: OPERATOR_STATE_FILE });
    const operatorPage = await operator.newPage();
    const approverApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: REVIEWER_STATE_FILE,
    });
    const adminApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    let selfApprovalId = '';
    try {
      // ---------- 1) 运维发起需审批命令 ----------
      await operatorPage.goto('/chat');
      await operatorPage.getByLabel('消息输入框').waitFor();
      await sendMessage(operatorPage, SCENARIO_RISKY);
      await expect(operatorPage).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 30_000 });
      const operatorCard = operatorPage.locator('.chat-approval', { hasText: '命令审批' }).first();
      await expect(operatorCard).toBeVisible({ timeout: 30_000 });
      await expect(operatorCard.getByText('等待审批者处理')).toBeVisible();
      const conversationId = conversationIdOf(operatorPage);

      // 审批归属（12.3 第 5 条）：requested_by 必须等于会话 owner（operator）。
      const pendingList = await approverApi.get(
        `/api/approvals?conversation_id=${conversationId}&status=pending&limit=10`,
      );
      expect(pendingList.status(), await pendingList.text()).toBe(200);
      const pending = (await pendingList.json()) as {
        items: Array<{ id: string; command: string; requested_by_name: string; status: string }>;
      };
      const risky = pending.items.find(
        (item) => item.command === RISKY_COMMAND && item.status === 'pending',
      );
      expect(risky, JSON.stringify(pending)).toBeTruthy();
      expect(risky?.requested_by_name).toBe('operator');

      // ---------- 2) 审批队列 → 上下文抽屉 ----------
      await page.goto('/approvals');
      const card = page.locator('.chat-approval', { hasText: RISKY_COMMAND }).first();
      await expect(card).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText('发起人：operator').first()).toBeVisible();
      await expect(card.getByRole('button', { name: '批准' })).toBeVisible();
      await expect(card.getByRole('button', { name: '拒绝' })).toBeVisible();

      await card.getByRole('button', { name: '查看上下文' }).click();
      const drawer = page.getByRole('dialog');
      await expect(drawer.getByText('审批上下文')).toBeVisible();
      await expect(drawer.getByText('审批创建前的上下文')).toBeVisible();
      // 审批前消息 ≤20 条（12.3 第 23 条）；命令以工具参数形式出现。
      await expect(drawer.getByText(/审批前消息（\d+ 条）/)).toBeVisible();
      await expect(drawer.getByText('ssh_exec')).toBeVisible();
      await expect(drawer.getByText(`"command": "${RISKY_COMMAND}"`)).toBeVisible();
      await drawer.locator('.ant-drawer-close').click();
      await expect(drawer).toBeHidden();

      // ---------- 3) 批准成功 ----------
      await card.getByRole('button', { name: '批准' }).click();
      // 队列只列 pending：决策成功后卡片随 refetch 移除。
      await expect(card).toHaveCount(0, { timeout: 30_000 });

      const detail = await approverApi.get(`/api/approvals/${risky?.id}`);
      expect(detail.status(), await detail.text()).toBe(200);
      const decided = (await detail.json()) as {
        status: string;
        decided_by: string | null;
        requested_by: string;
      };
      expect(decided.status).toBe('approved');
      expect(decided.decided_by).toBeTruthy();
      expect(decided.requested_by).not.toBe(decided.decided_by);

      // 发起人页面经 SSE 收到决策：状态翻转为已批准（operator 能看到自己
      // 发起的审批状态，虽然没有审批按钮）。
      await expect(operatorCard.getByText('审批已批准', { exact: true })).toBeVisible({
        timeout: 30_000,
      });

      // ---------- 4) 不能审批自己发起的（requested_by = approver 本人） ----------
      // approver 无 chat:use，无法自己发起对话；按契约 12.3 第 5 条构造一条
      // requested_by=approver 的 console 审批（source='console' 允许
      // conversation_id/tool_call_id 为 NULL，见 00006_phase4.sql:17-22）。
      const selfApprovalOutput = await psql(`
        INSERT INTO approvals (source, server_id, command, risk_level, rule_id, reason,
                               status, approval_mode, expires_at, requested_by)
        SELECT 'console', s.id, 'echo self-approval-check', 'needs_approval', '',
               'e2e 职责分离校验：发起人 = 审批者', 'pending', 'standard',
               now() + interval '10 minutes', u.id
        FROM users u, servers s
        WHERE u.username = 'reviewer' AND s.name = 'e2e-fake-host'
        RETURNING id;`);
      selfApprovalId = firstUuid(selfApprovalOutput);
      expect(selfApprovalId, selfApprovalOutput).not.toBe('');

      await page.reload();
      const selfCard = page.locator('.chat-approval', { hasText: 'echo self-approval-check' });
      await expect(selfCard).toBeVisible({ timeout: 30_000 });
      await expect(selfCard.getByText('不能审批自己发起的请求')).toBeVisible();
      await expect(
        selfCard.getByText('审批需由另一位具备审批权限的账号处理（服务端会拒绝自审）。'),
      ).toBeVisible();
      await expect(selfCard.getByRole('button', { name: '批准' })).toHaveCount(0);
      await expect(selfCard.getByRole('button', { name: '拒绝' })).toHaveCount(0);

      // 服务端仍是唯一授权点：直接调 decide（deny 不触发「批准前复检发起人
      // 执行权限」，必定走到职责分离判定）返回 403 APPROVAL_SELF_DECISION，
      // 且审批保持 pending（12.3 第 6 条）。
      const selfDecision = await approverApi.post(`/api/approvals/${selfApprovalId}/decide`, {
        headers: { Origin: E2E_ORIGIN },
        data: { decision: 'deny' },
      });
      const selfBody = (await selfDecision.json()) as {
        error?: { code?: string; message?: string };
      };
      expect(selfDecision.status(), JSON.stringify(selfBody)).toBe(403);
      expect(selfBody.error?.code).toBe('APPROVAL_SELF_DECISION');
      expect((selfBody.error?.message ?? '').length).toBeGreaterThan(0);

      const stillPending = await adminApi.get(`/api/approvals/${selfApprovalId}`);
      expect(stillPending.status(), await stillPending.text()).toBe(200);
      expect(((await stillPending.json()) as { status: string }).status).toBe('pending');
    } finally {
      if (selfApprovalId) {
        // 清理：把构造行收敛为 expired，避免污染后续审批队列断言。
        await psql(
          `UPDATE approvals SET status = 'expired' WHERE id = '${selfApprovalId}' AND status = 'pending';`,
        ).catch(() => undefined);
      }
      await adminApi.dispose();
      await approverApi.dispose();
      await operator.close();
    }
  });
});

test.describe('RBAC 审计员只读（auditor）', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('可读审计/回放/录制，页面无写入口，写接口一律 403', async ({ page }) => {
    // ---------- 可读：三个只读页面 + 只读提示 ----------
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText('审计员为只读角色')).toBeVisible();
    // 只读操作存在：校验哈希链（GET）与刷新。
    await expect(page.getByRole('button', { name: /校验哈希链/ })).toBeVisible();
    await expect(page.getByRole('button', { name: '刷新' })).toBeVisible();
    // 写入口一个都不渲染（新增/编辑/删除/保存/批准/拒绝…）。
    const auditButtons = (await page.getByRole('button').allInnerTexts()).join('|');
    expect(auditButtons).not.toMatch(/新增|编辑|删除|保存|批准|拒绝|启用|停用|上传/);

    await page.goto('/replays');
    await expect(page.getByRole('heading', { name: '回放执行' })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText('审计员为只读角色')).toBeVisible();

    await page.goto('/recordings');
    await expect(page.getByRole('heading', { name: '录制' })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText('审计员为只读角色')).toBeVisible();

    // ---------- 只读 API 200，写/越权 API 403 ----------
    const auditorApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUDITOR_STATE_FILE,
    });
    const adminApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    try {
      for (const readPath of ['/api/audit', '/api/audit/verify', '/api/replays']) {
        const response = await auditorApi.get(readPath, {
          headers: { Origin: E2E_ORIGIN },
        });
        expect(response.status(), `${readPath}: ${await response.text()}`).toBe(200);
      }
      // 12.3 第 4/24 条：audit:read 可看跨用户录制。
      const recordings = await auditorApi.get('/api/term/recordings', {
        headers: { Origin: E2E_ORIGIN },
      });
      expect(recordings.status(), await recordings.text()).toBe(200);

      const servers = await adminApi.get('/api/servers', { headers: { Origin: E2E_ORIGIN } });
      expect(servers.status()).toBe(200);
      const serverId = ((await servers.json()) as { items: Array<{ id: string }> }).items[0]?.id;
      expect(serverId).toBeTruthy();

      // asset:read 都不给：读资产也 403（矩阵里 auditor 该格为 —）。
      await expectForbiddenApi(
        await auditorApi.get('/api/servers', { headers: { Origin: E2E_ORIGIN } }),
        'GET /api/servers',
      );
      await expectForbiddenApi(
        await auditorApi.get('/api/approvals', { headers: { Origin: E2E_ORIGIN } }),
        'GET /api/approvals',
      );

      const writes: Array<{ label: string; response: APIResponse }> = [
        {
          label: 'POST /api/servers',
          response: await auditorApi.post('/api/servers', {
            headers: { Origin: E2E_ORIGIN },
            data: {
              name: `auditor-write-${Date.now()}`,
              host: '127.0.0.1',
              port: 22,
              username: 'e2e',
              auth_method: 'password',
              password: 'not-a-real-password',
            },
          }),
        },
        {
          label: `PATCH /api/servers/${serverId}`,
          response: await auditorApi.patch(`/api/servers/${serverId}`, {
            headers: { Origin: E2E_ORIGIN },
            data: { name: 'auditor-rename' },
          }),
        },
        {
          label: 'PUT /api/settings',
          response: await auditorApi.put('/api/settings', {
            headers: { Origin: E2E_ORIGIN },
            data: { approval_mode: 'none' },
          }),
        },
        {
          label: 'POST /api/users',
          response: await auditorApi.post('/api/users', {
            headers: { Origin: E2E_ORIGIN },
            data: {
              username: `auditor-user-${Date.now()}`,
              email: 'auditor-user@example.com',
              role: 'operator',
            },
          }),
        },
        {
          label: 'POST /api/approvals/:id/decide',
          response: await auditorApi.post(
            '/api/approvals/00000000-0000-4000-8000-000000000000/decide',
            { headers: { Origin: E2E_ORIGIN }, data: { decision: 'approve' } },
          ),
        },
        {
          label: 'POST /api/term/grants',
          response: await auditorApi.post('/api/term/grants', {
            headers: { Origin: E2E_ORIGIN },
            data: { server_id: serverId },
          }),
        },
      ];
      for (const write of writes) {
        await expectForbiddenApi(write.response, write.label);
      }

      // 写被拒绝后目标资源未被修改：服务器名称仍是原值。
      const after = await adminApi.get(`/api/servers/${serverId}`, {
        headers: { Origin: E2E_ORIGIN },
      });
      expect(after.status(), await after.text()).toBe(200);
      expect(((await after.json()) as { name: string }).name).not.toBe('auditor-rename');
    } finally {
      await adminApi.dispose();
      await auditorApi.dispose();
    }
  });
});

/** 403 响应断言：状态 403 + 错误码 FORBIDDEN（12.4 第 1 条）。 */
async function expectForbiddenApi(
  response: { status: () => number; text: () => Promise<string> },
  label: string,
): Promise<void> {
  const body = await response.text();
  expect(response.status(), `${label} -> ${body}`).toBe(403);
  const parsed = JSON.parse(body) as { error?: { code?: string } };
  expect(parsed.error?.code, `${label} -> ${body}`).toBe('FORBIDDEN');
}

test.describe('RBAC 运维工程师：无审批按钮但能看到自己发起的审批状态', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('审批卡只读提示 + 决策后状态翻转 + 不请求 /api/approvals', async ({ page }) => {
    const approvalRequests: string[] = [];
    const forbiddenResponses: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/approvals')) {
        approvalRequests.push(`${request.method()} ${request.url()}`);
      }
    });
    page.on('response', (response) => {
      if (response.url().includes('/api/approvals') && response.status() === 403) {
        forbiddenResponses.push(response.url());
      }
    });

    await page.goto('/chat');
    await page.getByLabel('消息输入框').waitFor();
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 30_000 });
    const card = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(card).toBeVisible({ timeout: 30_000 });

    // operator 只有 exec:write，无 approval:read/decide：不渲染批准/拒绝，
    // 也没有上下文入口，改为「等待审批者处理」说明。
    await expect(card.getByText('等待审批者处理')).toBeVisible();
    await expect(
      card.getByText('当前账号没有审批权限；审批将由具备审批权限的账号处理。'),
    ).toBeVisible();
    await expect(card.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(card.getByRole('button', { name: '拒绝' })).toHaveCount(0);
    await expect(card.getByRole('button', { name: '查看上下文' })).toHaveCount(0);

    // 会话页不因审批列表 403 产生请求（ChatPage.approvalsQueryEnabled，
    // 12.3 第 28 条）。
    expect(approvalRequests).toEqual([]);
    expect(forbiddenResponses).toEqual([]);

    // 由独立审批者会话真实决策（不是测试侧伪造状态）。
    const approverApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: REVIEWER_STATE_FILE,
    });
    try {
      const conversationId = conversationIdOf(page);
      const list = await approverApi.get(
        `/api/approvals?conversation_id=${conversationId}&status=pending&limit=10`,
      );
      expect(list.status(), await list.text()).toBe(200);
      const approval = (
        (await list.json()) as { items: Array<{ id: string; command: string }> }
      ).items.find((item) => item.command === RISKY_COMMAND);
      expect(approval, 'pending approval for operator command').toBeTruthy();
      const decided = await approverApi.post(`/api/approvals/${approval?.id}/decide`, {
        headers: { Origin: E2E_ORIGIN },
        data: { decision: 'approve' },
      });
      expect(decided.status(), await decided.text()).toBe(200);

      // operator 能看到自己发起的这条审批的最终状态（SSE 推送）。
      await expect(card.getByText('审批已批准', { exact: true })).toBeVisible({
        timeout: 30_000,
      });
    } finally {
      await approverApi.dispose();
    }
  });
});

test.describe('RBAC 会话失效：角色变更 / 停用后旧页面被引导回登录', () => {
  test('改角色后旧会话下一次请求 401 并带 reason=session_expired 提示', async ({ browser }) => {
    const { seeded, context } = await newSessionContext(browser, 'rbac-rolechange', 'operator');
    const adminApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    try {
      const page = await context.newPage();
      await page.goto('/servers');
      await expect(page.getByRole('heading', { name: '服务器' })).toBeVisible({ timeout: 30_000 });

      // 管理员改角色：同一事务吊销目标全部 auth session（12.3 第 13/14 条）。
      const patched = await adminApi.patch(`/api/users/${seeded.userId}`, {
        headers: { Origin: E2E_ORIGIN },
        data: { role: 'auditor' },
      });
      expect(patched.status(), await patched.text()).toBe(200);

      // 旧 Cookie 已失效：/api/auth/me 401。
      const staleApi = await playwrightRequest.newContext({
        baseURL: E2E_ORIGIN,
        storageState: { cookies: [seeded.cookie], origins: [] },
      });
      try {
        const me = await staleApi.get('/api/auth/me', { headers: { Origin: E2E_ORIGIN } });
        expect(me.status(), await me.text()).toBe(401);
      } finally {
        await staleApi.dispose();
      }

      // 旧页面下一次请求（reload → me 401）被引导回登录页并说明原因，
      // 而不是停在旧数据/白屏。
      await page.reload();
      await page.waitForURL(/\/login\?.*reason=session_expired/, { timeout: 30_000 });
      expect(new URL(page.url()).searchParams.get('next')).toBe('/servers');
      await expect(
        page.getByText('登录已失效（会话过期或角色/权限已变更），请重新登录。'),
      ).toBeVisible();
    } finally {
      await adminApi.dispose();
      await context.close();
      await deleteSeededUser(seeded.username);
    }
  });

  test('停用后旧会话下一次请求 401 并被引导回登录', async ({ browser }) => {
    const { seeded, context } = await newSessionContext(browser, 'rbac-disabled', 'operator');
    const adminApi = await playwrightRequest.newContext({
      baseURL: E2E_ORIGIN,
      storageState: AUTH_STATE_FILE,
    });
    try {
      const page = await context.newPage();
      await page.goto('/chat');
      await expect(page.getByLabel('消息输入框')).toBeVisible({ timeout: 30_000 });

      // 停用 = 软删（PATCH {enabled:false}），复用吊销会话链路。
      const patched = await adminApi.patch(`/api/users/${seeded.userId}`, {
        headers: { Origin: E2E_ORIGIN },
        data: { enabled: false },
      });
      expect(patched.status(), await patched.text()).toBe(200);

      await page.reload();
      await page.waitForURL(/\/login\?.*reason=session_expired/, { timeout: 30_000 });
      await expect(
        page.getByText('登录已失效（会话过期或角色/权限已变更），请重新登录。'),
      ).toBeVisible();

      // 停用账号再登录只能得到统一的 AUTH_INVALID（禁止账号枚举）。
      await loginAs(page, seeded.username, ADMIN_PASSWORD);
      await expect(page.getByText('用户名或密码错误')).toBeVisible({ timeout: 20_000 });
    } finally {
      await adminApi.dispose();
      await context.close();
      await deleteSeededUser(seeded.username);
    }
  });
});
