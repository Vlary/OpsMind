import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 10 轮走查的 2 个浏览器可达项：
// 1. 30s 采集的 24h 窗口被截断时，卡片必须写「仅返回最近 X 小时（更早的
//    点未返回）」，不能把未返回的历史标成「无采样」；
// 2. 窗口内只有 1 个采样点时，图上按窗口坐标画点并给出时间/值，不再显示
//    「点数不足」。
//
// 第 2 项（latest 被旧样本覆盖）是存储层行为，浏览器 mock 无法覆盖，
// 回归在 internal/store/postgres/phase5_m1_integration_test.go。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '33333333-4444-5555-6666-777777777777';

function metricPoint(createdAt: string, cpu: number) {
  return {
    created_at: createdAt,
    cpu_pct: cpu,
    mem_pct: 40,
    disk_pct: 50,
    load_1: 0.5,
    net_in_bps: 1_024,
    net_out_bps: 2_048,
  };
}

function serverRow(latest: ReturnType<typeof metricPoint>) {
  return {
    server_id: SERVER_ID,
    server_name: 'chart-round10-host',
    created_at: latest.created_at,
    cpu_pct: latest.cpu_pct,
    mem_pct: latest.mem_pct,
    disk_pct: latest.disk_pct,
    load_1: latest.load_1,
    net_in_bps: latest.net_in_bps,
    net_out_bps: latest.net_out_bps,
  };
}

async function mockMonitor(
  page: Page,
  options: {
    points: ReturnType<typeof metricPoint>[];
    truncated: boolean;
    intervalSeconds: number;
  },
): Promise<void> {
  const latest = options.points[options.points.length - 1] as ReturnType<typeof metricPoint>;
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [serverRow(latest)],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        last_inspection: null,
        collector: { enabled: true, interval_seconds: options.intervalSeconds },
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) =>
    route.fulfill({
      json: { enabled: true, interval_seconds: options.intervalSeconds },
    }),
  );
  await page.route('**/api/monitor/trend**', (route) =>
    route.fulfill({ json: { window: '24h', bucket_seconds: 3600, points: [], truncated: false } }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: { items: [{ id: SERVER_ID, name: 'chart-round10-host', host: '10.0.0.10', port: 22 }] },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: {
        server_id: SERVER_ID,
        minutes: 1440,
        points: options.points,
        truncated: options.truncated,
      },
    }),
  );
}

function cardOf(page: Page, title: string) {
  return page
    .locator('.ant-card', {
      has: page.locator('.ant-card-head-title', { hasText: title }),
    })
    .first();
}

function localChartTime(ms: number): string {
  const date = new Date(ms);
  const pad = (value: number) => String(value).padStart(2, '0');
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(
    date.getMinutes(),
  )}`;
}

test.describe('图表第 10 轮：30s 采集 24h 截断文案', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('被截断时写「仅返回最近 12 小时」而不是「无采样」', async ({ page }) => {
    const now = Date.now();
    const points = Array.from({ length: 1440 }, (_, index) =>
      metricPoint(new Date(now - (1439 - index) * 30_000).toISOString(), 20 + (index % 60)),
    );
    await mockMonitor(page, { points, truncated: true, intervalSeconds: 30 });

    await page.goto('/monitor');
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();

    const note = page.getByTestId('metric-chart-coverage-note').first();
    await expect(note).toBeVisible({ timeout: 20_000 });
    await expect(note).toHaveText('仅返回最近 12 小时（更早的点未返回）');
    await expect(page.getByTestId('metric-chart-coverage-note')).toHaveCount(6);

    const pageText = await page.locator('body').innerText();
    expect(pageText).not.toContain('窗口内其余时段无采样');
    expect(pageText).toContain('返回点集已截断（仅最近 12 小时，1440 点）');
  });
});

test.describe('图表第 10 轮：单采样点', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('1 个采样点仍按窗口坐标画点，并显示时间与值', async ({ page }) => {
    const pointTime = Date.now() - 2 * 3_600_000;
    const point = metricPoint(new Date(pointTime).toISOString(), 42.5);
    await mockMonitor(page, { points: [point], truncated: false, intervalSeconds: 60 });

    await page.goto('/monitor');
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();

    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard).toBeVisible({ timeout: 20_000 });
    const note = cpuCard.getByTestId('metric-chart-single-point-note');
    await expect(note).toHaveText(`仅 1 个采样点（${localChartTime(pointTime)}），值 42.5%`);

    // 每张卡都画出该点的圆圈（6 指标各一个），且不再出现「点数不足」。
    await expect(page.getByTestId('metric-chart-series-dot')).toHaveCount(6);
    const pageText = await page.locator('body').innerText();
    expect(pageText).not.toContain('点数不足');
    expect(pageText).not.toContain('暂无采集数据');

    // aria 仍按接口原始点数报数。
    const aria = await cpuCard.locator('svg[role="img"]').getAttribute('aria-label');
    expect(aria).toContain('共 1 个采样点');
    expect(aria).toContain('最新 42.5%');
  });
});
