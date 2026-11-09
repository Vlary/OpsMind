import { expect, test } from '@playwright/test';

import {
  ALERTS_FINAL_PREFIX,
  ALERTS_FINAL_SUFFIX,
  AUTH_STATE_FILE,
  BATCH_FINAL_PREFIX,
  BATCH_FINAL_SUFFIX,
  DIAGNOSIS_FINAL_PREFIX,
  DIAGNOSIS_FINAL_SUFFIX,
  METRICS_FINAL_PREFIX,
  METRICS_FINAL_SUFFIX,
  SCENARIO_ALERTS,
  SCENARIO_BATCH,
  SCENARIO_DIAGNOSIS,
  SCENARIO_METRICS,
  sendMessage,
} from './helpers';

// 五期打磨 sprint 1：四条对话工具链的 E2E 真链路覆盖（fake-llm 四场景 +
// 真后端工具执行），此前五期对话工具零 e2e，专属视图与深链是盲区：
// 1) get_metrics / list_alerts：两步链（list_servers 取真实 server_id）→
//    工具卡成功 + 最终文案书挡；
// 2) batch_execute：工具卡成功 + 专属视图（状态 Tag/共 N 台/待审批计数，
//    sprint 4 起与批量页同口径中文）直渲染在折叠面板外（默认收起的「结果」面板内用户发现不了）+
//    「查看批量任务」链接 target=_blank（新标签打开不触发 ChatPage 的
//    streaming 离开守卫，审批独立于会话流不受影响）；
// 3) run_diagnosis：同上，验证「查看诊断会话」深链。
// 资产为 global-setup 播种的 e2e-fake-host（127.0.0.1:1，SSH 不可达）：
// get_metrics 走库内时间序列（无数据也算成功）、batch_execute 对 safe
// 命令 + auto_exec=false 分派为逐台待审批、run_diagnosis 引擎对不可达
// 目标也会产出报告——均不需要真实 SSH。

test.use({ storageState: AUTH_STATE_FILE });

test('__scenario_metrics__：list_servers → get_metrics 工具链成功并给出结论文案', async ({
  page,
}) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_METRICS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const listCard = page.locator('.chat-toolcall-card', { hasText: 'list_servers' }).first();
  await expect(listCard).toBeVisible();
  await expect(listCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

  const metricsCard = page.locator('.chat-toolcall-card', { hasText: 'get_metrics' }).first();
  await expect(metricsCard).toBeVisible();
  await expect(metricsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

  await expect(page.getByText(METRICS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(METRICS_FINAL_SUFFIX)).toBeVisible();
});

test('__scenario_alerts__：list_alerts 工具链成功并给出结论文案', async ({ page }) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_ALERTS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const alertsCard = page.locator('.chat-toolcall-card', { hasText: 'list_alerts' }).first();
  await expect(alertsCard).toBeVisible();
  await expect(alertsCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

  await expect(page.getByText(ALERTS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(ALERTS_FINAL_SUFFIX)).toBeVisible();
});

test('__scenario_batch__：batch_execute 专属视图直渲染 + 新标签深链不触发离开守卫', async ({
  page,
}) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_BATCH);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const batchCard = page.locator('.chat-toolcall-card', { hasText: 'batch_execute' }).first();
  await expect(batchCard).toBeVisible();
  await expect(batchCard.getByText('成功')).toBeVisible({ timeout: 30_000 });

  // 专属视图提升在折叠面板外：不展开任何折叠面板，「结果」默认收起时
  // 状态计数与链接仍直接可见（打磨 #3——此前藏在收起面板内发现不了）。
  await expect(batchCard.getByText('共 1 台')).toBeVisible({ timeout: 15_000 });
  // sprint 4：计数行与批量页同口径中文（RESULT_STATUS_LABELS，待审批 1）。
  await expect(batchCard.getByText(/待审批\s*1/)).toBeVisible();

  // 「查看批量任务」深链（打磨 #2）：target=_blank 新标签打开——SPA 的
  // useBlocker 离开守卫只拦路由内导航，新标签不经过，审批也不受影响。
  const deepLink = batchCard.getByRole('link', { name: '查看批量任务' });
  await expect(deepLink).toBeVisible();
  await expect(deepLink).toHaveAttribute('target', '_blank');
  await expect(deepLink).toHaveAttribute('rel', /noopener/);
  const href = await deepLink.getAttribute('href');
  expect(href).toMatch(/^\/batch\?job=[0-9a-f-]{36}$/);

  // 点击：新标签打开批量任务详情，原会话页不弹「当前回答仍在进行」。
  const popupPromise = page.waitForEvent('popup', { timeout: 20_000 });
  await deepLink.click();
  const popup = await popupPromise;
  await expect(popup).toHaveURL(/\/batch\?job=[0-9a-f-]{36}/, { timeout: 20_000 });
  // 深链直达任务详情抽屉：e2e-fake-host 待审批行可见。
  const popupDrawer = popup.getByRole('dialog');
  await expect(popupDrawer.getByText('e2e-fake-host')).toBeVisible({ timeout: 30_000 });
  await expect(popupDrawer.getByText('待审批').first()).toBeVisible();

  // 原页面无离开确认弹窗（打磨 #2 的回归防线：链接绝不触发 blocker）。
  await expect(page.getByRole('dialog', { name: '当前回答仍在进行' })).toHaveCount(0);

  await expect(page.getByText(BATCH_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(BATCH_FINAL_SUFFIX)).toBeVisible();
});

test('__scenario_diagnosis__：run_diagnosis 专属视图直渲染 + 新标签深链', async ({ page }) => {
  await page.goto('/chat');
  await sendMessage(page, SCENARIO_DIAGNOSIS);
  await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

  const diagCard = page.locator('.chat-toolcall-card', { hasText: 'run_diagnosis' }).first();
  await expect(diagCard).toBeVisible();
  // 诊断引擎轮询至终态（上限 120s），给足窗口。
  await expect(diagCard.getByText('成功')).toBeVisible({ timeout: 60_000 });

  // 专属视图直渲染（折叠面板外）：查看诊断会话链接 + target=_blank。
  const deepLink = diagCard.getByRole('link', { name: /查看诊断会话/ });
  await expect(deepLink).toBeVisible();
  await expect(deepLink).toHaveAttribute('target', '_blank');
  await expect(deepLink).toHaveAttribute('rel', /noopener/);
  const href = await deepLink.getAttribute('href');
  expect(href).toMatch(/^\/alerts\?diagnosis=[0-9a-f-]{36}$/);

  await expect(page.getByText(DIAGNOSIS_FINAL_PREFIX)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(DIAGNOSIS_FINAL_SUFFIX)).toBeVisible();
});
