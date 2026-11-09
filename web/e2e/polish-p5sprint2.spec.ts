import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { expect, test } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  DIAGNOSIS_FINAL_PREFIX,
  DIAGNOSIS_FINAL_SUFFIX,
  SCENARIO_DIAGNOSIS,
  sendMessage,
} from './helpers';

// 五期打磨 sprint 2 的两条防回归 E2E（真链路）：
// 1) 告警引擎装配回归（走查 #1）：monitor.Manager 曾从未在 cmd/aiops
//    装配——采集/评估/通知/保留四个后台循环在生产二进制中不存在，监控页
//    永远无数据、告警永不评估，而 UI 仍显示「采集器：运行中」。本用例从
//    服务外部只能观测「越限数据落库后 firing 事件是否出现」：创建
//    cpu_pct>85 / duration_windows=1 规则 + psql 直插越限点与 latest 行，
//    评估循环（tick 30s）必须产出 firing 事件——引擎缺失时事件永不出现。
// 2) 诊断成功路径（走查 #2）：fake-llm 此前对诊断引擎的协议请求回默认
//    流式文本，会话跑满 6 轮落 failed(DIAGNOSIS_MAX_ROUNDS)，而工具卡标
//    「成功」、文案固定宣称「报告与修复建议已生成」。现在 fake-llm 按轮
//    次回 probe/report 协议 JSON（会话真实到达 awaiting_fix），最终文案按
//    run_diagnosis 结果的 status 动态生成；断言深链抽屉出现报告与修复
//    审批入口——五期 AI 诊断的成功路径（报告+修复命令审批）首次有覆盖。

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, '..', '..');
const E2E_DATABASE = 'aiops_e2e';
// 后端对 /api 写请求做 Origin 精确校验（page.request 不自动带 Origin，
// 须显式携带 vite preview 的源——与 global-setup 的 api() 帮助函数同款）。
const API_HEADERS = { Origin: 'http://localhost:4173' };

/** 经 docker compose 直连 e2e 库执行 SQL（与 global-setup 同一 db 服务）。 */
function psql(sql: string): void {
  execFileSync(
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
      '-c',
      sql,
    ],
    { cwd: repoRoot, stdio: ['ignore', 'pipe', 'pipe'] },
  );
}

test.use({ storageState: AUTH_STATE_FILE });

test('告警评估引擎已装配：越限数据落库后 firing 事件自动出现', async ({ page }) => {
  // seeded e2e-fake-host（127.0.0.1:1 不可达）——取真实 server id。
  const serversRes = await page.request.get('/api/servers', { headers: API_HEADERS });
  expect(serversRes.ok()).toBeTruthy();
  const servers = ((await serversRes.json()) ?? { items: [] }).items ?? [];
  expect(servers.length).toBeGreaterThan(0);
  const serverID: string = servers[0].id;

  // 规则：cpu_pct > 85、单窗口即触发（评估走「缓冲未命中回退查库」路径
  // ——目标从未被成功采集过，环形缓冲无该台）。
  const ruleName = `e2e-engine-${Date.now()}`;
  const ruleRes = await page.request.post('/api/alert-rules', {
    headers: API_HEADERS,
    data: {
      name: ruleName,
      metric: 'cpu_pct',
      comparator: 'gt',
      threshold: 85,
      duration_windows: 1,
      cooldown_seconds: 60,
      severity: 'warning',
      enabled: true,
    },
  });
  expect(ruleRes.status()).toBe(201);

  // 直插一个越限点 + UPSERT latest（评估目标来自 server_metrics_latest；
  // 与采集器 BatchInsert 的同事务 UPSERT 语义一致）。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    VALUES ('${serverID}', now(), 95, 40, 50, 0.5, 0, 0);
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${serverID}', now(), 95, 40, 50, 0.5, 0, 0, now())
    ON CONFLICT (server_id) DO UPDATE SET
      created_at = EXCLUDED.created_at, cpu_pct = EXCLUDED.cpu_pct,
      mem_pct = EXCLUDED.mem_pct, disk_pct = EXCLUDED.disk_pct,
      load_1 = EXCLUDED.load_1, net_in_bps = EXCLUDED.net_in_bps,
      net_out_bps = EXCLUDED.net_out_bps, updated_at = EXCLUDED.updated_at;
  `);

  // 评估循环 tick 30s（另有采集轮后追加评估）：轮询至多 75s 等 firing。
  // 引擎未装配（回归）时本断言必然超时——这是走查 #1 的防回归闸。
  let fired: unknown = null;
  const deadline = Date.now() + 75_000;
  while (Date.now() < deadline) {
    const eventsRes = await page.request.get('/api/alert-events?status=firing&limit=50', {
      headers: API_HEADERS,
    });
    expect(eventsRes.ok()).toBeTruthy();
    const events = ((await eventsRes.json()) ?? { items: [] }).items ?? [];
    fired = events.find(
      (event: { rule_name?: string; metric?: string; message?: string }) =>
        event.rule_name === ruleName && event.metric === 'cpu_pct',
    );
    if (fired) {
      break;
    }
    await page.waitForTimeout(3_000);
  }
  expect(fired, '评估循环应在 75s 内对越限点产出 firing 事件（monitor.Manager 必须在服务入口装配）').toBeTruthy();
});

test('__scenario_diagnosis__：诊断会话到达 awaiting_fix，深链抽屉出现报告与修复审批', async ({
  page,
}) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard).toBeVisible();
  // 引擎两轮协议调用（probe → report）+ 轮询至终态，给足窗口。
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });

  // 最终文案按 run_diagnosis 结果的 status 动态生成：成功路径宣称报告
  // 可审批（不再是无视 status 的固定「报告与修复建议已生成」）。
  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText('诊断已产出报告与修复建议')).toBeVisible();
  await expect(page.getByText(DIAGNOSIS_FINAL_SUFFIX)).toBeVisible();

  // 深链新标签打开诊断抽屉（与 sprint 1 用例同入口）。
  const deepLink = diagCard.getByRole('link', { name: /查看诊断会话/ });
  await expect(deepLink).toBeVisible();
  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await deepLink.click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/alerts\?diagnosis=[0-9a-f-]{36}/, { timeout: 20_000 });

  const drawer = popup.getByRole('dialog');
  // 会话终态是 awaiting_fix（修复建议待处理），不是 failed(DIAGNOSIS_
  // MAX_ROUNDS)——诊断成功路径的核心断言。
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 30_000 });
  // 报告本体可见：summary 摘要 + 修复建议区。
  await expect(drawer.getByText('SSH 端口不可达', { exact: false })).toBeVisible();
  await expect(drawer.getByText('修复建议')).toBeVisible();

  // 发起修复审批：「执行（需审批）」创建 console 审批后出现批准/拒绝
  // 按钮（报告 → 修复命令审批的完整入口，此前成功路径零覆盖）。
  await drawer.getByRole('button', { name: /执行（需审批）/ }).first().click();
  await expect(drawer.getByRole('button', { name: /批准执行/ })).toBeVisible({ timeout: 15_000 });
});
