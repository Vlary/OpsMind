import { execFile } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { expect, test } from './fixtures';
import { AUTH_STATE_FILE, SCENARIO_RISKY, conversationIdOf, sendMessage } from './helpers';

// RBAC 第 1 轮走查的页面级回归（docs/CONTRACT.md 12.3 第 15/25 条）：
//
//  1. 停用账号后其 pending 审批立刻从审批队列消失（界面承诺「待处理审批会
//     立即失效」），re-enable 后也不会回来、也不能被批准；
//  2. 改角色弹窗显示角色职责说明（与创建弹窗同源）；
//  3. must_change_password 会话只渲染改密页，背后不再出现英文
//     PASSWORD_CHANGE_REQUIRED 与「模型供应商加载失败」横幅。
//
// 账号准备：e2e 无 SMTP，账号直连 psql 播种（与 rbac-roles.spec.ts 同一前提）。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const E2E_DATABASE = 'aiops_e2e';
const E2E_ORIGIN = 'http://localhost:4173';
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

function firstUuid(output: string): string {
  const match = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/.exec(output);
  return match?.[0] ?? '';
}

/** 播种一个指定角色的账号（复用 admin 密码哈希），返回 id。 */
async function seedUser(username: string, role: string): Promise<string> {
  const output = await psql(`
    INSERT INTO users (username, password_hash, role, enabled, display_name, email)
    SELECT '${username}', password_hash, '${role}', true, '${username}', '${username}@example.com'
    FROM users WHERE username = 'admin'
    RETURNING id::text;`);
  const id = firstUuid(output);
  if (!id) {
    throw new Error(`seedUser(${username}) returned unexpected id: ${output}`);
  }
  return id;
}

/** 为该账号写一个已登录会话并返回 storageState 片段。 */
async function seedSession(userId: string) {
  const value = randomBytes(32).toString('base64url');
  const tokenHash = createHash('sha256').update(value).digest('hex');
  await psql(
    `INSERT INTO sessions (user_id, token_hash, expires_at, last_seen_at)
     VALUES ('${userId}', decode('${tokenHash}', 'hex'), now() + interval '12 hours', now())`,
  );
  return {
    cookies: [
      {
        name: 'opsmind_session',
        value,
        domain: 'localhost',
        path: '/',
        expires: -1,
        httpOnly: true,
        secure: false,
        sameSite: 'Lax' as const,
      },
    ],
    origins: [],
  };
}

async function cleanup(username: string): Promise<void> {
  await psql(
    `DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE username = '${username}');
     DELETE FROM approvals WHERE requested_by = (SELECT id FROM users WHERE username = '${username}');
     DELETE FROM users WHERE username = '${username}';`,
  ).catch(() => undefined);
}

test.describe.configure({ mode: 'serial' });

test.describe('RBAC 第 1 轮走查：停用账号作废待处理审批', () => {
  // 管理员会话：审批队列与用户停用都需要 users:write。
  test.use({ storageState: AUTH_STATE_FILE });

  test('停用后 pending 审批立即不在队列，re-enable 也不能批准执行', async ({ page, browser }) => {
    const username = `revoked${Date.now() % 100000}`;
    const userId = await seedUser(username, 'operator');
    let operatorContext: Awaited<ReturnType<typeof browser.newContext>> | null = null;
    try {
      // 1) operator 发起一条高危命令（假 LLM 场景），产生 pending 审批。
      //    上下文必须保持打开：关闭页面会断开 SSE，服务端按「流断开」把该
      //    审批置 stream_cancelled（六期打磨 sprint 3 的既有行为），那就测
      //    不到「停用后失效」这条链路了。
      operatorContext = await browser.newContext({ storageState: await seedSession(userId) });
      const operatorPage = await operatorContext.newPage();
      await operatorPage.goto('/chat');
      await sendMessage(operatorPage, SCENARIO_RISKY);
      await expect(operatorPage).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
      await expect(
        operatorPage.locator('.chat-approval', { hasText: '命令审批' }).first(),
      ).toBeVisible({ timeout: 30_000 });
      const conversationId = conversationIdOf(operatorPage);

      // 管理员视角的审批队列里能看到它（审批页按卡片渲染，发起人在卡片上方）。
      await page.goto('/approvals');
      await expect(page.getByRole('heading', { name: '审批' })).toBeVisible({ timeout: 20_000 });
      const requesterLine = page.getByText(`发起人：${username}`, { exact: true });
      await expect(requesterLine).toBeVisible({ timeout: 20_000 });

      // 2) 停用账号 → 该卡片必须立即消失（pending 已置 expired）。
      const patch = await page.request.patch(`/api/users/${userId}`, {
        headers: { Origin: E2E_ORIGIN },
        data: { enabled: false },
      });
      expect(patch.status(), await patch.text()).toBe(200);
      await page.getByRole('button', { name: /刷\s*新/ }).click();
      await expect(page.getByText(`发起人：${username}`, { exact: true })).toHaveCount(0, {
        timeout: 20_000,
      });

      // 3) re-enable → 旧审批不得复活，也不能再被批准。
      const enable = await page.request.patch(`/api/users/${userId}`, {
        headers: { Origin: E2E_ORIGIN },
        data: { enabled: true },
      });
      expect(enable.status(), await enable.text()).toBe(200);
      await page.reload();
      await expect(page.getByText(`发起人：${username}`, { exact: true })).toHaveCount(0, {
        timeout: 20_000,
      });
      // 该会话下已没有任何 pending 审批（旧审批是 expired，不是 pending）。
      const pending = await page.request.get(
        `/api/approvals?status=pending&conversation_id=${conversationId}&limit=200`,
      );
      expect(pending.status()).toBe(200);
      const body = (await pending.json()) as {
        items: Array<{ id: string; status: string; command: string }>;
      };
      expect(body.items).toHaveLength(0);
      // 会话的 tool_call 也收敛为 expired（不再 awaiting_approval）。
      // 会话 owner 是临时 operator，管理员读 tool-calls 会 404（归属隔离），
      // 因此直接查库断言（只读单行，输出上限 1MB）。
      const tcStatus = await psql(
        `SELECT coalesce(string_agg(status, ','), '') FROM tool_calls
         WHERE conversation_id = '${conversationId}'`,
      );
      expect(tcStatus).not.toContain('awaiting_approval');
      const approvalStatus = await psql(
        `SELECT coalesce(string_agg(status, ','), '') FROM approvals
         WHERE conversation_id = '${conversationId}'`,
      );
      expect(approvalStatus).not.toContain('pending');
    } finally {
      await operatorContext?.close();
      await cleanup(username);
    }
  });
});

test.describe('RBAC 第 1 轮走查：改角色弹窗职责说明', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('改角色弹窗显示所选角色的职责说明，切换角色同步更新', async ({ page }) => {
    await page.goto('/users');
    await expect(page.getByRole('heading', { name: '用户' })).toBeVisible({ timeout: 20_000 });
    // 用 operator 行：reviewer 在本环境是「当前登录用户」（页面标「我」），
    // 自我改角色会按契约被禁用，无法打开弹窗。
    const row = page.locator('.ant-table-row').filter({ hasText: 'operator' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: '改角色' }).click();

    const dialog = page.getByRole('dialog');
    // 初始角色是运维工程师：说明必须同屏可见（此前只有四个名字）。
    await expect(dialog.getByText('不能审批', { exact: false })).toBeVisible();

    // 切换到审批者 → 说明同步更新为「只审批不发起」。
    // antd 6 的 Select 是 ARIA combobox + aria-activedescendant：选项节点
    // 由虚拟列表管理，直接 click 会命中「不可见」的镜像节点；用键盘选择
    // 与真实用户一致（打开 → ArrowDown ×1 → Enter）。
    const roleSelect = dialog.getByRole('combobox', { name: '角色' });
    await roleSelect.click();
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(dialog.getByText('只审批不发起', { exact: false })).toBeVisible();

    // 再切到审计员 → 「只读治理」（从审批者再下移一位）。
    await roleSelect.click();
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(dialog.getByText('只读治理', { exact: false })).toBeVisible();
    // 取消：不改变任何账号状态。
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
  });
});

test.describe('RBAC 第 1 轮走查：must_change_password 页面', () => {
  test('强制改密会话只渲染改密页，无英文错误码与供应商加载失败横幅', async ({ browser }) => {
    const username = `mustchg${Date.now() % 100000}`;
    const userId = await seedUser(username, 'operator');
    await psql(`UPDATE users SET must_change_password = true WHERE id = '${userId}'`);
    const context = await browser.newContext({ storageState: await seedSession(userId) });
    try {
      const forced = await context.newPage();
      await forced.goto('/chat');
      await expect(forced.getByText('请先修改密码')).toBeVisible({ timeout: 20_000 });
      await expect(forced.getByRole('dialog', { name: '修改密码' })).toBeVisible();
      await expect(forced.getByText('PASSWORD_CHANGE_REQUIRED')).toHaveCount(0);
      await expect(forced.getByText('模型供应商加载失败', { exact: false })).toHaveCount(0);
    } finally {
      await context.close();
      await cleanup(username);
    }
  });
});
