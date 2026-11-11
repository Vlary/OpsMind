import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { expect, test, type Page } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  BATCH_FINAL_PREFIX,
  DIAGNOSIS_FINAL_PREFIX,
  METRICS_FINAL_PREFIX,
  ALERTS_FINAL_PREFIX,
  SCENARIO_ALERTS,
  SCENARIO_BATCH,
  SCENARIO_DIAGNOSIS,
  SCENARIO_METRICS,
  sendMessage,
} from './helpers';

// 五期第 6 轮冲刺激收走查（round 6）：真链路逐页操作 + 全程 console 收集。
// 与 round4/round5 互补的增量覆盖（已修项不重复报）：
// - /alerts 规则表单边界：空名必填、阈值越界钳回、重名 409 错误展示；
// - /alerts 事件行「AI 诊断」入口跳 Tab、诊断全链路（fix 审批→批准→完成）复走；
// - /monitor 双服务器切换、firing 概要分色、巡检均分联动、1440 点截断提示；
// - /inspections cron 非法前端校验、无匹配 tag 任务的手动触发行为、
//   立即巡检同步报告、任务编辑/删除；
// - /batch 禁止级命令风险预览、双机创建→逐台批准→partial 2/2→重试预填；
// - 对话四条工具链全走（metrics/alerts/batch 深链批准/diagnosis 深链报告）。
// 本 spec 是走查脚本：断言用 expect.soft，失败不中断；失败点与 console
// error 均作为走查发现记录。

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, '..', '..');
const E2E_DATABASE = 'aiops_e2e';
const API_HEADERS = { Origin: 'http://localhost:4173' };

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

/** 收集 console error 与 pageerror（网络 4xx/5xx 的 error 也计入）。 */
function watchConsole(page: Page): { errors: string[]; pageErrors: string[] } {
  const errors: string[] = [];
  const pageErrors: string[] = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      errors.push(msg.text());
    }
  });
  page.on('pageerror', (err) => {
    pageErrors.push(String(err));
  });
  return { errors, pageErrors };
}

/** 断言本轮 console 干净（soft：失败记为走查发现，不中断）。
 * tolerate：走查主动制造的预期 4xx 业务响应（浏览器网络层必然打
 * console error，非产品缺陷），按子串豁免。 */
function expectCleanConsole(
  watched: { errors: string[]; pageErrors: string[] },
  label: string,
  tolerate: string[] = [],
): void {
  const errors = watched.errors.filter((text) => !tolerate.some((needle) => text.includes(needle)));
  expect
    .soft(watched.pageErrors, `${label} pageerror：${watched.pageErrors.join(' | ')}`)
    .toEqual([]);
  expect.soft(errors, `${label} console error：${errors.join(' | ')}`).toEqual([]);
}

async function serverIds(page: Page): Promise<Array<{ id: string; name: string }>> {
  const res = await page.request.get('/api/servers', { headers: API_HEADERS });
  expect(res.ok()).toBeTruthy();
  const servers = (((await res.json()) ?? { items: [] }).items ?? []) as Array<{
    id: string;
    name: string;
  }>;
  expect(servers.length).toBeGreaterThan(0);
  return servers;
}

test.use({ storageState: AUTH_STATE_FILE });

// 走查脚本：不用 serial——soft 失败（走查发现）不应阻断后续页面的走查；
// config fullyParallel=false 保证本文件内仍按声明顺序执行（batch 用例
// 依赖 monitor 用例创建的第二台服务器，均在各自断言前完成数据准备）。

test('alerts：规则表单边界——必填/阈值钳回/重名 409 展示/编辑/删除 + 事件行诊断入口', async ({
  page,
}) => {
  const watched = watchConsole(page);
  const ruleName = `走查r6规则-${Date.now()}`;

  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();

  // ① 空名称提交 → 前端必填校验。
  await page.getByRole('button', { name: /新建规则/ }).click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('新建告警规则')).toBeVisible();
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(drawer.getByText('请输入规则名称')).toBeVisible();

  // ② 阈值越界：输入 150（cpu_pct 上限 100）→ blur 钳回。
  const thresholdBox = drawer.getByRole('spinbutton', { name: /阈值/ });
  await thresholdBox.fill('150');
  await drawer.getByLabel('规则名称').click();
  const clamped = await thresholdBox.inputValue();
  console.log(`[round6] threshold after blur (input 150): ${clamped}`);
  expect.soft(Number(clamped), 'cpu_pct 阈值 150 blur 后应钳回 100').toBeLessThanOrEqual(100);

  // ③ 正常创建（阈值 90，warning）。
  await drawer.getByLabel('规则名称').fill(ruleName);
  await thresholdBox.fill('90');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已创建' })).toBeVisible();
  const ruleRow = page.getByRole('row', { name: new RegExp(ruleName) });
  await expect(ruleRow).toBeVisible({ timeout: 15_000 });

  // ④ 重名创建 → 后端 409 → 抽屉内错误展示（走查点：是否中文可读）。
  await page.getByRole('button', { name: /新建规则/ }).click();
  await expect(drawer.getByText('新建告警规则')).toBeVisible();
  await drawer.getByLabel('规则名称').fill(ruleName);
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  const errorAlert = drawer.locator('.ant-alert-error, .ant-form-item-explain-error').first();
  await expect(errorAlert).toBeVisible({ timeout: 15_000 });
  const conflictText = (await errorAlert.textContent()) ?? '';
  console.log(`[round6] duplicate rule error text: ${conflictText}`);
  // p5 sprint 6 修复锁定：重名 409 的 message 必须点名「规则名称已存在」
  // （后端对齐 servers「服务器名称已存在」口径；此前走查实测为泛化
  // 「资源冲突或当前状态不允许该操作（resource conflict）CONFLICT …」）。
  expect.soft(conflictText, '重名错误应点名「规则名称已存在」').toContain('规则名称已存在');
  expect.soft(conflictText.length, '重名错误应有可读文案（非空）').toBeGreaterThan(3);
  // 关闭重名抽屉（精确点抽屉 X——错误 Alert 也有 aria-label=Close 的按钮，
  // 且关掉 Alert 后抽屉仍在；点 .ant-drawer-close 并等标题隐藏）。
  await drawer.locator('.ant-drawer-close').click();
  await expect(drawer.getByText('新建告警规则')).toBeHidden({ timeout: 5_000 });

  // ⑤ 编辑：阈值改 85 → 保存。
  await ruleRow.getByRole('button', { name: `编辑规则 ${ruleName}` }).click();
  await expect(drawer.getByText('编辑告警规则')).toBeVisible();
  await drawer.getByRole('spinbutton', { name: /阈值/ }).fill('85');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已更新' })).toBeVisible();
  await expect(ruleRow.getByText(/CPU 使用率 > 85/)).toBeVisible({ timeout: 15_000 });

  // ⑥ 插 firing 事件 → 事件 Tab 行可见 + 「AI 诊断」入口跳诊断 Tab。
  psql(`
    INSERT INTO alert_events (rule_id, server_id, status, severity, value, message, fired_at)
    SELECT r.id, s.id, 'firing', 'warning', 95.5, '走查r6触发：CPU 95.5% 持续越限', now()
    FROM alert_rules r, servers s
    WHERE r.name = '${ruleName}' AND s.name = 'e2e-fake-host';
  `);
  await page.getByRole('tab', { name: '事件' }).click();
  const eventRow = page.getByRole('row', { name: new RegExp(ruleName) });
  await expect(eventRow).toBeVisible({ timeout: 20_000 });
  await expect(eventRow.getByText('触发中')).toBeVisible();
  await expect(eventRow.getByText('CPU 使用率 = 95.5')).toBeVisible();
  await eventRow.getByRole('button', { name: 'AI 诊断' }).click();
  // 入口语义：切到 AI 诊断 Tab（诊断目标下拉可见）。
  await expect(page.getByRole('combobox', { name: '诊断目标服务器' })).toBeVisible({
    timeout: 10_000,
  });

  // ⑦ 删除规则（级联删事件）。
  await page.getByRole('tab', { name: '规则' }).click();
  await ruleRow.getByRole('button', { name: `删除规则 ${ruleName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已删除' })).toBeVisible();
  await expect(ruleRow).toHaveCount(0, { timeout: 15_000 });

  expectCleanConsole(watched, 'alerts rules', ['409 (Conflict)']);
});

test('alerts：诊断全链路——发起→awaiting_fix→执行（需审批）→批准→执行结果→标记完成', async ({
  page,
}) => {
  const watched = watchConsole(page);
  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();
  await page.getByRole('tab', { name: 'AI 诊断' }).click();
  await page.getByRole('combobox', { name: '诊断目标服务器' }).click();
  await page
    .locator(
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="e2e-fake-host"]',
    )
    .click();
  await page.getByRole('button', { name: /发起 AI 诊断/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '诊断已发起' })).toBeVisible();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 90_000 });
  await expect(drawer.getByText('修复建议')).toBeVisible();

  await drawer.getByRole('button', { name: '执行（需审批）' }).first().click();
  await expect(
    page.locator('.ant-message-success', { hasText: '已创建修复审批，请批准或拒绝' }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(drawer.getByText('待审批的修复命令')).toBeVisible({ timeout: 15_000 });

  const decideCalls: Array<{ status: number; body: unknown }> = [];
  page.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      decideCalls.push({
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });

  await drawer.getByRole('button', { name: '批准执行' }).click();
  await expect.poll(() => decideCalls.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect.soft(decideCalls[0]?.status, '诊断 fix decide 应 200').toBe(200);
  const toast = page.locator('.ant-message-warning').first();
  await expect(toast).toBeVisible({ timeout: 15_000 });
  const toastText = (await toast.textContent()) ?? '';
  console.log(`[round6] diagnosis fix toast: ${toastText}`);
  console.log(
    `[round6] diagnosis fix execution: ${JSON.stringify((decideCalls[0]?.body as { execution?: unknown })?.execution)}`,
  );
  expect.soft(toastText, '执行结果 toast 应含失败语义').toContain('修复命令执行失败');
  expect.soft(/undefined/.test(toastText), 'toast 不应拼出 undefined').toBe(false);
  await expect(drawer.getByText('待审批的修复命令')).toHaveCount(0, { timeout: 15_000 });

  await drawer.getByRole('button', { name: /标记处理完成/ }).click();
  await expect(
    page.locator('.ant-message-success', { hasText: '诊断会话已标记完成' }),
  ).toBeVisible();
  await expect(drawer.getByText('已完成')).toBeVisible({ timeout: 15_000 });
  await drawer.getByRole('button', { name: /关\s*闭/ }).click();

  expectCleanConsole(watched, 'alerts diagnosis');
});

test('monitor：双机切换 + firing 概要分色 + 巡检均分联动 + 1440 点截断提示', async ({ page }) => {
  const watched = watchConsole(page);

  // 第二台服务器（监控选择器要两台 latest 才可切换）。
  const createRes = await page.request.post('/api/servers', {
    headers: { ...API_HEADERS, 'Content-Type': 'application/json' },
    data: {
      name: 'e2e-fake-host-2',
      host: '127.0.0.1',
      port: 2,
      username: 'root',
      auth_method: 'password',
      password: 'not-a-real-password',
      tags: ['e2e'],
      auto_exec_enabled: false,
    },
  });
  expect(createRes.ok()).toBeTruthy();
  const servers = await serverIds(page);
  const host1 = servers.find((s) => s.name === 'e2e-fake-host');
  const host2 = servers.find((s) => s.name === 'e2e-fake-host-2');
  expect(host1 && host2).toBeTruthy();

  // 两台 latest（不同 cpu 值以便分辨切换）+ 1h 内序列点。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    SELECT '${host1!.id}', now() - (n * interval '5 minutes'), 12.5 + n * 0.5, 30, 40, 0.4, 1024, 2048
    FROM generate_series(1, 6) AS n;
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    SELECT '${host2!.id}', now() - (n * interval '5 minutes'), 77.7 - n * 0.5, 50, 60, 0.9, 4096, 8192
    FROM generate_series(1, 6) AS n;
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${host1!.id}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048, now()),
           ('${host2!.id}', now() - interval '5 minutes', 77.7, 50, 60, 0.9, 4096, 8192, now())
    ON CONFLICT (server_id) DO UPDATE SET
      created_at = EXCLUDED.created_at, cpu_pct = EXCLUDED.cpu_pct,
      mem_pct = EXCLUDED.mem_pct, disk_pct = EXCLUDED.disk_pct,
      load_1 = EXCLUDED.load_1, net_in_bps = EXCLUDED.net_in_bps,
      net_out_bps = EXCLUDED.net_out_bps, updated_at = EXCLUDED.updated_at;
  `);

  // firing 概要联动：建规则（load_1 阈值 999 永不触发，避免评估器误报）
  // + 插 firing 事件（severity=critical 验证分色 Tag）。
  const ruleRes = await page.request.post('/api/alert-rules', {
    headers: { ...API_HEADERS, 'Content-Type': 'application/json' },
    data: {
      name: `走查r6监控概要-${Date.now()}`,
      metric: 'load_1',
      comparator: 'gt',
      threshold: 999,
      duration_windows: 3,
      cooldown_seconds: 300,
      severity: 'warning',
      enabled: true,
    },
  });
  expect(ruleRes.ok()).toBeTruthy();
  const rule = await ruleRes.json();
  psql(`
    INSERT INTO alert_events (rule_id, server_id, status, severity, value, message, fired_at)
    VALUES ('${rule.id}', '${host1!.id}', 'firing', 'critical', 98.5, '走查r6：概要分色', now());
  `);

  await page.goto('/monitor');
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  // 两台已采集 + firing 概要（严重 1）。
  const serverStat = page.locator('.ant-statistic', { hasText: '服务器（已采集）' });
  await expect(serverStat).toBeVisible();
  await expect
    .poll(async () => (await serverStat.textContent()) ?? '', { timeout: 20_000 })
    .toContain('2');
  await expect(page.locator('.ant-tag', { hasText: '严重 1' })).toBeVisible({ timeout: 20_000 });

  // 切换到第二台 → 概要值跟随（当前 77.7%）。
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  await page
    .locator(
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="e2e-fake-host-2"]',
    )
    .click();
  await expect(page.getByText('当前 77.7%').first()).toBeVisible({ timeout: 20_000 });

  // 时间窗 24h：请求 minutes=1440。
  const minutesSeen: number[] = [];
  page.on('request', (req) => {
    const m = /\/api\/servers\/[^/]+\/metrics\?minutes=(\d+)/.exec(req.url());
    if (m) {
      minutesSeen.push(Number(m[1]));
    }
  });
  await page.locator('.ant-segmented-item', { hasText: '24 小时' }).click();
  await expect
    .poll(() => minutesSeen.filter((m) => m === 1440).length, { timeout: 20_000 })
    .toBeGreaterThan(0);

  // 截断提示：host1 插 1500 点（57s 间隔 ≈ 23.75h 全在 24h 窗内）→
  // Window LIMIT 1440 → truncated=true → 前端「返回点集已截断（1440 点）」。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    SELECT '${host1!.id}', now() - (n * interval '57 seconds'), 20 + (n % 30), 40, 50, 0.5, 2048, 4096
    FROM generate_series(1, 1500) AS n;
  `);
  await page.reload();
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  // reload 后 react state 重置回落第一台：显式切回 host1（1500 点所在）
  // 再看 24h 窗口的截断提示。
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  await page
    .locator(
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="e2e-fake-host"]',
    )
    .click();
  await page.locator('.ant-segmented-item', { hasText: '24 小时' }).click();
  await expect(page.getByText(/返回点集已截断/).first()).toBeVisible({ timeout: 20_000 });

  // 巡检均分联动：立即巡检 host2（同步返回）→ 概要「最近巡检均分」非 —。
  const runRes = await page.request.post('/api/inspection/run', {
    headers: { ...API_HEADERS, 'Content-Type': 'application/json' },
    data: { server_ids: [host2!.id], template: 'basic' },
  });
  expect(runRes.ok()).toBeTruthy();
  const runBody = await runRes.json();
  console.log(`[round6] inspection run avg_score: ${runBody?.report?.avg_score}`);
  // p5 sprint 6 修复锁定（API 层硬断言）：overview.last_inspection 必须装配
  // 刚完成巡检的均分（此前后端从未实现该字段，概要卡恒显 —）。
  const overviewRes = await page.request.get('/api/monitor/overview', { headers: API_HEADERS });
  expect(overviewRes.ok()).toBeTruthy();
  const overviewBody = (await overviewRes.json()) as { last_inspection?: { avg_score?: number } };
  expect(
    overviewBody.last_inspection?.avg_score,
    'overview.last_inspection.avg_score 应等于刚完成巡检的均分',
  ).toBe(runBody?.report?.avg_score);
  await page.reload();
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  const scoreStat = page.locator('.ant-statistic', { hasText: '最近巡检均分' });
  await expect(scoreStat).toBeVisible({ timeout: 20_000 });
  const scoreText = (await scoreStat.textContent()) ?? '';
  console.log(`[round6] monitor last_inspection stat: ${scoreText}`);
  expect.soft(scoreText.includes('—'), '巡检已完成后均分不应仍显示 —').toBe(false);

  expectCleanConsole(watched, 'monitor');
});

test('inspections：cron 非法校验 + 无匹配 tag 任务触发 + 立即巡检报告 + 编辑/删除', async ({
  page,
}) => {
  const watched = watchConsole(page);
  const jobName = `走查r6巡检-${Date.now()}`;

  await page.goto('/inspections');
  await expect(page.getByRole('heading', { name: '巡检' })).toBeVisible();

  // ① cron 非法：前端校验「应为五段表达式」。
  await page.getByRole('button', { name: /新建任务/ }).click();
  const formDrawer = page.getByRole('dialog');
  await formDrawer.getByLabel('任务名称').fill(jobName);
  await formDrawer.getByLabel('cron 表达式（UTC）').fill('abc def');
  await formDrawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(formDrawer.getByText('应为五段表达式（分 时 日 月 周）')).toBeVisible();

  // ② target_tags 填不存在的 tag → 任务保存成功（Select tags 模式的
  // 输入框 id 即 Form.Item name，placeholder 走 .ant-select 不会挂 input）。
  await formDrawer.getByLabel('cron 表达式（UTC）').fill('0 8 * * *');
  const tagBox = formDrawer.locator('#target_tags');
  await tagBox.click();
  await tagBox.pressSequentially('no-such-tag-r6');
  await page.keyboard.press('Enter');
  await formDrawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '任务已创建' })).toBeVisible();
  const jobRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(jobRow).toBeVisible({ timeout: 15_000 });

  // ③ 手动触发无匹配任务 → 报告 Tab 出报告（走查点：0 目标报告形态）。
  await jobRow.getByRole('button', { name: `立即运行 ${jobName}` }).click();
  await expect(page.locator('.ant-message', { hasText: '已触发巡检' })).toBeVisible();
  await page.getByRole('tab', { name: '报告' }).click();
  const reportRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(reportRow).toBeVisible({ timeout: 60_000 });
  const reportRowText = (await reportRow.textContent()) ?? '';
  console.log(`[round6] no-target job report row: ${reportRowText}`);
  await expect(reportRow.getByText('已完成')).toBeVisible({ timeout: 60_000 });
  await reportRow.getByRole('button', { name: /详\s*情/ }).click();
  const reportDrawer = page.getByRole('dialog');
  await expect(reportDrawer.getByText('巡检报告')).toBeVisible();
  const reportBodyText = (await reportDrawer.textContent()) ?? '';
  console.log(`[round6] no-target report drawer (first 500): ${reportBodyText.slice(0, 500)}`);
  await reportDrawer.getByRole('button', { name: '关闭', exact: true }).click();

  // ④ 立即巡检（RunNowModal）：同步报告详情。
  await page.getByRole('tab', { name: '任务' }).click();
  await page.getByRole('button', { name: '立即巡检' }).click();
  const modal = page.getByRole('dialog');
  await modal.getByRole('combobox', { name: '巡检目标服务器' }).click();
  await page
    .locator(
      '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="e2e-fake-host"]',
    )
    .click();
  await page.keyboard.press('Escape');
  await modal.getByRole('button', { name: /开始巡检/ }).click();
  await expect(page.locator('.ant-message', { hasText: /巡检完成：平均分/ })).toBeVisible({
    timeout: 60_000,
  });
  await expect(page.getByRole('dialog').getByText('巡检报告')).toBeVisible({ timeout: 15_000 });
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('e2e-fake-host')).toBeVisible({ timeout: 15_000 });
  await expect(drawer.getByText(/巡检目标不可达/).first()).toBeVisible({ timeout: 15_000 });
  // 六期打磨 sprint 1（契约 8.7 ⑧）：报告明细的「命令」列——模板展开后的
  // 命令清单可复制（此前明细只有 raw_output，报告抽屉无命令可复制）。
  // 通道不可达时约定仍然记录模板命令（命令清单在执行前装配）。
  await expect(drawer.getByLabel('复制命令').first()).toBeVisible({ timeout: 10_000 });
  await drawer.locator('.ant-table-row-expand-icon').first().click();
  await expect(drawer.getByText(/connection refused/i).first()).toBeVisible({ timeout: 10_000 });
  await drawer.getByRole('button', { name: '关闭', exact: true }).click();

  // ⑤ 编辑任务改名。
  await jobRow.getByRole('button', { name: `编辑任务 ${jobName}` }).click();
  await formDrawer.getByLabel('任务名称').fill(`${jobName}-v2`);
  await formDrawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '任务已更新' })).toBeVisible();
  await expect(page.getByRole('row', { name: new RegExp(`${jobName}-v2`) })).toBeVisible({
    timeout: 15_000,
  });

  // ⑥ 删除任务（报告级联）。
  const v2Row = page.getByRole('row', { name: new RegExp(`${jobName}-v2`) });
  await v2Row.getByRole('button', { name: `删除任务 ${jobName}-v2` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '任务已删除' })).toBeVisible();
  await expect(v2Row).toHaveCount(0, { timeout: 15_000 });

  expectCleanConsole(watched, 'inspections');
});

test('batch：禁止级命令预览 + 双机创建→逐台批准→partial 2/2→重试预填', async ({ page }) => {
  const watched = watchConsole(page);
  await page.goto('/batch');
  await expect(page.getByRole('heading', { name: '批量', exact: true })).toBeVisible();

  // ① 禁止级命令（mkfs 直击块设备，risk 分册 forbidden）：红色「禁止」预览。
  const commandBox = page.getByPlaceholder('df -h /');
  await commandBox.fill('mkfs.ext4 /dev/sda1');
  await expect(page.getByText('命令被风险规则禁止：全部目标将跳过')).toBeVisible({
    timeout: 15_000,
  });

  // ①b 需审批级（rm -rf /tmp/… 在分册中为 needs_approval）：黄色提示。
  await commandBox.fill('rm -rf /tmp/walkthrough-r6');
  await expect(page.getByText('每台目标将创建审批，逐台批准后执行')).toBeVisible({
    timeout: 15_000,
  });

  // ② 改安全命令 + 双机目标 → 创建。
  await commandBox.fill('uptime');
  await expect(page.getByText('全部目标将直接执行').first()).toBeVisible({ timeout: 15_000 });
  await page.getByLabel('自然语言描述（可选）').fill('走查r6双机批量');
  await page.getByRole('combobox', { name: '目标服务器' }).click();
  for (const name of ['e2e-fake-host', 'e2e-fake-host-2']) {
    // 全量链式跑后库内服务器几十台：antd Select 虚拟滚动只渲染可视项，
    // 排序靠后的 e2e-fake-host-2 不在 DOM 里，直接 locator 选项会超时
    // （实测 error-context 快照 listbox 仅 2 项）。用 showSearch 输入过滤
    // 把目标项筛到可视区再点（multiple Select 选中后搜索词自动清空，
    // 可继续输入下一台）。
    await page.keyboard.type(name);
    await page
      .locator(
        `.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option[title="${name}"]`,
      )
      .click();
  }
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '创建并分派' }).click();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  // 两台均待审批（auto_exec=false）。
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('待审批').nth(1)).toBeVisible({ timeout: 15_000 });

  const decideCalls: Array<{ status: number; body: unknown }> = [];
  page.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      decideCalls.push({
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });

  // ③ 逐台批准两行（按「批准」按钮数量推进——「待审批」文本在行 Tag 与
  // 摘要计数行各出现一次，不能作为行状态信号）。
  for (let i = 0; i < 2; i += 1) {
    await expect
      .poll(async () => await drawer.getByRole('button', { name: /^批\s*准$/ }).count(), {
        timeout: 20_000,
      })
      .toBeGreaterThanOrEqual(2 - i);
    await drawer
      .getByRole('button', { name: /^批\s*准$/ })
      .first()
      .click();
    await expect.poll(() => decideCalls.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(i + 1);
    const toast = page.locator('.ant-message-warning').first();
    await expect(toast).toBeVisible({ timeout: 15_000 });
    const toastText = (await toast.textContent()) ?? '';
    console.log(`[round6] batch decide #${i + 1} toast: ${toastText}`);
    console.log(
      `[round6] batch decide #${i + 1} execution: ${JSON.stringify((decideCalls[decideCalls.length - 1]?.body as { execution?: unknown })?.execution)}`,
    );
    expect.soft(/undefined/.test(toastText), 'toast 不应拼出 undefined').toBe(false);
    // 等该行离开待审批（按钮数量减少）。
    await expect
      .poll(async () => await drawer.getByRole('button', { name: /^批\s*准$/ }).count(), {
        timeout: 20_000,
      })
      .toBeLessThanOrEqual(1 - i);
  }
  expect.soft(decideCalls.length, '两台各一次 decide').toBe(2);
  expect
    .soft(
      decideCalls.every((c) => c.status === 200),
      'decide 均应 200',
    )
    .toBe(true);

  // ④ 两行终态执行失败 + 错误列中文。
  await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('远程命令执行失败').first()).toBeVisible({ timeout: 30_000 });

  // ⑤ 关抽屉 → 列表 partial 2/2。
  await drawer.getByRole('button', { name: /关\s*闭/ }).click();
  const jobRow = page.getByRole('row', { name: /走查r6双机批量/ });
  await expect(jobRow.getByText('部分完成')).toBeVisible({ timeout: 20_000 });
  // 进度列按单元格精确匹配：getByText('2/2') 是子串匹配，会命中日期
  // （如 2026/12/2x）触发 strict mode 冲突（同 round5 门禁实测）。
  await expect(jobRow.getByRole('cell', { name: '2/2', exact: true })).toBeVisible();

  // ⑥ 重试失败项（2 台）→ 预填。
  await jobRow.getByRole('button', { name: /详\s*情/ }).click();
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 15_000 });
  await drawer.getByRole('button', { name: /重试失败项（2 台）/ }).click();
  await expect(commandBox).toHaveValue('uptime', { timeout: 10_000 });
  await expect(page.getByText(/已用失败的 2 台预填新建表单/)).toBeVisible();

  expectCleanConsole(watched, 'batch');
});

test('chat 四链全走：metrics→alerts→batch(深链批准)→diagnosis(深链报告)', async ({ page }) => {
  const watched = watchConsole(page);

  // ① metrics：get_metrics 工具卡成功 + 最终文案。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_METRICS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const metricsCard = page.locator('.chat-toolcall-card', { hasText: 'get_metrics' }).first();
  await expect(metricsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(METRICS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  // ② alerts：list_alerts 工具卡成功 + 最终文案。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_ALERTS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const alertsCard = page.locator('.chat-toolcall-card', { hasText: 'list_alerts' }).first();
  await expect(alertsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(ALERTS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  // ③ batch：batch_execute 卡（待审批 1）→ 深链新标签 → 批准。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_BATCH);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const batchCard = page.locator('.chat-toolcall-card', { hasText: 'batch_execute' }).first();
  await expect(batchCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(batchCard.getByText('待审批 1')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(BATCH_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await batchCard.getByRole('link', { name: '查看批量任务' }).click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/batch\?job=[0-9a-f-]{36}/, { timeout: 20_000 });
  const popupWatched = watchConsole(popup);
  const popupDrawer = popup.getByRole('dialog');
  await expect(popupDrawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });

  const popupDecide: Array<{ status: number; body: unknown }> = [];
  popup.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      popupDecide.push({
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });
  await popupDrawer
    .getByRole('button', { name: /批\s*准/ })
    .first()
    .click();
  await expect.poll(() => popupDecide.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect.soft(popupDecide[0]?.status, 'chat batch 深链 decide 应 200').toBe(200);
  const popupToast = popup.locator('.ant-message-warning').first();
  await expect(popupToast).toBeVisible({ timeout: 15_000 });
  console.log(`[round6] chat batch popup toast: ${(await popupToast.textContent()) ?? ''}`);
  console.log(
    `[round6] chat batch popup execution: ${JSON.stringify((popupDecide[0]?.body as { execution?: unknown })?.execution)}`,
  );
  expect.soft(/undefined/.test((await popupToast.textContent()) ?? '')).toBe(false);
  await expect(popupDrawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
  await expect(popupDrawer.getByText('该任务来自对话中的 batch_execute 工具。')).toBeVisible();

  // ④ diagnosis：run_diagnosis 卡 → 深链抽屉报告 + 审批建议。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  const diagPopupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await diagCard.getByRole('link', { name: /查看诊断会话/ }).click();
  const diagPopup = await diagPopupPromise;
  await expect(diagPopup).toHaveURL(/\/alerts\?diagnosis=[0-9a-f-]{36}/, { timeout: 20_000 });
  const diagWatched = watchConsole(diagPopup);
  const diagDrawer = diagPopup.getByRole('dialog');
  await expect(diagDrawer.getByText('等待修复处理')).toBeVisible({ timeout: 30_000 });
  await expect(diagDrawer.getByText('修复建议')).toBeVisible();
  // 深链抽屉内建议带「执行（需审批）」入口（审批跳转在会话内完成）。
  await expect(diagDrawer.getByRole('button', { name: '执行（需审批）' }).first()).toBeVisible();

  expectCleanConsole(watched, 'chat main');
  expectCleanConsole(popupWatched, 'chat batch popup');
  expectCleanConsole(diagWatched, 'chat diagnosis popup');
});
