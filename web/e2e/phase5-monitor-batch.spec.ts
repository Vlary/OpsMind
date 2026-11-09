import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// M4 五期 E2E（必做两条，真链路——后端 M1/M3 已注册路由）：
// 1) 监控页能打开：标题/概要卡/采集卡可见；无已采集服务器时展示空态
//    （seed 的 e2e-fake-host 指向 127.0.0.1:1，采集不可达，无 latest 行）。
// 2) 批量页能创建任务并看到进度框架：填命令（safe 预览）→ 选目标 →
//    创建 → 详情抽屉展示 per-server 行（auto_exec=false → 待审批行 +
//    批准/拒绝按钮 + 0/1 进度），随后取消任务清理。

test.use({ storageState: AUTH_STATE_FILE });

/** antd Select 多选：点开、滚到目标项、点选（虚拟滚动同 containers 先例）。 */
async function pickServer(page: Page, name: string) {
  await page.getByRole('combobox', { name: '目标服务器' }).click();
  const option = page.locator('.ant-select-item-option', { hasText: name }).first();
  for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
    await page.keyboard.press('ArrowDown');
  }
  await option.click();
  await page.keyboard.press('Escape');
}

test('监控页能打开并显示概要与空态/数据', async ({ page }) => {
  await page.goto('/monitor');
  await expect(page.getByRole('heading', { name: '监控' })).toBeVisible();

  // 概要卡四张：已采集服务器数 / 触发中告警 / 最近巡检均分 / 采集器。
  await expect(page.getByText('服务器（已采集）')).toBeVisible();
  await expect(page.getByText('触发中告警')).toBeVisible();
  await expect(page.getByText('最近巡检均分')).toBeVisible();
  await expect(page.getByText('采集器')).toBeVisible();

  // 采集配置卡（enabled 开关 + 间隔输入）。
  await expect(page.getByText('指标采集', { exact: true }).first()).toBeVisible();
  await expect(page.getByRole('button', { name: '保存采集配置' })).toBeVisible();

  // seed 环境无可达采集目标：要么空态提示，要么已有服务器数据（时间窗切换器）。
  const emptyState = page.getByText(/暂无服务器指标/);
  const timeWindow = page.getByText('1 小时');
  await expect(emptyState.or(timeWindow).first()).toBeVisible({ timeout: 30_000 });
});

test('批量页能创建任务并看到逐台进度框架', async ({ page }) => {
  await page.goto('/batch');
  await expect(page.getByRole('heading', { name: '批量', exact: true })).toBeVisible();

  // 命令（safe）→ 实时风险预览出现「安全」徽标。
  const commandBox = page.getByPlaceholder('df -h /');
  await expect(commandBox).toBeVisible();
  await commandBox.fill('ls -la');
  await expect(page.getByText('安全').first()).toBeVisible({ timeout: 15_000 });

  // 目标多选：seed 的 e2e-fake-host。
  await pickServer(page, 'e2e-fake-host');

  await page.getByRole('button', { name: '创建并分派' }).click();

  // 详情抽屉：per-server 行 + 待审批（auto_exec=false，safe 命令）+ 进度 0/1。
  // antd v6 Drawer：标题节点不在 .ant-drawer-content 直系，用 dialog role 定位。
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByText('批量任务详情')).toBeVisible({ timeout: 30_000 });
  await expect(drawer.getByText('e2e-fake-host')).toBeVisible();
  await expect(drawer.getByText('待审批').first()).toBeVisible({ timeout: 30_000 });
  // antd 对两字按钮自动插空格（accessible name “批 准”），正则兼容。
  await expect(drawer.getByRole('button', { name: /批\s*准/ }).first()).toBeVisible();
  await expect(drawer.getByText(/0\s*\/\s*1|1\s*\/\s*1/).first()).toBeVisible();

  // 清理：取消任务（pending/awaiting_approval → cancelled），验证取消链路。
  await drawer.getByRole('button', { name: '取消任务' }).first().click();
  const confirmLayer = page.locator('.ant-popover');
  await confirmLayer.getByRole('button', { name: '取消任务' }).click();
  await expect(drawer.getByText('已取消').first()).toBeVisible({ timeout: 30_000 });
});
