import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { expect, test, type Page } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  DIAGNOSIS_FINAL_PREFIX,
  METRICS_FINAL_PREFIX,
  ALERTS_FINAL_PREFIX,
  SCENARIO_ALERTS,
  SCENARIO_BATCH,
  SCENARIO_DIAGNOSIS,
  SCENARIO_METRICS,
  sendMessage,
} from './helpers';

// 五期第 5 轮冲刺激收走查（round 5）：真链路逐页操作 + 全程 console 收集。
// 与 round4 互补的增量覆盖：
// - /monitor 空态三档（页级空态 / 窗口无点「暂无采集数据」/ 单点「点数不足」）；
// - /alerts 诊断全链路到 decide：awaiting_fix → 执行（需审批）→ 批准 →
//   执行结果 toast 全文 → 审批 Alert 消失 → 标记完成 → done；
// - /batch 页面链路：tag 筛选 + 意图描述 → 创建 → 批准 → 执行失败 toast
//   全文（重点：exit_code 缺失时是否拼出「退出码 undefined」）→ partial
//   进度 1/1 → 错误列中文 → 重试预填；
// - /inspections 报告详情：不可达主机错误列中文 + 原始输出折叠；
// - 对话四条工具链全走一遍（metrics/alerts/batch 深链批准/diagnosis 深链）。
// 本 spec 是走查脚本：断言失败 / console error 均作为走查发现记录。

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

async function seedServerId(page: Page): Promise<string> {
  const res = await page.request.get('/api/servers', { headers: API_HEADERS });
  expect(res.ok()).toBeTruthy();
  const servers = (((await res.json()) ?? { items: [] }).items ?? []) as Array<{
    id: string;
    name: string;
  }>;
  expect(servers.length).toBeGreaterThan(0);
  return servers[0].id;
}

test.use({ storageState: AUTH_STATE_FILE });

test.describe.configure({ mode: 'serial' });

test('monitor：空态三档——页级空态/窗口无点/单点点数不足', async ({ page }) => {
  const watched = watchConsole(page);

  // 清场：本用例是「独占干净库」设计（①空态/②窗口无点均要求库内无其他
  // 指标行）；前置 spec（walkthrough-p5round4 直插 server_metrics_latest
  // 且不回收）会残留行——config 已把两走查 spec 链式串行（round4 →
  // round5），此时库无并发写入，开头一次性清空两表即可恢复空态前置。
  psql('DELETE FROM server_metrics; DELETE FROM server_metrics_latest;');

  // ① 页级空态：环境刚起、尚无 latest 行（collector 对 e2e-fake-host 采集
  // 失败不写库），/monitor 应显示 0 台 + 引导空态。
  await page.goto('/monitor');
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  await expect(page.getByText('服务器（已采集）')).toBeVisible();
  await expect(page.getByText('暂无服务器指标：请先在「服务器」页添加资产并开启采集')).toBeVisible({
    timeout: 20_000,
  });
  // 下拉空提示（打开下拉验证 notFoundContent）。
  await page.getByRole('combobox', { name: '选择服务器' }).click();
  await expect(
    page.getByText('暂无已采集的服务器（等待首轮采集）').first(),
  ).toBeVisible({ timeout: 10_000 });
  await page.keyboard.press('Escape');

  // ② 窗口无点：只插 20h 前的点（1h/6h 窗口外，24h 窗口内）。
  const serverID = await seedServerId(page);
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    VALUES ('${serverID}', now() - interval '20 hours', 55.5, 38, 44, 0.6, 4096, 8192);
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${serverID}', now() - interval '20 hours', 55.5, 38, 44, 0.6, 4096, 8192, now())
    ON CONFLICT (server_id) DO UPDATE SET
      created_at = EXCLUDED.created_at, cpu_pct = EXCLUDED.cpu_pct,
      mem_pct = EXCLUDED.mem_pct, disk_pct = EXCLUDED.disk_pct,
      load_1 = EXCLUDED.load_1, net_in_bps = EXCLUDED.net_in_bps,
      net_out_bps = EXCLUDED.net_out_bps, updated_at = EXCLUDED.updated_at;
  `);
  await page.reload();
  // 有资产后：概要「1 台」+ 卡片当前值可见。
  await expect(page.getByText('当前 55.5%').first()).toBeVisible({ timeout: 20_000 });
  // 1h 默认窗口无点：六张卡显示「暂无采集数据」。
  await expect(page.getByText('暂无采集数据').first()).toBeVisible({ timeout: 20_000 });

  // ③ 单点：24h 窗口含 20h 前 1 点 → 「点数不足，等待更多采集数据」。
  await page.locator('.ant-segmented-item', { hasText: '24 小时' }).click();
  await expect(page.getByText('点数不足，等待更多采集数据').first()).toBeVisible({
    timeout: 20_000,
  });

  // ④ 两点成线：再插 5min 前的点，6h 窗口（5min 在窗内、20h 在窗外——只有
  // 一点？改插 3h 前）→ 补一个 3h 前的点，6h 窗口 2 点出 polyline。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    VALUES ('${serverID}', now() - interval '3 hours', 33.5, 35, 42, 0.5, 2048, 4096),
           ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048);
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048, now())
    ON CONFLICT (server_id) DO UPDATE SET
      created_at = EXCLUDED.created_at, cpu_pct = EXCLUDED.cpu_pct,
      mem_pct = EXCLUDED.mem_pct, disk_pct = EXCLUDED.disk_pct,
      load_1 = EXCLUDED.load_1, net_in_bps = EXCLUDED.net_in_bps,
      net_out_bps = EXCLUDED.net_out_bps, updated_at = EXCLUDED.updated_at;
  `);
  // overview（latest 概要值）无轮询、缓存未失效——插库后 reload 再看。
  await page.reload();
  await page.locator('.ant-segmented-item', { hasText: '6 小时' }).click();
  await expect(page.locator('svg polyline').first()).toBeVisible({ timeout: 20_000 });
  // 概要卡「当前 12.5%」（latest 已更新到 5min 前的值）。
  await expect(page.getByText('当前 12.5%').first()).toBeVisible({ timeout: 20_000 });

  expect(watched.pageErrors, `monitor pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `monitor console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('alerts：诊断全链路——fix 审批→批准→执行失败 toast 全文→标记完成', async ({ page }) => {
  const watched = watchConsole(page);

  // AI 诊断 Tab 手动发起（fake-llm 协议 probe→report → awaiting_fix）。
  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();
  await page.getByRole('tab', { name: 'AI 诊断' }).click();
  await page.getByRole('combobox', { name: '诊断目标服务器' }).click();
  await page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: 'e2e-fake-host',
    })
    .first()
    .click();
  await page.getByRole('button', { name: /发起 AI 诊断/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '诊断已发起' })).toBeVisible();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 90_000 });
  await expect(drawer.getByText('修复建议')).toBeVisible();

  // 修复建议行：「执行（需审批）」→ fix 端点 → 待审批 Alert。
  await drawer.getByRole('button', { name: '执行（需审批）' }).first().click();
  await expect(
    page.locator('.ant-message-success', { hasText: '已创建修复审批，请批准或拒绝' }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(drawer.getByText('待审批的修复命令')).toBeVisible({ timeout: 15_000 });
  // 建议命令原样出现在审批区。
  await expect(drawer.locator('.opsmind-admin-mono', { hasText: 'systemctl status sshd' })).toBeVisible();

  const decideCalls: Array<{ url: string; status: number; body: unknown }> = [];
  page.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      decideCalls.push({
        url: res.url(),
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });

  // 批准执行：decide 200，执行结果 toast（目标不可达 → warning 全文含退出码）。
  await drawer.getByRole('button', { name: '批准执行' }).click();
  await expect.poll(() => decideCalls.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect(decideCalls[0].status).toBe(200);
  const toast = page.locator('.ant-message-warning').first();
  await expect(toast).toBeVisible({ timeout: 15_000 });
  const toastText = (await toast.textContent()) ?? '';
  // 走查记录点：console 完整原文（是否含 undefined / 退出码数值）。
  console.log(`[round5] diagnosis fix decide toast: ${toastText}`);
  console.log(
    `[round5] diagnosis fix decide execution: ${JSON.stringify((decideCalls[0].body as { execution?: unknown })?.execution)}`,
  );
  // 至少语义为失败（不误导成功）。
  await expect(toast).toContainText('修复命令执行失败');
  // 执行后审批 Alert 消失（decide 完成 setPendingApproval(null)）。
  await expect(drawer.getByText('待审批的修复命令')).toHaveCount(0, { timeout: 15_000 });

  // 标记处理完成 → 状态「已完成」。
  await drawer.getByRole('button', { name: /标记处理完成/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '诊断会话已标记完成' })).toBeVisible();
  await expect(drawer.getByText('已完成')).toBeVisible({ timeout: 15_000 });
  await drawer.getByRole('button', { name: /关\s*闭/ }).click();

  expect(watched.pageErrors, `diagnosis pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `diagnosis console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('batch：tag 筛选+描述创建→批准→失败 toast 全文→partial 进度→重试预填', async ({ page }) => {
  const watched = watchConsole(page);
  await page.goto('/batch');
  await expect(page.getByRole('heading', { name: '批量', exact: true })).toBeVisible();

  // tag 快速筛选：点 e2e chip → extra 文案切换。
  await page.locator('.ant-tag', { hasText: 'e2e' }).first().click();
  await expect(page.getByText(/已按 tags 筛选：e2e/)).toBeVisible();

  const commandBox = page.getByPlaceholder('df -h /');
  await commandBox.fill('uptime');
  await expect(page.getByText('安全').first()).toBeVisible({ timeout: 15_000 });
  await page.getByLabel('自然语言描述（可选）').fill('走查round5批量意图');
  // tag 筛选后下拉只剩 e2e-fake-host。
  await page.getByRole('combobox', { name: '目标服务器' }).click();
  await page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: 'e2e-fake-host',
    })
    .first()
    .click();
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '创建并分派' }).click();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  // 意图描述行可见。
  await expect(drawer.getByText('走查round5批量意图')).toBeVisible();

  const decideCalls: Array<{ url: string; status: number; body: unknown }> = [];
  page.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      decideCalls.push({
        url: res.url(),
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });

  // 批准：decide 200 + 执行失败 toast 全文（重点查「退出码 undefined」）。
  await drawer.getByRole('button', { name: /批\s*准/ }).first().click();
  await expect.poll(() => decideCalls.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect(decideCalls[0].status).toBe(200);
  const toast = page.locator('.ant-message-warning').first();
  await expect(toast).toBeVisible({ timeout: 15_000 });
  const toastText = (await toast.textContent()) ?? '';
  console.log(`[round5] batch decide toast: ${toastText}`);
  console.log(
    `[round5] batch decide execution: ${JSON.stringify((decideCalls[0].body as { execution?: unknown })?.execution)}`,
  );

  // 执行失败行 + 错误列中文（sprint4：SSH_EXEC_FAILED → 远程命令执行失败）。
  await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('远程命令执行失败').first()).toBeVisible({ timeout: 15_000 });

  // 关闭抽屉 → 列表行状态 partial + 进度 1/1。
  await drawer.getByRole('button', { name: /关\s*闭/ }).click();
  const jobRow = page.getByRole('row', { name: /走查round5批量意图/ });
  await expect(jobRow.getByText('部分完成')).toBeVisible({ timeout: 20_000 });
  // 进度列是独立单元格「done/total」。不能用 getByText('1/1')：子串匹配
  // 会命中年份里含「1/1」的创建时间（如 2026/11/10 02:39:35）而触发
  // strict mode 冲突（八期门禁实测）。按单元格精确匹配。
  await expect(jobRow.getByRole('cell', { name: '1/1', exact: true })).toBeVisible();

  // 重新打开详情 → 重试失败项（1 台）→ 预填表单。
  await jobRow.getByRole('button', { name: /详\s*情/ }).click();
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 15_000 });
  await drawer.getByRole('button', { name: /重试失败项（1 台）/ }).click();
  await expect(commandBox).toHaveValue('uptime', { timeout: 10_000 });

  expect(watched.pageErrors, `batch pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `batch console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('inspections：报告详情——不可达主机错误列中文 + 原始输出折叠', async ({ page }) => {
  const watched = watchConsole(page);
  const jobName = `走查r5巡检-${Date.now()}`;

  await page.goto('/inspections');
  await expect(page.getByRole('heading', { name: '巡检' })).toBeVisible();

  await page.getByRole('button', { name: /新建任务/ }).click();
  const formDrawer = page.getByRole('dialog');
  await formDrawer.getByLabel('任务名称').fill(jobName);
  await formDrawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '任务已创建' })).toBeVisible();
  const jobRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(jobRow).toBeVisible({ timeout: 15_000 });

  // 手动触发 → 报告 Tab → 已完成 → 详情。
  await jobRow.getByRole('button', { name: `立即运行 ${jobName}` }).click();
  await expect(page.locator('.ant-message-success', { hasText: '已触发巡检' })).toBeVisible();
  await page.getByRole('tab', { name: '报告' }).click();
  const reportRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(reportRow).toBeVisible({ timeout: 60_000 });
  await expect(reportRow.getByText('已完成')).toBeVisible({ timeout: 60_000 });
  await reportRow.getByRole('button', { name: /详\s*情/ }).click();

  const reportDrawer = page.getByRole('dialog');
  await expect(reportDrawer.getByText('巡检报告')).toBeVisible();
  await expect(reportDrawer.getByText('e2e-fake-host')).toBeVisible({ timeout: 15_000 });
  // 不可达主机：错误列落中文分类（connection refused → 巡检目标不可达…）。
  await expect(reportDrawer.getByText(/巡检目标不可达/).first()).toBeVisible({ timeout: 15_000 });
  // 展开行：异常项 detail 携带错误链原文（采集失败 raw_output 为空，
  // 「原始采集输出」折叠不渲染是数据事实，非缺陷）。
  await reportDrawer.locator('.ant-table-row-expand-icon').first().click();
  await expect(reportDrawer.getByText(/connection refused/i).first()).toBeVisible({
    timeout: 10_000,
  });
  await reportDrawer.getByRole('button', { name: '关闭', exact: true }).click();

  // 清理任务。
  await page.getByRole('tab', { name: '任务' }).click();
  await jobRow.getByRole('button', { name: `删除任务 ${jobName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(jobRow).toHaveCount(0, { timeout: 15_000 });

  expect(watched.pageErrors, `inspections pageerror：${watched.pageErrors.join(' | ')}`).toEqual(
    [],
  );
  expect(watched.errors, `inspections console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('monitor：采集器暂停/恢复——Alert 提示与概要卡状态联动', async ({ page }) => {
  const watched = watchConsole(page);
  const serverID = await seedServerId(page);
  // 保证有 latest 数据（上一用例已插，幂等再插一条 5min 前）。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    VALUES ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048);
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048, now())
    ON CONFLICT (server_id) DO UPDATE SET updated_at = EXCLUDED.updated_at;
  `);
  await page.goto('/monitor');
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  await expect(page.getByText('当前 12.5%').first()).toBeVisible({ timeout: 20_000 });

  // 暂停：关 Switch → 保存 → Alert + 概要卡「已暂停」。
  await page.getByRole('switch', { name: '启用采集' }).click();
  await page.getByRole('button', { name: '保存采集配置' }).click();
  await expect(page.locator('.ant-message-success', { hasText: '采集配置已保存' })).toBeVisible();
  await expect(page.getByText('采集已暂停：折线展示历史数据，不再自动刷新。')).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByText('已暂停').first()).toBeVisible({ timeout: 15_000 });

  // 恢复。
  await page.getByRole('switch', { name: '启用采集' }).click();
  await page.getByRole('button', { name: '保存采集配置' }).click();
  await expect(
    page.locator('.ant-message-success', { hasText: '采集配置已保存' }),
  ).toBeVisible();
  await expect(page.getByText('运行中').first()).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('采集已暂停：折线展示历史数据，不再自动刷新。')).toHaveCount(0, {
    timeout: 15_000,
  });

  expect(watched.pageErrors, `collector pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `collector console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('alerts：通知渠道 CRUD 冒烟——新建 webhook → 行出现 → 删除', async ({ page }) => {
  const watched = watchConsole(page);
  const channelName = `走查渠道-${Date.now()}`;

  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();
  await page.getByRole('tab', { name: '渠道' }).click();

  await page.getByRole('button', { name: /新建渠道/ }).click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText(/通知渠道/).first()).toBeVisible();
  await drawer.getByLabel('渠道名称').fill(channelName);
  await drawer.getByLabel('回调 URL').fill('https://example.com/hook');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: /渠道已创建/ })).toBeVisible();

  const row = page.getByRole('row', { name: new RegExp(channelName) });
  await expect(row).toBeVisible({ timeout: 15_000 });

  await row.getByRole('button', { name: `删除渠道 ${channelName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: /渠道已删除/ })).toBeVisible();
  await expect(row).toHaveCount(0, { timeout: 15_000 });

  expect(watched.pageErrors, `channels pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `channels console error：${watched.errors.join(' | ')}`).toEqual([]);
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

  // ③ batch：batch_execute 卡计数行中文（待审批 1）→ 深链新标签 → 逐台批准。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_BATCH);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const batchCard = page.locator('.chat-toolcall-card', { hasText: 'batch_execute' }).first();
  await expect(batchCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(batchCard.getByText('待审批 1')).toBeVisible({ timeout: 15_000 });

  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await batchCard.getByRole('link', { name: '查看批量任务' }).click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/batch\?job=[0-9a-f-]{36}/, { timeout: 20_000 });
  const popupWatched = watchConsole(popup);
  const popupDrawer = popup.getByRole('dialog');
  await expect(popupDrawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });

  const popupDecide: Array<{ url: string; status: number; body: unknown }> = [];
  popup.on('response', async (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url()) && res.request().method() === 'POST') {
      popupDecide.push({
        url: res.url(),
        status: res.status(),
        body: res.ok() ? await res.json().catch(() => null) : null,
      });
    }
  });
  await popupDrawer.getByRole('button', { name: /批\s*准/ }).first().click();
  await expect.poll(() => popupDecide.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect(popupDecide[0].status).toBe(200);
  const popupToast = popup.locator('.ant-message-warning').first();
  await expect(popupToast).toBeVisible({ timeout: 15_000 });
  console.log(`[round5] chat batch popup toast: ${(await popupToast.textContent()) ?? ''}`);
  console.log(
    `[round5] chat batch popup execution: ${JSON.stringify((popupDecide[0].body as { execution?: unknown })?.execution)}`,
  );
  await expect(popupDrawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
  // 任务来自对话标记。
  await expect(popupDrawer.getByText('该任务来自对话中的 batch_execute 工具。')).toBeVisible();

  // ④ diagnosis：run_diagnosis 卡 severity + summary → 深链抽屉报告。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  // 卡片专属视图：severity Tag + 摘要文本。
  await expect(diagCard.locator('.ant-tag', { hasText: 'warning' })).toBeVisible();

  const diagPopupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await diagCard.getByRole('link', { name: /查看诊断会话/ }).click();
  const diagPopup = await diagPopupPromise;
  await expect(diagPopup).toHaveURL(/\/alerts\?diagnosis=[0-9a-f-]{36}/, { timeout: 20_000 });
  const diagWatched = watchConsole(diagPopup);
  const diagDrawer = diagPopup.getByRole('dialog');
  await expect(diagDrawer.getByText('等待修复处理')).toBeVisible({ timeout: 30_000 });
  await expect(diagDrawer.getByText('修复建议')).toBeVisible();

  expect(watched.pageErrors, `chat pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `chat console error：${watched.errors.join(' | ')}`).toEqual([]);
  expect(
    popupWatched.pageErrors,
    `batch popup pageerror：${popupWatched.pageErrors.join(' | ')}`,
  ).toEqual([]);
  expect(popupWatched.errors, `batch popup console error：${popupWatched.errors.join(' | ')}`).toEqual(
    [],
  );
  expect(
    diagWatched.pageErrors,
    `diag popup pageerror：${diagWatched.pageErrors.join(' | ')}`,
  ).toEqual([]);
  expect(diagWatched.errors, `diag popup console error：${diagWatched.errors.join(' | ')}`).toEqual(
    [],
  );
});
