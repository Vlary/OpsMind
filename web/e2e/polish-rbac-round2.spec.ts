import { execSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

import { expect, test } from './fixtures';
import {
  AUDITOR_STATE_FILE,
  AUTH_STATE_FILE,
  OPERATOR_STATE_FILE,
  REVIEWER_STATE_FILE,
} from './helpers';

// RBAC 第 2 轮验收走查的端到端验证（8 项中需要在浏览器侧取证的部分）：
//  3) 运维工程师（monitor:read，无 monitor:manage）在 /monitor 看到只读的采集
//     配置回显，没有可点的开关/间隔/保存——点击必然 403 的入口不再渲染；
//  5) 审批者看到的 policy 审批卡不再渲染空的「待执行命令」代码块；
//  7) 审计员校验哈希链时文案说明窗口覆盖区间（不是「全链通过」）；
//  8) 审批者批准终端授权后，「最近已决策」保留该行并标注待消费；
//     发起人 /term 顶部出现「你有 N 条已批准」提示。
//
// 探针说明：数据用页面会话（admin）真实建；跨用户/跨角色场景用预置的
// operator/reviewer/auditor storageState。第 7 项库行数不足以触发窗口
// 截断时，只断言文案含「链头」与响应字段（不做 499 行造数——那会污染
// 共享 e2e 库）。

const ORIGIN = 'http://localhost:4173';
const E2E_DATABASE = 'aiops_e2e';
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');

function psql(sql: string): void {
  execSync(
    `docker compose exec -T db psql -U aiops -d ${E2E_DATABASE} -v ON_ERROR_STOP=1 -c "${sql}"`,
    { cwd: repoRoot, stdio: ['ignore', 'pipe', 'pipe'] },
  );
}

test.describe('RBAC 第 2 轮：运维工程师的采集配置只读', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('/monitor 不渲染可提交的采集表单，改为只读回显且不发 PUT', async ({ page }) => {
    const puts: string[] = [];
    page.on('request', (request) => {
      if (request.method() === 'PUT' && request.url().includes('/api/monitor/collector')) {
        puts.push(request.url());
      }
    });

    await page.goto('/monitor');
    await expect(page.getByRole('heading', { name: '监控' })).toBeVisible({ timeout: 20_000 });

    const readonly = page.getByTestId('collector-readonly');
    await expect(readonly).toBeVisible({ timeout: 20_000 });
    await expect(readonly).toContainText('当前角色只读，不能修改采集配置');
    await expect(readonly).toContainText(/启用采集：/);
    await expect(readonly).toContainText(/采集间隔：\d+ 秒/);

    // 宁可失败的入口不再存在：保存按钮、开关、间隔输入框。
    await expect(page.getByRole('button', { name: '保存采集配置' })).toHaveCount(0);
    await expect(page.getByRole('switch', { name: '启用采集' })).toHaveCount(0);
    await expect(page.getByRole('spinbutton', { name: '采集间隔（秒）' })).toHaveCount(0);
    expect(puts).toEqual([]);
  });
});

test.describe('RBAC 第 2 轮：管理员仍可编辑采集配置', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('/monitor 对 monitor:manage 保持可提交表单', async ({ page }) => {
    await page.goto('/monitor');
    await expect(page.getByRole('button', { name: '保存采集配置' })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByTestId('collector-readonly')).toHaveCount(0);
  });
});

test.describe('RBAC 第 2 轮：policy 审批卡不渲染空命令区', () => {
  test.use({ storageState: REVIEWER_STATE_FILE });

  test('门限变更审批显示策略与「不含命令」，没有待执行命令代码块', async ({ page }) => {
    // 为避免改动共享环境的全局门限（放宽需要第二人批 + settings 写），
    // 这里直接伪造审批列表：结构与后端 Approval 一致，只验证卡片渲染分支
    //（后端 source=policy 的 command 恒为空串）。
    const approval = {
      id: '11111111-1111-4111-8111-111111111111',
      conversation_id: null,
      tool_call_id: null,
      source: 'policy',
      server_id: null,
      server_name: '',
      command: '',
      risk_level: 'safe',
      rule_id: '',
      reason: 'global approval mode widening',
      status: 'pending',
      approval_mode: 'all',
      requested_at: new Date().toISOString(),
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      decided_at: null,
      decided_by: null,
      note: '',
      edited_command: '',
      edited_risk_level: '',
      edited_rule_id: '',
      grant_kind: null,
      grant_expires_at: null,
      requested_by: '22222222-2222-4222-8222-222222222222',
      requested_by_name: 'admin',
      consumed_at: null,
      policy_from: 'all',
      policy_to: 'standard',
      break_glass: false,
      legacy: false,
    };
    await page.route(/\/api\/approvals\?/, async (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get('status') !== 'pending') {
        await route.fulfill({ status: 200, json: { items: [] } });
        return;
      }
      await route.fulfill({ status: 200, json: { items: [approval] } });
    });

    await page.goto('/approvals');
    const card = page.locator('.chat-approval', { hasText: '审批门限变更' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card).toContainText('策略：');
    await expect(card).toContainText('本审批只变更全局审批门限，不含可执行命令');
    // 第 8 轮走查 low：policy 审批没有目标服务器，公共头不得渲染
    // 「目标服务器：未知」（审批者会以为目标缺失/请求不完整），改为
    // 标明变更对象是全局门限。
    await expect(card).toContainText('全局审批门限变更');
    await expect(card).not.toContainText('目标服务器');
    await expect(card).not.toContainText('未知');
    // 修复前：恒渲染「待执行命令」标题 + 空 <pre>。
    await expect(card.getByText('待执行命令')).toHaveCount(0);
    await expect(card.locator('pre')).toHaveCount(0);
  });
});

test.describe('RBAC 第 2 轮：审计校验窗口文案', () => {
  test.use({ storageState: AUDITOR_STATE_FILE });

  test('校验结果说明覆盖区间与更早记录未校验', async ({ page }) => {
    // 造一条较早的审计之外的库状态不可控；直接伪造响应，锁死文案口径。
    await page.route('**/api/audit/verify', async (route) => {
      await route.fulfill({
        status: 200,
        json: {
          ok: true,
          checked: 200,
          first_id: 300,
          window: 200,
          legacy: 0,
          head_id: 499,
          head_hash: 'a'.repeat(64),
          broken_at: null,
        },
      });
    });

    await page.goto('/audit');
    await page.getByRole('button', { name: /校验哈希链/ }).click();
    const result = page.getByTestId('audit-verify-result');
    await expect(result).toBeVisible({ timeout: 20_000 });
    await expect(result).toContainText('已校验 200 条（#300–#499）');
    await expect(result).toContainText('更早记录（#1–#299）未在本次校验范围内');
    await expect(result).toContainText('链头 #499');
  });

  test('真实接口返回 first_id/window 两个字段', async ({ page }) => {
    const response = await page.request.get('/api/audit/verify');
    expect(response.status()).toBe(200);
    const body = (await response.json()) as {
      checked: number;
      first_id: number;
      window: number;
      head_id: number;
    };
    // 窗口上限恒为 200；空库时 checked=0 且 first_id=0（不臆造区间）。
    expect(body.window).toBe(200);
    expect(body.checked).toBeLessThanOrEqual(body.window);
    if (body.checked > 0) {
      expect(body.first_id).toBeGreaterThan(0);
      expect(body.first_id).toBeLessThanOrEqual(body.head_id);
    }
  });
});

test.describe('RBAC 第 2 轮：终端授权的发起人与审批者入口', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('批准后发起人 /term 出现「已批准」提示，审批者「最近已决策」保留该行', async ({
    browser,
  }) => {
    // 1) admin 建一台指向假 sshd 的服务器（operator 无 asset:write）；
    //    operator 自己对它申请 term 授权。
    const adminContext = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const adminPage = await adminContext.newPage();
    const serverName = `r2-grant-${Date.now()}`;
    const created = await adminPage.request.post('/api/servers', {
      headers: { Origin: ORIGIN },
      data: {
        name: serverName,
        host: '127.0.0.1',
        port: 18083,
        os: 'linux',
        auth_method: 'password',
        username: 'e2e',
        password: 'e2e-ssh-pass',
        auto_exec_enabled: false,
      },
    });
    expect(created.status(), await created.text()).toBe(201);
    const serverId = ((await created.json()) as { id: string }).id;

    const operatorContext = await browser.newContext({ storageState: OPERATOR_STATE_FILE });
    const operatorPage = await operatorContext.newPage();
    const grantCreated = await operatorPage.request.post('/api/term/grants', {
      headers: { Origin: ORIGIN },
      data: { server_id: serverId },
    });
    expect(grantCreated.status(), await grantCreated.text()).toBe(202);
    const grantId = ((await grantCreated.json()) as { approval: { id: string } }).approval.id;

    // 2) reviewer 批准。
    const reviewerContext = await browser.newContext({ storageState: REVIEWER_STATE_FILE });
    const reviewerPage = await reviewerContext.newPage();
    const decided = await reviewerPage.request.post(`/api/approvals/${grantId}/decide`, {
      headers: { Origin: ORIGIN },
      data: { decision: 'approve' },
    });
    expect(decided.status(), await decided.text()).toBe(200);

    try {
      // 3) 发起人侧：GET /api/term/grants 列出该授权，/term 顶部出现提示。
      await operatorPage.goto('/term');
      const available = operatorPage.getByTestId('term-available-grants');
      await expect(available).toBeVisible({ timeout: 20_000 });
      await expect(available).toContainText(/你有 \d+ 条已批准、尚未使用的终端授权/);
      await expect(available.getByRole('button', { name: '使用该授权' })).toBeVisible();

      // 4) 审批者侧：/approvals 的「最近已决策」仍能看到终端授权行与有效期。
      await reviewerPage.goto('/approvals');
      const decidedCard = reviewerPage.getByTestId('decided-approvals');
      await expect(decidedCard).toBeVisible({ timeout: 20_000 });
      const rows = decidedCard.locator('.ant-table-row');
      await expect(rows.filter({ hasText: '终端授权' }).first()).toBeVisible({ timeout: 20_000 });
      await expect(
        rows
          .filter({ hasText: serverName })
          .or(rows.filter({ hasText: '待发起人使用' }))
          .first(),
      ).toBeVisible({ timeout: 20_000 });
      await expect(decidedCard).toContainText('待发起人使用');
    } finally {
      // 清理：服务器由 admin 删除；审批行保留为审计事实，由库生命周期收尾。
      await adminPage.request.delete(`/api/servers/${serverId}`, {
        headers: { Origin: ORIGIN },
      });
      await adminContext.close();
      await operatorContext.close();
      await reviewerContext.close();
    }
  });
});

test.describe('RBAC 第 2 轮：会话列表归属过滤（SQL 侧）', () => {
  test.use({ storageState: OPERATOR_STATE_FILE });

  test('他人会话占满窗口时，自己的会话仍可见且可翻页', async ({ page, browser }) => {
    // 1) admin 造 12 条更新的会话；operator 造 1 条更旧的会话。
    const adminContext = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const adminPage = await adminContext.newPage();
    const created: string[] = [];
    for (let i = 0; i < 12; i++) {
      const res = await adminPage.request.post('/api/conversations', {
        headers: { Origin: ORIGIN },
        data: { title: `r2-window-admin-${Date.now()}-${i}` },
      });
      expect(res.status()).toBe(201);
      created.push(((await res.json()) as { id: string }).id);
    }
    const own = await page.request.post('/api/conversations', {
      headers: { Origin: ORIGIN },
      data: { title: `r2-window-own-${Date.now()}` },
    });
    expect(own.status()).toBe(201);
    const ownId = ((await own.json()) as { id: string }).id;

    // 2) 把 operator 的会话时间戳压到 admin 那些之后（直接改库，模拟「旧会话」）。
    psql(
      `UPDATE conversations SET last_message_at = now() - interval '2 hours' WHERE id='${ownId}'`,
    );

    try {
      // 3) API 取证：limit=1 必须返回一条自己的会话（不是空数组），且沿
      // 游标逐页推进能到达本用例造的那条（被压到 2 小时前）。修复前是
      // items=[] + has_more=true + next_cursor=null——窗口被他人占满，
      // 翻页根本无从开始。
      type Page = {
        items: Array<{ id: string }>;
        has_more: boolean;
        next_cursor: { last_message_at: string; id: string } | null;
      };
      const firstRes = await page.request.get('/api/conversations?limit=1');
      expect(firstRes.status()).toBe(200);
      let current = (await firstRes.json()) as Page;
      expect(current.items).toHaveLength(1);
      expect(current.has_more).toBe(current.next_cursor !== null);
      const visited = new Set<string>([current.items[0]!.id]);
      // 有界推进（防 OOM 铁律：循环必须有上限）：最多 20 页。
      for (let i = 0; i < 20 && current.next_cursor !== null && !visited.has(ownId); i++) {
        const q =
          `?limit=1&before_last_message_at=${encodeURIComponent(current.next_cursor.last_message_at)}` +
          `&before_id=${current.next_cursor.id}`;
        const res = await page.request.get(`/api/conversations${q}`);
        expect(res.status()).toBe(200);
        current = (await res.json()) as Page;
        if (current.items.length > 0) {
          visited.add(current.items[0]!.id);
        } else {
          break;
        }
      }
      expect(visited.has(ownId), `own conversation unreachable: ${[...visited].join(',')}`).toBe(
        true,
      );
      // 翻过的每一页都不得出现他人会话（修复前 Go 侧后过滤时会先返回
      // 他人行、再被剥成空数组；SQL 侧过滤后窗口里根本没有它们）。
      const leaked = [...visited].filter((id) => created.includes(id));
      expect(leaked, `foreign conversations leaked: ${leaked.join(',')}`).toEqual([]);

      // 4) 浏览器取证：侧栏显示自己的会话，不是「还没有会话」。
      await page.goto('/chat');
      await expect(
        page.locator('.chat-conv-item').filter({ hasText: 'r2-window-own' }),
      ).toHaveCount(1, { timeout: 20_000 });
      await expect(page.getByText('还没有会话')).toHaveCount(0);
    } finally {
      await Promise.all([
        ...created.map((id) =>
          adminPage.request.delete(`/api/conversations/${id}`, { headers: { Origin: ORIGIN } }),
        ),
        page.request.delete(`/api/conversations/${ownId}`, { headers: { Origin: ORIGIN } }),
      ]);
      await adminContext.close();
    }
  });

  test('回放目录同样只列自己的会话', async ({ page, browser }) => {
    // 并行用例会为 operator 产生其它会话，因此用本次唯一标题前缀 + `q`
    // 过滤把断言固定在本次数据上（同时避免与并行会话竞争 limit 窗口）。
    const stamp = Date.now();
    const adminPrefix = `r2-catalog-admin-${stamp}`;
    const ownPrefix = `r2-catalog-own-${stamp}`;
    const adminContext = await browser.newContext({ storageState: AUTH_STATE_FILE });
    const adminPage = await adminContext.newPage();
    const created: string[] = [];
    for (let i = 0; i < 6; i++) {
      const res = await adminPage.request.post('/api/conversations', {
        headers: { Origin: ORIGIN },
        data: { title: `${adminPrefix}-${i}` },
      });
      expect(res.status()).toBe(201);
      created.push(((await res.json()) as { id: string }).id);
    }
    const own = await page.request.post('/api/conversations', {
      headers: { Origin: ORIGIN },
      data: { title: `${ownPrefix}` },
    });
    const ownId = ((await own.json()) as { id: string }).id;

    try {
      // 6 条标题命中 `q` 但都属于他人：修复前 store 先按 limit 取到其中
      // 一条、Go 侧再过滤成空数组，返回 items=[] + has_more=true（游标
      // 无从推进）；修复后 SQL 内强制 user_id，窗口里根本没有他人行。
      const adminOnly = await page.request.get(`/api/replays?limit=1&q=${adminPrefix}`);
      expect(adminOnly.status()).toBe(200);
      const adminBody = (await adminOnly.json()) as {
        items: Array<{ conversation_id: string }>;
        has_more: boolean;
      };
      expect(adminBody.items).toEqual([]);
      expect(adminBody.has_more).toBe(false);

      // 自己的会话仍可见（不是被他人会话挤空）。
      const ownRes = await page.request.get(`/api/replays?limit=1&q=${ownPrefix}`);
      expect(ownRes.status()).toBe(200);
      const ownBody = (await ownRes.json()) as {
        items: Array<{ conversation_id: string }>;
        has_more: boolean;
      };
      expect(ownBody.items.map((item) => item.conversation_id)).toEqual([ownId]);
      expect(ownBody.has_more).toBe(false);

      // 目录的 q 是页面内搜索（不走 URL），按 aria-label 填关键词；
      // 300ms 防抖后发一次 /api/replays?q=…
      await page.goto('/replays');
      const catalog = page.getByRole('group', { name: '可回放会话' });
      await page.getByLabel('搜索回放会话').fill(ownPrefix);
      await expect(catalog.getByText(ownPrefix, { exact: false })).toHaveCount(1, {
        timeout: 20_000,
      });
      await page.getByLabel('搜索回放会话').fill(adminPrefix);
      await expect(page.getByText('暂无可回放的会话')).toHaveCount(0, { timeout: 20_000 });
      await expect(page.getByText('没有符合条件的历史会话')).toBeVisible({ timeout: 20_000 });
    } finally {
      await Promise.all([
        ...created.map((id) =>
          adminPage.request.delete(`/api/conversations/${id}`, { headers: { Origin: ORIGIN } }),
        ),
        page.request.delete(`/api/conversations/${ownId}`, { headers: { Origin: ORIGIN } }),
      ]);
      await adminContext.close();
    }
  });
});
