import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { expect, test } from './fixtures';
import { REVIEWER_STATE_FILE } from './helpers';

// RBAC 第 8 轮走查（medium）：审批者在 /approvals 批准 source=console 的
// 审批时，服务端在 decide handler 里立即执行命令并把 execution 放回响应，
// 但页头仍写「批准/拒绝后由发起人执行」、审批卡丢弃 execution，批准后只
// 闪现「已批准」——审批者会误判动作尚未执行或不知道是否失败。
//
// 覆盖：
// 1) 页头按来源说明执行时机（不再声称一律由发起人执行）；
// 2) 真实审批链路：直插一条 source=console、command=uptime、目标
//    e2e-fake-host（127.0.0.1:1，SSH 立拒）的 pending 审批，审批者点击
//    「批准」→ decide 响应 execution.error_code=SSH_EXEC_FAILED，
//    页面顶部保留「执行失败 + exit_code/error_code」的决策反馈。
//
// policy 审批卡不渲染「目标服务器」的断言放在 polish-rbac-round2.spec.ts
// 的 policy 卡片用例里（同一处 mock 列表，避免并发改动全局门限）。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
// global-setup.ts 重建的 e2e 数据库；psql 只用于构造「无法经当前角色
// 发起的 console 审批」（审批者没有 execute:readonly，无法发起诊断修复）。
const E2E_DATABASE = 'aiops_e2e';
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

/**
 * 构造一条待审批的 console 审批（requested_by=admin）。
 *
 * 与 rbac-roles.spec.ts 同因：approver 角色没有 execute:readonly，无法经
 * 诊断修复等业务入口创建 console 审批；直接插入与后端 Create 同形状的行，
 * 后续 decide 仍走真实 handler（复检发起人、同步执行、写审计）。
 */
async function insertPendingConsoleApproval(reason: string): Promise<string> {
  const output = await psql(`
    INSERT INTO approvals (source, server_id, command, risk_level, rule_id, reason,
                           status, approval_mode, expires_at, requested_by)
    SELECT 'console', s.id, 'uptime', 'needs_approval', '',
           '${reason}', 'pending', 'standard',
           now() + interval '10 minutes', u.id
    FROM users u, servers s
    WHERE u.username = 'admin' AND s.name = 'e2e-fake-host'
    RETURNING id;`);
  const id = firstUuid(output);
  expect(id, output).not.toBe('');
  return id;
}

test.describe('RBAC 第 8 轮：审批决定的执行时机与结果', () => {
  // 真实审批者账号（approver）：/approvals 是它的工作台。
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('批准 console 审批：页头说明服务端立即执行，结果展示 exit_code/error_code', async ({
    page,
  }) => {
    const approvalID = await insertPendingConsoleApproval('rev8 console exec ui');
    try {
      await page.goto('/approvals');
      const card = page.locator('.chat-approval', { hasText: 'uptime' }).first();
      await expect(card).toBeVisible({ timeout: 30_000 });

      // 页头：console/batch/诊断修复在批准时由服务端立即执行；策略批准即
      // 生效。修复前是无条件「批准/拒绝后由发起人执行」。
      await expect(
        page.getByText(/控制台\/批量\/诊断修复类审批在批准时由服务端立即执行/),
      ).toBeVisible();
      await expect(page.getByText(/批准\/拒绝后由发起人执行/)).toHaveCount(0);

      const decideResponse = page.waitForResponse(
        (response) =>
          response.url().includes(`/api/approvals/${approvalID}/decide`) &&
          response.request().method() === 'POST',
      );
      await card.getByRole('button', { name: '批准' }).click();
      const decided = await decideResponse;
      const raw = await decided.text();
      expect(decided.status(), raw).toBe(200);
      const body = JSON.parse(raw) as {
        approval: { status: string };
        execution: { exit_code: number; error_code?: string } | null;
      };
      // 服务端确实在批准时执行了（目标不可达 → 通道失败），不是「待发起人执行」。
      expect(body.approval.status).toBe('approved');
      expect(body.execution?.exit_code).toBe(-1);
      expect(body.execution?.error_code).toBe('SSH_EXEC_FAILED');

      // 审批卡离开 pending 队列后，页面顶部仍保留决定结果（修复前只有
      // 一闪而过的「已批准」，execution 被丢弃）。
      const feedback = page.getByTestId('approval-decision-feedback');
      await expect(feedback).toBeVisible({ timeout: 15_000 });
      await expect(feedback).toContainText('执行失败');
      await expect(feedback).toContainText('SSH_EXEC_FAILED');
      await expect(feedback).toContainText('exit_code=-1');
      await expect(feedback).toContainText('error_code=SSH_EXEC_FAILED');
    } finally {
      // 用例失败时清理未决行，避免污染后续审批队列断言。
      await psql(
        `UPDATE approvals SET status = 'expired' WHERE id = '${approvalID}' AND status = 'pending';`,
      ).catch(() => undefined);
    }
  });
});
