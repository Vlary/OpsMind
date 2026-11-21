import { expect, test } from './fixtures';

import { AUTH_STATE_FILE, OPERATOR_STATE_FILE } from './helpers';

// 图表第 5 轮走查的 5 个浏览器可达项：
// 1. 弱网/高延迟下切换时间窗：六张指标卡显示同高度加载占位，不闪
//    「暂无采集数据」（窗口数据未到前卡头显示 overview 的「最新（年龄）」，
//    到达后切回「当前 …」；第 14 轮起窗口无点不再冒充「当前」）；
// 2. /api/monitor/overview 500：只保留错误横幅，不再叠加「请先添加资产」
//    的资产空态；
// 3. 390 触屏点按：单机指标图、集群趋势、巡检均分走势都能钉住 tooltip，
//    再点取消；
// 4. 24h 读屏文案按接口原始 1440 点报数，并注明绘制 500 点；
// 5. 只有 monitor:read 的运维：采集卡只读，页面不出现可提交表单。
//
// 第 4 项「useForm 未连接 Form」的开发构建警告在 vite preview 产物里被
// antd 编译期裁掉，浏览器断言无法复现；对应回归放在单测
// collectorCardForm.test.ts 静态检查 useForm 的调用位置，这里只验证
// 只读分支的可见行为。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';

function monitoredServer(name: string, cpu: number) {
  return {
    server_id: SERVER_ID,
    server_name: name,
    created_at: new Date().toISOString(),
    cpu_pct: cpu,
    mem_pct: 34.5,
    disk_pct: 56.7,
    load_1: 0.42,
    net_in_bps: 12_300,
    net_out_bps: 6_800,
  };
}

function overview(servers: unknown[]) {
  return {
    servers,
    firing: { total: 0, critical: 0, warning: 0, info: 0 },
    last_inspection: null,
    collector: { enabled: true, interval_seconds: 60 },
  };
}

function metricPoints(count: number, stepMs = 60_000) {
  const now = Date.now();
  return Array.from({ length: count }, (_, index) => ({
    created_at: new Date(now - (count - 1 - index) * stepMs).toISOString(),
    cpu_pct: 12.3,
    mem_pct: 34.5,
    disk_pct: 56.7,
    load_1: 0.42,
    net_in_bps: 12_300,
    net_out_bps: 6_800,
  }));
}

function trendPoints(count: number) {
  const start = Date.UTC(2026, 10, 19);
  return Array.from({ length: count }, (_, index) => ({
    bucket_start: new Date(start + index * 3_600_000).toISOString(),
    cpu_p95: 20 + index * 4,
    cpu_avg: 15 + index * 3,
    cpu_max: 25 + index * 5,
    servers: 3,
    samples: 180,
  }));
}

test.describe('图表第 5 轮：弱网加载态', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('切换时间窗时六张卡显示加载占位，不出现「暂无采集数据」', async ({ page }) => {
    await page.route('**/api/monitor/overview', (route) =>
      route.fulfill({ json: overview([monitoredServer('staging-worker-01', 12.3)]) }),
    );
    // 每次指标请求延迟 2s，模拟弱网；初始加载与切换 6h 都能观察到占位。
    await page.route(/\/api\/servers\/[^/]+\/metrics/, async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 2_000));
      await route.fulfill({
        json: { server_id: SERVER_ID, points: metricPoints(2), truncated: false },
      });
    });

    await page.goto('/monitor');
    // 卡片标题精确匹配「CPU 使用率」：集群趋势卡副标题也含该子串。
    const cpuCard = page
      .locator('.ant-card')
      .filter({ has: page.getByText('CPU 使用率', { exact: true }) })
      .first();
    await expect(cpuCard).toBeVisible({ timeout: 20_000 });
    // 窗口数据尚未到达：卡头显示 overview 的「最新（年龄）」而不是
    // 「当前」——不能用 overview 的值冒充当前窗口的读数。
    await expect(cpuCard).toContainText('最新（');
    await expect(cpuCard).toContainText('12.3%');

    const loading = page.getByTestId('metric-chart-loading');
    await expect(loading).toHaveCount(6, { timeout: 1_500 });
    await expect(page.getByText('暂无采集数据')).toHaveCount(0);

    const charts = page.locator('[data-testid="metric-chart-svg"]');
    await expect(charts).toHaveCount(6, { timeout: 15_000 });
    await expect(loading).toHaveCount(0);
    // 数据到达后卡头才是「当前」。
    await expect(cpuCard).toContainText('当前 12.3%');

    // 切 6h：新 query key 尚无数据，仍是加载占位而不是窗口空态；
    // 窗口数据到达前卡头回到「最新（年龄）」。
    await page.locator('.ant-segmented-item', { hasText: /^6 小时$/ }).click();
    await expect(loading).toHaveCount(6, { timeout: 1_500 });
    await expect(cpuCard).toContainText('最新（');
    await expect(page.getByText('暂无采集数据')).toHaveCount(0);
    await expect(charts).toHaveCount(6, { timeout: 15_000 });
    await expect(cpuCard).toContainText('当前 12.3%');
  });
});

test.describe('图表第 5 轮：overview 失败不误报资产空态', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('500 时只显示错误横幅，不叠加「请先添加资产并开启采集」', async ({ page }) => {
    await page.route('**/api/monitor/overview', (route) =>
      route.fulfill({
        status: 500,
        json: {
          error: { code: 'INTERNAL', message: 'boom', request_id: 'test-rid', details: null },
        },
      }),
    );

    await page.goto('/monitor');
    await expect(page.getByText('服务器内部错误（boom）')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('request_id: test-rid')).toBeVisible();
    // 接口故障被当成「没有资产」会让用户去添加资产/开启采集，做无效操作。
    await expect(page.getByText(/暂无服务器指标/)).toHaveCount(0);
    await expect(page.getByText(/添加资产并开启采集/)).toHaveCount(0);
  });
});

test.describe('图表第 5 轮：390 触屏点按 tooltip', () => {
  test.use({
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
  });

  test('指标图 / 集群趋势 / 巡检走势都能点按钉住并再点取消', async ({ page }) => {
    await page.route('**/api/monitor/overview', (route) =>
      route.fulfill({ json: overview([monitoredServer('staging-worker-01', 12.3)]) }),
    );
    await page.route(/\/api\/servers\/[^/]+\/metrics/, (route) =>
      route.fulfill({
        json: { server_id: SERVER_ID, points: metricPoints(6, 10 * 60_000), truncated: false },
      }),
    );
    await page.route('**/api/monitor/trend**', (route) =>
      route.fulfill({
        json: {
          window: '24h',
          bucket_seconds: 3_600,
          from: '2026-11-19T00:00:00Z',
          to: '2026-11-20T00:00:00Z',
          points: trendPoints(8),
        },
      }),
    );
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/inspection/jobs', (route) =>
      route.fulfill({
        json: {
          items: [
            {
              id: 'job-1',
              name: '每日基线巡检',
              cron: '5 3 * * *',
              target_tags: [],
              template: 'basic',
              notify_enabled: false,
              notify_channel_id: null,
              enabled: true,
              last_run_at: '2026-11-19T02:13:00Z',
              next_run_at: '2026-11-20T03:05:00Z',
              created_at: '2026-11-01T00:00:00Z',
              updated_at: '2026-11-19T02:13:00Z',
            },
          ],
        },
      }),
    );
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/inspection/trend**', (route) =>
      route.fulfill({
        json: {
          job_id: 'job-1',
          points: Array.from({ length: 5 }, (_, index) => ({
            report_id: `report-${index}`,
            created_at: new Date(Date.UTC(2026, 10, 19, index * 2)).toISOString(),
            avg_score: 82 + index,
            server_count: 8,
            anomaly_count: 1,
            trigger: 'cron',
          })),
          truncated: false,
        },
      }),
    );

    await page.goto('/monitor');

    // ① 单机指标图：点按绘图区中部，tooltip 出现；再点同一位置取消。
    // 卡片标题精确匹配「CPU 使用率」：集群趋势卡副标题也含该子串。
    const cpuCard = page
      .locator('.ant-card')
      .filter({ has: page.getByText('CPU 使用率', { exact: true }) })
      .first();
    const metricSvg = cpuCard.getByTestId('metric-chart-svg');
    await expect(metricSvg).toBeVisible({ timeout: 20_000 });
    // 390 宽时单机卡在集群态势下方，先滚到视口内再取触点坐标。
    await metricSvg.scrollIntoViewIfNeeded();
    const metricBox = await metricSvg.boundingBox();
    expect(metricBox).not.toBeNull();
    const metricTap = {
      position: { x: (metricBox?.width ?? 0) * 0.62, y: (metricBox?.height ?? 0) * 0.5 },
    };
    await metricSvg.tap(metricTap);
    const metricTooltip = cpuCard.getByTestId('metric-chart-tooltip');
    await expect(metricTooltip).toBeVisible();
    await metricSvg.tap(metricTap);
    await expect(metricTooltip).toHaveCount(0);

    // ② 集群趋势：点按第 2 个桶。
    const trendCard = page.locator('.ant-card', { hasText: '集群压力趋势' }).first();
    await expect(trendCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const trendHit = trendCard.locator('svg rect[tabindex="0"]').nth(1);
    await expect(trendHit).toHaveCount(1);
    await trendHit.tap();
    const trendTooltip = trendCard.getByTestId('cluster-trend-tooltip');
    await expect(trendTooltip).toBeVisible();
    await expect(trendTooltip).toContainText('P95');
    await trendHit.tap();
    await expect(trendTooltip).toHaveCount(0);

    // ③ 巡检均分走势：点按第 2 份报告。
    await page.goto('/inspections');
    await page.getByRole('tab', { name: '报告' }).click();
    const inspectionCard = page.locator('.ant-card', { hasText: '均分走势' }).first();
    await expect(inspectionCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const inspectionHit = inspectionCard.locator('svg rect[tabindex="0"]').nth(1);
    await expect(inspectionHit).toHaveCount(1);
    await inspectionHit.tap();
    const inspectionTooltip = inspectionCard.getByTestId('inspection-trend-tooltip');
    await expect(inspectionTooltip).toBeVisible();
    await expect(inspectionTooltip).toContainText('均分');
    await inspectionHit.tap();
    await expect(inspectionTooltip).toHaveCount(0);
  });
});

test.describe('图表第 5 轮：24h 读屏采样点口径', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('aria 报 1440 个原始采样点（绘制 500），不把降采样当数据量', async ({ page }) => {
    await page.route('**/api/monitor/overview', (route) =>
      route.fulfill({ json: overview([monitoredServer('prod-api-01', 12.3)]) }),
    );
    await page.route(/\/api\/servers\/[^/]+\/metrics/, async (route) => {
      const url = new URL(route.request().url());
      const minutes = Number(url.searchParams.get('minutes') ?? '60');
      await route.fulfill({
        json: {
          server_id: SERVER_ID,
          points: minutes === 1440 ? metricPoints(1440) : metricPoints(2),
          truncated: false,
        },
      });
    });

    await page.goto('/monitor');
    // 卡片标题精确匹配「CPU 使用率」：集群趋势卡副标题也含该子串。
    const cpuCard = page
      .locator('.ant-card')
      .filter({ has: page.getByText('CPU 使用率', { exact: true }) })
      .first();
    const svg = cpuCard.getByTestId('metric-chart-svg');
    await expect(svg).toBeVisible({ timeout: 20_000 });
    await expect(svg).toHaveAttribute('aria-label', /共 2 个采样点/);

    const requested = page.waitForResponse(
      (response) => response.url().includes('/metrics') && response.url().includes('minutes=1440'),
    );
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();
    await requested;
    await expect(svg).toHaveAttribute('aria-label', /共 1440 个采样点（绘制 500）/);
  });
});

test.describe('图表第 5 轮：运维只读采集卡', () => {
  test.use({ storageState: OPERATOR_STATE_FILE, viewport: { width: 1440, height: 900 } });

  test('只有 monitor:read 时渲染只读回显，不渲染可提交表单', async ({ page }) => {
    const consoleMessages: string[] = [];
    page.on('console', (message) => {
      if (message.type() === 'warning' || message.type() === 'error') {
        consoleMessages.push(message.text());
      }
    });
    await page.route('**/api/monitor/overview', (route) => route.fulfill({ json: overview([]) }));
    await page.route(
      (url) => url.pathname === '/api/monitor/collector',
      (route) => route.fulfill({ json: { enabled: true, interval_seconds: 60 } }),
    );

    await page.goto('/monitor');
    const collectorCard = page.locator('.ant-card', { hasText: '指标采集' }).first();
    await expect(collectorCard.getByTestId('collector-readonly')).toBeVisible({
      timeout: 20_000,
    });
    await expect(collectorCard.getByText('当前角色只读，不能修改采集配置')).toBeVisible();
    await expect(collectorCard.locator('form')).toHaveCount(0);
    await expect(collectorCard.getByRole('button', { name: '保存采集配置' })).toHaveCount(0);

    // 生产产物会被 antd 裁掉开发期警告；这里仍守住「console 里不得出现
    // useForm 未连接」的可见契约，源码级保证见 collectorCardForm.test.ts。
    const useFormWarnings = consoleMessages.filter(
      (message) => message.includes('useForm') && message.includes('not connected'),
    );
    expect(useFormWarnings, useFormWarnings.join('\n')).toEqual([]);
  });
});
