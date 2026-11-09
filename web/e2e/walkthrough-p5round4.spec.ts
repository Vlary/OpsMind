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

// 五期第 4 轮冲刺激收走查（round 4）：真链路逐页操作 + 全程 console 收集。
// 覆盖：/monitor 时间窗切换与图表、/alerts 规则 CRUD + 事件、/inspections
// 任务 CRUD/手动触发/报告/立即巡检、/batch 创建-拒绝-重试-取消、对话四条
// 工具链（metrics/alerts/batch 含深链逐台批准/diagnosis 只读）。
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

/** antd Select 多选：点开、滚到目标项、点选。 */
async function pickServer(page: Page, name: string) {
  await page.getByRole('combobox', { name: '目标服务器' }).click();
  const option = page.locator('.ant-select-item-option', { hasText: name }).first();
  for (let i = 0; i < 80 && !(await option.isVisible()); i += 1) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();
  await page.keyboard.press('Escape');
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

test('monitor：插库后有数据态——时间窗切换请求 minutes 且折线 SVG 渲染', async ({ page }) => {
  const watched = watchConsole(page);
  const serverID = await seedServerId(page);
  // 三个时间点：5min 前（1h/6h/24h 均可见）、3h 前（6h/24h）、20h 前（24h）。
  psql(`
    INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps)
    VALUES ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048),
           ('${serverID}', now() - interval '3 hours', 33.5, 35, 42, 0.5, 2048, 4096),
           ('${serverID}', now() - interval '20 hours', 55.5, 38, 44, 0.6, 4096, 8192);
    INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps, updated_at)
    VALUES ('${serverID}', now() - interval '5 minutes', 12.5, 30, 40, 0.4, 1024, 2048, now())
    ON CONFLICT (server_id) DO UPDATE SET
      created_at = EXCLUDED.created_at, cpu_pct = EXCLUDED.cpu_pct,
      mem_pct = EXCLUDED.mem_pct, disk_pct = EXCLUDED.disk_pct,
      load_1 = EXCLUDED.load_1, net_in_bps = EXCLUDED.net_in_bps,
      net_out_bps = EXCLUDED.net_out_bps, updated_at = EXCLUDED.updated_at;
  `);

  const metricsCalls: number[] = [];
  page.on('response', (res) => {
    const match = /\/api\/servers\/[^/]+\/metrics\?minutes=(\d+)/.exec(res.url());
    if (match) {
      metricsCalls.push(Number(match[1]));
    }
  });

  await page.goto('/monitor');
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();
  // 有数据态：服务器选择器 + 概要卡计数 1 台 + 最新值。
  await expect(page.getByText('当前 12.5%').first()).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText('CPU 使用率')).toBeVisible();

  // 1h 默认：折线 SVG 存在（单点也应有点标记）。
  await expect(page.locator('.opsmind-admin-page svg').first()).toBeVisible();

  // 切 6h → minutes=360（antd Segmented 的 radio input 隐藏，点 item 文本）。
  await page.locator('.ant-segmented-item', { hasText: '6 小时' }).click();
  await expect
    .poll(() => metricsCalls.filter((m) => m === 360).length, { timeout: 15_000 })
    .toBeGreaterThan(0);
  // 切 24h → minutes=1440。
  await page.locator('.ant-segmented-item', { hasText: '24 小时' }).click();
  await expect
    .poll(() => metricsCalls.filter((m) => m === 1440).length, { timeout: 15_000 })
    .toBeGreaterThan(0);
  // 24h 窗口应含 20h 前的点：折线 polyline 存在（≥2 点）。
  await expect(page.locator('svg polyline').first()).toBeVisible({ timeout: 15_000 });

  // 概要卡巡检均分（无巡检时显示 —）与采集器状态可见。
  await expect(page.getByText('采集器')).toBeVisible();

  expect(watched.pageErrors, `monitor pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `monitor console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('alerts：规则 CRUD 全 UI——新建/编辑/启停/删除', async ({ page }) => {
  const watched = watchConsole(page);
  const ruleName = `走查规则-${Date.now()}`;

  await page.goto('/alerts');
  await expect(page.getByRole('heading', { name: '告警' })).toBeVisible();

  // 新建：默认值齐全（cpu_pct gt 90 warning），只需填名称。
  await page.getByRole('button', { name: /新建规则/ }).click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('新建告警规则')).toBeVisible();
  await drawer.getByLabel('规则名称').fill(ruleName);
  await drawer.getByLabel('阈值（0..100）').fill('85');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已创建' })).toBeVisible();
  const row = page.getByRole('row', { name: new RegExp(ruleName) });
  await expect(row).toBeVisible({ timeout: 15_000 });
  // 阈值断言锚定条件列全文（RulesTab 条件单元格「CPU 使用率 > N」）：
  // getByText 是子串匹配，规则名含时间戳数字（走查规则-1793172549545
  // 恰含「95」）时裸 '85'/'95' 会同时命中名称单元格 → strict mode 双元素
  // 失败（六期 sprint 1 复验抓到）；条件全文永不撞名。
  await expect(row.getByText('CPU 使用率 > 85')).toBeVisible();

  // 编辑：阈值 85 → 95。
  await row.getByRole('button', { name: `编辑规则 ${ruleName}` }).click();
  await expect(drawer.getByText('编辑告警规则')).toBeVisible();
  await drawer.getByLabel('阈值（0..100）').fill('95');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已更新' })).toBeVisible();
  await expect(row.getByText('CPU 使用率 > 95')).toBeVisible({ timeout: 15_000 });
  await expect(row.getByText('CPU 使用率 > 85')).toHaveCount(0);

  // 启停 Switch：关 → 开。
  const toggle = row.getByRole('switch', { name: `启用规则 ${ruleName}` });
  await toggle.click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已停用' })).toBeVisible();
  await expect(toggle).not.toHaveClass(/ant-switch-checked/, { timeout: 15_000 });
  await toggle.click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已启用' })).toBeVisible();

  // 删除。
  await row.getByRole('button', { name: `删除规则 ${ruleName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '规则已删除' })).toBeVisible();
  await expect(row).toHaveCount(0, { timeout: 15_000 });

  expect(watched.pageErrors, `alerts pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `alerts console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('alerts：越限事件出现在事件 Tab，筛选与「AI 诊断」入口可用', async ({ page }) => {
  const watched = watchConsole(page);
  const serverID = await seedServerId(page);
  const ruleName = `走查事件-${Date.now()}`;
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

  await page.goto('/alerts');
  await page.getByRole('tab', { name: '事件' }).click();
  // 评估循环 tick 30s：等 firing 行出现。
  const eventRow = page.getByRole('row', { name: new RegExp(ruleName) });
  await expect(eventRow).toBeVisible({ timeout: 90_000 });
  await expect(eventRow.getByText('触发中')).toBeVisible();
  await expect(eventRow.getByText('警告')).toBeVisible();

  // 行操作「AI 诊断」→ 切到诊断 Tab。
  await eventRow.getByRole('button', { name: 'AI 诊断' }).click();
  await expect(page.getByRole('tab', { name: 'AI 诊断' })).toHaveAttribute(
    'aria-selected',
    'true',
    { timeout: 10_000 },
  );

  // 清理：删规则（级联删事件）。
  await page.getByRole('tab', { name: '规则' }).click();
  const row = page.getByRole('row', { name: new RegExp(ruleName) });
  await row.getByRole('button', { name: `删除规则 ${ruleName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(row).toHaveCount(0, { timeout: 15_000 });

  expect(watched.pageErrors, `events pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `events console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('inspections：任务新建/手动触发/报告详情 + 立即巡检同步报告', async ({ page }) => {
  const watched = watchConsole(page);
  const jobName = `走查巡检-${Date.now()}`;

  await page.goto('/inspections');
  await expect(page.getByRole('heading', { name: '巡检' })).toBeVisible();

  // 新建任务。
  await page.getByRole('button', { name: /新建任务/ }).click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('新建巡检任务').or(drawer.getByText('巡检任务'))).toBeVisible();
  await drawer.getByLabel('任务名称').fill(jobName);
  await drawer.getByLabel('cron 表达式（UTC）').fill('10 3 * * *');
  await drawer.getByRole('button', { name: /保\s*存/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: '任务已创建' })).toBeVisible();
  const jobRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(jobRow).toBeVisible({ timeout: 15_000 });

  // 行内手动触发 → 报告 Tab 出现行（4s 补拉）→ 详情抽屉。
  await jobRow.getByRole('button', { name: `立即运行 ${jobName}` }).click();
  await expect(page.locator('.ant-message-success', { hasText: '已触发巡检' })).toBeVisible();
  await page.getByRole('tab', { name: '报告' }).click();
  const reportRow = page.getByRole('row', { name: new RegExp(jobName) });
  await expect(reportRow).toBeVisible({ timeout: 60_000 });
  // 等报告完成（finished_at 落定）。
  await expect(reportRow.getByText('已完成')).toBeVisible({ timeout: 60_000 });
  // antd 对纯双字文本按钮自动插空格（accessible name「详 情」）。
  await reportRow.getByRole('button', { name: /详\s*情/ }).click();
  const reportDrawer = page.getByRole('dialog');
  await expect(reportDrawer.getByText('巡检报告')).toBeVisible();
  await expect(reportDrawer.getByText('覆盖 / 异常')).toBeVisible();
  await expect(reportDrawer.getByText('e2e-fake-host')).toBeVisible({ timeout: 15_000 });
  await reportDrawer.getByRole('button', { name: /关\s*闭/ }).click();

  // 立即巡检（同步）：选 e2e-fake-host → 开始 → 报告详情打开。
  await page.getByRole('button', { name: /立即巡检/ }).click();
  const modal = page.getByRole('dialog');
  await modal.getByRole('combobox', { name: '巡检目标服务器' }).click();
  // antd 下拉面板挂在 body 下（非 modal 子树），全局定位当前打开的选项层。
  const option = page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: 'e2e-fake-host',
    })
    .first();
  for (let i = 0; i < 80 && !(await option.isVisible()); i += 1) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();
  await page.keyboard.press('Escape');
  await modal.getByRole('button', { name: /开始巡检/ }).click();
  await expect(page.locator('.ant-message-success', { hasText: /巡检完成/ })).toBeVisible({
    timeout: 60_000,
  });
  await expect(page.getByRole('dialog').getByText('巡检报告')).toBeVisible({ timeout: 15_000 });

  // 删除任务（级联删报告）。
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /关\s*闭/ })
    .click();
  await page.getByRole('tab', { name: '任务' }).click();
  await jobRow.getByRole('button', { name: `删除任务 ${jobName}` }).click();
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(jobRow).toHaveCount(0, { timeout: 15_000 });

  expect(watched.pageErrors, `inspections pageerror：${watched.pageErrors.join(' | ')}`).toEqual(
    [],
  );
  expect(watched.errors, `inspections console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('batch：创建 → 拒绝 → 重试预填 → 再创建 → 取消', async ({ page }) => {
  const watched = watchConsole(page);
  await page.goto('/batch');
  await expect(page.getByRole('heading', { name: '批量', exact: true })).toBeVisible();

  const commandBox = page.getByPlaceholder('df -h /');
  await commandBox.fill('uptime');
  await expect(page.getByText('安全').first()).toBeVisible({ timeout: 15_000 });
  await pickServer(page, 'e2e-fake-host');
  await page.getByRole('button', { name: '创建并分派' }).click();

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });

  // 拒绝：行变「已拒绝」，失败计数 1，重试按钮出现。
  await drawer
    .getByRole('button', { name: /拒\s*绝/ })
    .first()
    .click();
  await expect(drawer.getByText('已拒绝').first()).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText(/失败\s*1/)).toBeVisible();
  const retryButton = drawer.getByRole('button', { name: /重试失败项（1 台）/ });
  await expect(retryButton).toBeVisible();
  await retryButton.click();

  // 预填断言：命令回填 uptime，目标回填 e2e-fake-host。
  await expect(commandBox).toHaveValue('uptime', { timeout: 10_000 });
  await expect(page.getByTitle('e2e-fake-host')).toBeVisible();

  // 再创建（同一台）：回到待审批，随后取消任务清理。
  await page.getByRole('button', { name: '创建并分派' }).click();
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  await drawer.getByRole('button', { name: '取消任务' }).first().click();
  await page.locator('.ant-popover').getByRole('button', { name: '取消任务' }).click();
  await expect(drawer.getByText('已取消').first()).toBeVisible({ timeout: 30_000 });

  expect(watched.pageErrors, `batch pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `batch console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('chat __scenario_metrics__ / __scenario_alerts__：工具链成功且无 console 错误', async ({
  page,
}) => {
  const watched = watchConsole(page);

  // metrics 链。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_METRICS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const metricsCard = page.locator('.chat-toolcall-card', { hasText: 'get_metrics' }).first();
  await expect(metricsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(METRICS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  // alerts 链（同页新会话）。
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_ALERTS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
  const alertsCard = page.locator('.chat-toolcall-card', { hasText: 'list_alerts' }).first();
  await expect(alertsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(ALERTS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  expect(watched.pageErrors, `chat tools pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `chat tools console error：${watched.errors.join(' | ')}`).toEqual([]);
});

test('chat __scenario_batch__：深链新标签逐台批准——decide 200 且失败语义 warning', async ({
  page,
}) => {
  const watched = watchConsole(page);
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_BATCH);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const batchCard = page.locator('.chat-toolcall-card', { hasText: 'batch_execute' }).first();
  await expect(batchCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

  const deepLink = batchCard.getByRole('link', { name: '查看批量任务' });
  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await deepLink.click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/batch\?job=[0-9a-f-]{36}/, { timeout: 20_000 });

  const popupWatched = watchConsole(popup);
  const decideCalls: Array<{ url: string; status: number }> = [];
  popup.on('response', (res) => {
    if (/\/api\/approvals\/[^/]+\/decide$/.test(res.url())) {
      decideCalls.push({ url: res.url(), status: res.status() });
    }
  });

  const drawer = popup.getByRole('dialog');
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  await drawer
    .getByRole('button', { name: /批\s*准/ })
    .first()
    .click();

  // decide 打真实 UUID 且 200。
  await expect.poll(() => decideCalls.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect(decideCalls[0].url).toMatch(/\/api\/approvals\/[0-9a-f-]{36}\/decide$/);
  expect(decideCalls[0].status).toBe(200);

  // 目标 127.0.0.1:1 不可达 → 执行失败行（通道失败无退出码 → 列显 —，
  // 错误码 SSH_EXEC_FAILED 落中文「远程命令执行失败」——sprint 4 起错误列
  // 与 decide toast 均回落通用 CODE_MESSAGES，不再直显英文原码）。
  await expect(drawer.getByText('执行失败').first()).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('远程命令执行失败').first()).toBeVisible();
  await expect(popup.locator('.ant-message-warning', { hasText: /命令执行失败/ })).toBeVisible({
    timeout: 15_000,
  });

  expect(watched.pageErrors, `chat batch pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(
    popupWatched.pageErrors,
    `batch popup pageerror：${popupWatched.pageErrors.join(' | ')}`,
  ).toEqual([]);
  expect(
    popupWatched.errors,
    `batch popup console error：${popupWatched.errors.join(' | ')}`,
  ).toEqual([]);
});

test('chat __scenario_diagnosis__：只读诊断——工具卡成功 + 深链抽屉报告可见', async ({ page }) => {
  const watched = watchConsole(page);
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  const deepLink = diagCard.getByRole('link', { name: /查看诊断会话/ });
  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await deepLink.click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/alerts\?diagnosis=[0-9a-f-]{36}/, { timeout: 20_000 });
  const popupWatched = watchConsole(popup);

  const drawer = popup.getByRole('dialog');
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('修复建议')).toBeVisible();

  expect(watched.pageErrors, `chat diag pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(
    popupWatched.pageErrors,
    `diag popup pageerror：${popupWatched.pageErrors.join(' | ')}`,
  ).toEqual([]);
  expect(
    popupWatched.errors,
    `diag popup console error：${popupWatched.errors.join(' | ')}`,
  ).toEqual([]);
});

test('alerts：事件状态/等级筛选联动 + AI 诊断 Tab 手动发起会话', async ({ page }) => {
  const watched = watchConsole(page);
  const serverID = await seedServerId(page);
  const ruleName = `走查筛选-${Date.now()}`;
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

  await page.goto('/alerts');
  await page.getByRole('tab', { name: '事件' }).click();
  const eventRow = page.getByRole('row', { name: new RegExp(ruleName) });
  await expect(eventRow).toBeVisible({ timeout: 90_000 });

  // 状态筛选：切「已恢复」→ firing 行消失；切回「全部」→ 行回来。
  const statusSelect = page.getByRole('combobox', { name: '事件状态过滤' });
  await statusSelect.click();
  await page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: '已恢复',
    })
    .click();
  await expect(eventRow).toHaveCount(0, { timeout: 15_000 });
  await statusSelect.click();
  await page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: '全部',
    })
    .click();
  await expect(eventRow).toBeVisible({ timeout: 15_000 });

  // 等级筛选：warning 级行在「严重」下消失。
  const severitySelect = page.getByRole('combobox', { name: '事件等级过滤' });
  await severitySelect.click();
  await page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option', {
      hasText: '严重',
    })
    .click();
  await expect(eventRow).toHaveCount(0, { timeout: 15_000 });

  // AI 诊断 Tab：手动发起（选 e2e-fake-host）→ 抽屉自动打开至 awaiting_fix。
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

  expect(watched.pageErrors, `filter pageerror：${watched.pageErrors.join(' | ')}`).toEqual([]);
  expect(watched.errors, `filter console error：${watched.errors.join(' | ')}`).toEqual([]);
});
