import { expect, test } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  DIAGNOSIS_FINAL_PREFIX,
  SCENARIO_DIAGNOSIS,
  sendMessage,
} from './helpers';

// 五期打磨 sprint 3 的三条防回归 E2E（真链路，走查 #1/#2/#3/#4）：
// 1) 诊断「执行（需审批）」首点链路：fix 端点曾返回 {approval, diagnosis_id}
//    wrapper（契约 200 直返 $ref Approval），前端把 wrapper 整体当 Approval
//    存入待审批态——.id/.command 全 undefined：命令区空白、批准/拒绝 decide
//    打到字面模板 URL /api/approvals/%7Bid%7D/decide 返回 500。既有用例
//    （polish-p5sprint2）只断言「批准执行」按钮出现，从未点击，故漏网。
//    本用例点击批准并四重断言：待审批命令可见、fix 直返审批对象、decide
//    打真实 UUID 且 200、无 pageerror（void mutateAsync 未 catch 的
//    unhandledrejection 回归）。
// 2) 执行失败语义：目标 e2e-fake-host（127.0.0.1:1 不可达）批准后执行
//    必然失败（exit_code -1 / SSH_EXEC_FAILED），此前 toast 用绿色 success
//    样式提示「命令已执行：退出码 -1（…）」误导用户——现在必须 warning。
// 3) 删除巡检任务后报告 Tab 幽灵行：deleteMutation.onSuccess 此前只失效
//    inspectionJobs；Tabs 面板不销毁、ReportsTab 无未完成报告时不轮询
//    ——报告缓存永不重取，切 Tab 仍显示已删任务的报告行，点「详情」404。

const API_HEADERS = { Origin: 'http://localhost:4173' };
// 诊断场景（e2e/fixtures/fake-llm.mjs DIAGNOSIS_REPORT_JSON）的建议命令。
const DIAGNOSIS_SUGGESTION = 'systemctl status sshd';

test.use({ storageState: AUTH_STATE_FILE });

test('__scenario_diagnosis__：首点批准链路——fix 直返审批、decide 打真实 UUID、失败执行 warning、无 pageerror', async ({
  page,
}) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard).toBeVisible();
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });

  const deepLink = diagCard.getByRole('link', { name: /查看诊断会话/ });
  await expect(deepLink).toBeVisible();
  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await deepLink.click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/alerts\?diagnosis=[0-9a-f-]{36}/, { timeout: 20_000 });

  // 收集页面未捕获异常（void mutateAsync 未 catch → unhandledrejection）。
  const pageErrors: string[] = [];
  popup.on('pageerror', (err) => pageErrors.push(String(err)));
  // 捕获 fix / decide 的响应（URL + 状态 + fix 响应体）。
  const fixBodies: Array<Record<string, unknown>> = [];
  const decideCalls: Array<{ url: string; status: number }> = [];
  popup.on('response', async (res) => {
    const url = res.url();
    if (/\/api\/diagnosis\/sessions\/[^/]+\/fix$/.test(url)) {
      fixBodies.push(((await res.json().catch(() => ({}))) ?? {}) as Record<string, unknown>);
    }
    if (/\/api\/approvals\/[^/]+\/decide$/.test(url)) {
      decideCalls.push({ url, status: res.status() });
    }
  });

  const drawer = popup.getByRole('dialog');
  await expect(drawer.getByText('等待修复处理')).toBeVisible({ timeout: 30_000 });

  // 首点「执行（需审批）」创建 console 审批。
  await drawer.getByRole('button', { name: /执行（需审批）/ }).first().click();

  // 断言 A（走查 #1 后半）：待审批 Alert 内命令文本非空白——wrapper 直存
  // 时 .command 为 undefined，Alert 渲染空白（报告区的 CodeBlock 也含同一
  // 命令文本，必须限定在「待审批的修复命令」Alert 内断言）。
  const pendingAlert = drawer.locator('.ant-alert', { hasText: '待审批的修复命令' });
  await expect(pendingAlert).toBeVisible({ timeout: 15_000 });
  await expect(pendingAlert.getByText(DIAGNOSIS_SUGGESTION)).toBeVisible();

  // 断言 B（六期契约 2.11 约定 2 ⑤）：fix 200 返回 DiagnosisFixResult
  // wrapper——{approval, execution} 按统一门限分流。standard 档 + 种子
  // 服务器 auto_exec_enabled=false → safe 命令也走审批：approval 非 null、
  // execution 为 null（五期「fix 直返 Approval」已被六期 wrapper 取代）。
  await expect
    .poll(() => fixBodies.length, { timeout: 10_000 })
    .toBeGreaterThanOrEqual(1);
  const fixBody = fixBodies[0] as {
    approval?: { id?: string; command?: string } | null;
    execution?: unknown;
  };
  expect(fixBody.approval?.id, 'fix 200 wrapper.approval.id 为审批 UUID').toMatch(
    /^[0-9a-f-]{36}$/,
  );
  expect(fixBody.approval?.command).toBe(DIAGNOSIS_SUGGESTION);
  expect(fixBody.execution, 'standard 档经审批分流：wrapper.execution 为 null').toBeNull();

  // 断言 C（走查 #1 核心）：首点批准——decide 必须打到真实审批 UUID 且
  // 200（回归时 .id 为 undefined，URL 字面 %7Bid%7D → 500）。
  await drawer.getByRole('button', { name: /批准执行/ }).click();
  await expect
    .poll(() => decideCalls.length, { timeout: 20_000 })
    .toBeGreaterThanOrEqual(1);
  expect(decideCalls[0].url).toMatch(/\/api\/approvals\/[0-9a-f-]{36}\/decide$/);
  expect(decideCalls[0].status).toBe(200);

  // 断言 D（走查 #3）：目标不可达 → 执行失败（exit_code -1、SSH_EXEC_
  // FAILED），toast 必须 warning 语义，不得出现绿色 success 的
  // 「修复命令已执行」。
  await expect(
    popup.locator('.ant-message-warning', { hasText: /修复命令执行失败：退出码 -1/ }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(
    popup.locator('.ant-message-success', { hasText: /修复命令已执行/ }),
  ).toHaveCount(0);

  // 断言 E（走查 #4）：decide 全链路无 unhandledrejection（回归时 void
  // decideMutation.mutateAsync 未 catch，500 reject 抛出 pageerror
  // 「internal server error」）。
  expect(pageErrors, `不应有未捕获页面异常：${pageErrors.join(' | ')}`).toEqual([]);
});

test('删除巡检任务后报告 Tab 立即同步——已删任务的报告幽灵行不再显示', async ({ page }) => {
  const jobName = `e2e-ghost-${Date.now()}`;
  // 建任务（cron 五段，目标空 tags = 全部服务器）。
  const createRes = await page.request.post('/api/inspection/jobs', {
    headers: API_HEADERS,
    data: {
      name: jobName,
      cron: '5 3 * * *',
      target_tags: [],
      template: 'basic',
      notify_enabled: false,
      enabled: true,
    },
  });
  expect(createRes.status()).toBe(201);
  const job = (await createRes.json()) as { id: string };

  // 触发一次巡检并等报告完成（目标不可达也产出报告行——覆盖台数含
  // e2e-fake-host，SSH 拒连很快收尾）。
  const runRes = await page.request.post(`/api/inspection/jobs/${job.id}/run`, {
    headers: API_HEADERS,
  });
  expect(runRes.status()).toBe(202);

  const deadline = Date.now() + 60_000;
  let finished = false;
  while (Date.now() < deadline) {
    const reportsRes = await page.request.get('/api/inspection/reports?limit=50', {
      headers: API_HEADERS,
    });
    expect(reportsRes.ok()).toBeTruthy();
    const items = (((await reportsRes.json()) ?? { items: [] }).items ?? []) as Array<{
      job_name: string;
      finished_at: string | null;
    }>;
    // 等 finished_at 落定：进行中报告会让 ReportsTab 开 10s 轮询，自动
    // 刷新会掩盖「删除后缓存不失效」的回归——终态报告才无轮询。
    if (items.some((r) => r.job_name === jobName && r.finished_at !== null)) {
      finished = true;
      break;
    }
    await page.waitForTimeout(2_000);
  }
  expect(finished, '巡检报告应在 60s 内完成（finished_at 落定）').toBeTruthy();

  // UI：先切报告 Tab 建立查询缓存（幽灵行复现前提），再删任务。
  await page.goto('/inspections');
  await page.getByRole('tab', { name: '报告' }).click();
  await expect(page.getByRole('cell', { name: jobName })).toBeVisible({ timeout: 20_000 });

  await page.getByRole('tab', { name: '任务' }).click();
  const jobRow = page.getByRole('row', { name: new RegExp(jobName) });
  await jobRow.getByRole('button', { name: `删除任务 ${jobName}` }).click();
  // antd 对纯双字文本按钮自动插空格（accessible name「删 除」），正则
  // 锚定全文，避免部分匹配到行内「删除任务 <name>」图标按钮。
  await page.getByRole('button', { name: /^删\s*除$/ }).click();
  await expect(page.getByRole('row', { name: new RegExp(jobName) })).toHaveCount(0, {
    timeout: 20_000,
  });

  // 关键断言：切回报告 Tab，已删任务的报告行必须消失——deleteMutation
  // 已失效 inspectionReportsPrefix（回归时缓存永不重取：Tabs 面板不销毁
  // + 终态报告无轮询 → 幽灵行残留，点「详情」404）。
  await page.getByRole('tab', { name: '报告' }).click();
  await expect(page.getByRole('cell', { name: jobName })).toHaveCount(0, { timeout: 20_000 });

  // API 真值复核：后端已级联删除该任务的全部报告。
  const afterRes = await page.request.get('/api/inspection/reports?limit=50', {
    headers: API_HEADERS,
  });
  expect(afterRes.ok()).toBeTruthy();
  const afterItems = (((await afterRes.json()) ?? { items: [] }).items ?? []) as Array<{
    job_name: string;
  }>;
  expect(afterItems.filter((r) => r.job_name === jobName)).toEqual([]);
});
