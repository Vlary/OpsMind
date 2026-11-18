// RBAC 第 11 轮走查的端到端回归：
// 1) 过期 pending 审批（sweeper 尚未收敛的窗口）POST decide 必须 409 且不执行；
// 2) 诊断会话列表与详情同源：非 admin 看不到他人/系统会话的摘要与「查看报告」；
// 3) 设置页降级文案改为「管理员 + 审批者合计」口径。
import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { request as playwrightRequest } from '@playwright/test';

import { expect, test } from './fixtures';
import { AUTH_STATE_FILE, OPERATOR_STATE_FILE, REVIEWER_STATE_FILE } from './helpers';

// 默认以 admin 页面会话运行；诊断归属用例的 describe 内覆盖为 operator。
test.use({ storageState: AUTH_STATE_FILE });

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const E2E_DATABASE = 'aiops_e2e';
const E2E_ORIGIN = 'http://localhost:4173';
// psql 输出上限 1MB：只取单行 id/计数，任何意外放大都会立即失败（防 OOM）。
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

test('过期审批 POST decide → 409 且不执行（TTL 是服务端有效性的一部分）', async () => {
  const approvalID = firstUuid(
    await psql(`
      INSERT INTO approvals (source, server_id, command, risk_level, rule_id, reason,
                             status, approval_mode, expires_at, requested_at, requested_by)
      SELECT 'console', s.id, 'uptime', 'needs_approval', '',
             'round11-expired', 'pending', 'standard',
             now() - interval '2 seconds', now() - interval '2 minutes', u.id
      FROM users u, servers s
      WHERE u.username = 'operator' AND s.name = 'e2e-fake-host'
      RETURNING id;`),
  );
  expect(approvalID, 'expired approval id').not.toBe('');

  const reviewer = await playwrightRequest.newContext({
    baseURL: E2E_ORIGIN,
    storageState: REVIEWER_STATE_FILE,
  });
  const response = await reviewer.post(`/api/approvals/${approvalID}/decide`, {
    headers: { Origin: E2E_ORIGIN },
    data: { decision: 'approve' },
  });
  const body = await response.text();
  expect(response.status(), body).toBe(409);
  expect(body).toContain('APPROVAL_NOT_PENDING');
  await reviewer.dispose();

  // 行被收敛为 expired，且没有决定性字段（不会成为「已批准可执行」）。
  const row = await psql(
    `SELECT status || '|' || coalesce(decided_by::text, '') FROM approvals WHERE id = '${approvalID}';`,
  );
  expect(row).toBe('expired|');
});

test.describe('诊断会话列表归属收敛', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('运维工程师只看到自己的会话，他人/系统会话不出现在列表', async ({ page }) => {
    await psql(`
      INSERT INTO diagnosis_sessions
        (server_id, trigger, status, report, created_by, expires_at)
      SELECT s.id, 'manual', 'report',
             '{"summary":"ADMIN-ONLY-DIAG-11","severity":"info","root_cause":"r","evidence":[],"suggestions":[],"confidence":"high"}'::jsonb,
             u.id, now() + interval '30 minutes'
      FROM servers s, users u
      WHERE s.name = 'e2e-fake-host' AND u.username = 'admin';

      INSERT INTO diagnosis_sessions
        (server_id, trigger, status, report, created_by, expires_at)
      SELECT s.id, 'alert', 'report',
             '{"summary":"SYSTEM-ALERT-DIAG-11","severity":"warning","root_cause":"r","evidence":[],"suggestions":[],"confidence":"high"}'::jsonb,
             NULL, now() + interval '30 minutes'
      FROM servers s WHERE s.name = 'e2e-fake-host';

      INSERT INTO diagnosis_sessions
        (server_id, trigger, status, report, created_by, expires_at)
      SELECT s.id, 'manual', 'report',
             '{"summary":"OPERATOR-OWN-DIAG-11","severity":"info","root_cause":"r","evidence":[],"suggestions":[],"confidence":"high"}'::jsonb,
             u.id, now() + interval '30 minutes'
      FROM servers s, users u
      WHERE s.name = 'e2e-fake-host' AND u.username = 'operator';`);

    await page.goto('/alerts');
    await page.getByRole('tab', { name: 'AI 诊断' }).click();
    await expect(page.getByText('OPERATOR-OWN-DIAG-11')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('ADMIN-ONLY-DIAG-11')).toHaveCount(0);
    await expect(page.getByText('SYSTEM-ALERT-DIAG-11')).toHaveCount(0);

    // 自己的会话点「查看报告」成功打开抽屉（不再出现 404 引导）。
    const row = page.locator('tr', { hasText: 'OPERATOR-OWN-DIAG-11' }).first();
    await row.getByRole('button', { name: '查看报告' }).click();
    const drawer = page.getByRole('dialog');
    await expect(drawer.getByText('OPERATOR-OWN-DIAG-11')).toBeVisible();
    await expect(drawer.getByText('DIAGNOSIS_SESSION_NOT_FOUND')).toHaveCount(0);
  });
});

test('设置页降级口径为「管理员 + 审批者合计」', async ({ page }) => {
  // 默认 storageState 是 admin（playwright.config 的全局 setup）。
  await page.goto('/settings');
  const hint = page.getByText(/放宽会创建/);
  await expect(hint).toBeVisible({ timeout: 20_000 });
  const text = await hint.innerText();
  expect(text).toContain('管理员 + 审批者');
  expect(text).toContain('不足 2 个');
  expect(text).not.toContain('仅一个启用管理员');
});
