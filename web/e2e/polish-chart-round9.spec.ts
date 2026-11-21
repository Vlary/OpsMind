import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 9 轮走查的 4 个浏览器可达项：
// 1. 巡检跨度 <24h 但同一天两份报告（08:51 与 14:51）：x 轴补 HH:mm，
//    两个「11/19」不再同名；
// 2. 5 份跨天报告的日期标签与分带中心的数据点重合（旧版首/末贴边）；
// 3. 零告警时告警分布走 ChartFrame 空态，不再渲染 24 个 tabIndex=0 空桶；
// 4. 负载轴 0..2e6 统一科学计数（0 | 5.00e5 | 1.00e6 …），不再混排。
//
// 第 1 项（network_device 混入已采集服务器）是后端读路径过滤 + 改类型
// 清理，浏览器 mock 无法覆盖，回归在
// internal/store/postgres/monitor_agg_integration_test.go。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';
const JOB_ID = '22222222-3333-4444-5555-666666666666';

function metricPoint(createdAt: string, load: number) {
  return {
    created_at: createdAt,
    cpu_pct: 30,
    mem_pct: 40,
    disk_pct: 50,
    load_1: load,
    net_in_bps: 1_024,
    net_out_bps: 2_048,
  };
}

function serverRow(latest: ReturnType<typeof metricPoint>) {
  return {
    server_id: SERVER_ID,
    server_name: 'chart-round9-host',
    os: 'linux',
    created_at: latest.created_at,
    cpu_pct: latest.cpu_pct,
    mem_pct: latest.mem_pct,
    disk_pct: latest.disk_pct,
    load_1: latest.load_1,
    net_in_bps: latest.net_in_bps,
    net_out_bps: latest.net_out_bps,
  };
}

async function mockMonitor(page: Page, points: ReturnType<typeof metricPoint>[]): Promise<void> {
  const latest = points[points.length - 1] as ReturnType<typeof metricPoint>;
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [serverRow(latest)],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        last_inspection: null,
        collector: { enabled: true, interval_seconds: 60 },
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) =>
    route.fulfill({ json: { enabled: true, interval_seconds: 60 } }),
  );
  await page.route('**/api/monitor/trend**', (route) =>
    route.fulfill({ json: { window: '24h', bucket_seconds: 3600, points: [], truncated: false } }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: { items: [{ id: SERVER_ID, name: 'chart-round9-host', host: '10.0.0.9', port: 22 }] },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({ json: { server_id: SERVER_ID, points, truncated: false } }),
  );
}

function cardOf(page: Page, title: string) {
  return page
    .locator('.ant-card', {
      has: page.locator('.ant-card-head-title', { hasText: title }),
    })
    .first();
}

test.describe('图表第 9 轮：负载轴统一记法', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('1h 负载 1e6：轴刻度全部科学计数，不混排 500000', async ({ page }) => {
    const now = Date.now();
    await mockMonitor(page, [
      metricPoint(new Date(now - 60_000).toISOString(), 800_000),
      metricPoint(new Date(now).toISOString(), 1_000_000),
    ]);
    await page.goto('/monitor');
    const card = cardOf(page, '负载（1m）');
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const labels = await card.locator('[data-testid="metric-chart-y-tick"]').allTextContents();
    expect(labels).toEqual(['0', '5.00e5', '1.00e6', '1.50e6', '2.00e6']);
    await expect(card.getByText('当前 1.00e6')).toBeVisible();
  });
});

test.describe('图表第 9 轮：零告警空态', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24 个全零桶走 ChartFrame 空态，Tab 序里没有空柱热区', async ({ page }) => {
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/alert-rules', (route) => route.fulfill({ json: { items: [] } }));
    await page.route('**/api/alert-events**', (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith('/stats')) {
        const start = Date.UTC(2026, 10, 19);
        return route.fulfill({
          json: {
            window: '24h',
            bucket_seconds: 3600,
            from: '2026-11-19T00:00:00Z',
            to: '2026-11-20T00:00:00Z',
            total: 0,
            distinct_rules: 0,
            severity_timeline: Array.from({ length: 24 }, (_, index) => ({
              bucket_start: new Date(start + index * 3_600_000).toISOString(),
              info: 0,
              warning: 0,
              critical: 0,
            })),
            top_rules: [],
          },
        });
      }
      return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
    });

    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    const card = cardOf(page, '严重度时间分布');
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card.getByTestId('chart-empty')).toBeVisible();
    await expect(card.getByText('该时间窗内暂无告警事件')).toBeVisible();
    await expect(card.locator('[tabindex="0"]')).toHaveCount(0);
    await expect(card.locator('svg[role="img"]')).toHaveCount(0);
  });
});

function job() {
  return {
    id: JOB_ID,
    name: '业务巡检',
    cron: '5 3 * * *',
    target_tags: [],
    template: 'basic',
    notify_enabled: false,
    notify_channel_id: null,
    enabled: true,
    last_run_at: '2026-11-20T02:13:00Z',
    next_run_at: '2026-11-21T03:05:00Z',
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-20T02:13:00Z',
  };
}

async function mockInspectionReports(
  page: Page,
  points: Array<{ report_id: string; created_at: string }>,
): Promise<void> {
  await page.route('**/api/notification-channels', (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route('**/api/inspection/jobs', (route) =>
    route.fulfill({ json: { items: [job()] } }),
  );
  await page.route('**/api/inspection/reports**', (route) =>
    route.fulfill({
      json: {
        items: points.map((point) => ({
          id: point.report_id,
          job_id: JOB_ID,
          job_name: '业务巡检',
          trigger: 'cron',
          server_count: 5,
          avg_score: 88,
          anomaly_count: 1,
          started_at: point.created_at,
          finished_at: point.created_at,
          created_at: point.created_at,
        })),
      },
    }),
  );
  await page.route('**/api/inspection/trend**', (route) =>
    route.fulfill({
      json: {
        job_id: JOB_ID,
        points: points.map((point) => ({
          ...point,
          avg_score: 88,
          server_count: 5,
          anomaly_count: 1,
          trigger: 'cron',
        })),
        truncated: false,
      },
    }),
  );
}

async function openTrendChart(page: Page) {
  await page.goto('/inspections');
  await page.getByRole('tab', { name: '报告' }).click();
  const card = page.locator('.ant-card', { hasText: '均分走势' }).first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  const svg = card.locator('svg[role="img"]');
  await expect(svg).toBeVisible({ timeout: 20_000 });
  return { card, svg };
}

test.describe('图表第 9 轮：巡检同日标签与 5 份对齐', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('跨度 6h 的同日两份报告：x 轴带 HH:mm 区分 08:51/14:51', async ({ page }) => {
    await mockInspectionReports(page, [
      { report_id: 'rep-morning', created_at: '2026-11-19T08:51:17' },
      { report_id: 'rep-afternoon', created_at: '2026-11-19T14:51:17' },
    ]);
    const { svg } = await openTrendChart(page);
    const labels = await svg
      .locator('[data-testid="inspection-trend-x-axis"] text')
      .allTextContents();
    expect(labels).toEqual(['11/19 08:51', '11/19 14:51']);
  });

  test('5 份跨天报告：首/末日期标签与各自数据点重合', async ({ page }) => {
    const days = ['2026-11-16', '2026-11-17', '2026-11-18', '2026-11-19', '2026-11-20'];
    await mockInspectionReports(
      page,
      days.map((day, index) => ({
        report_id: `rep-${index}`,
        created_at: `${day}T10:00:00`,
      })),
    );
    const { svg } = await openTrendChart(page);
    const geometry = await svg.evaluate((element) => {
      const labels = [
        ...element.querySelectorAll('[data-testid="inspection-trend-x-axis"] text'),
      ].map((node) => ({
        x: Number(node.getAttribute('x')),
        anchor: node.getAttribute('text-anchor'),
      }));
      const circles = [...element.querySelectorAll('circle')]
        .filter((node) => Number(node.getAttribute('cy')) > 0)
        .map((node) => Number(node.getAttribute('cx')));
      return { labels, circles };
    });
    expect(geometry.labels).toHaveLength(5);
    expect(geometry.circles).toHaveLength(5);
    for (let index = 0; index < 5; index++) {
      // 旧版 5 份起首/末强制贴边：首标签 x=38 / 首点 cx=147.6（1440 卡）。
      expect(geometry.labels[index]?.anchor).toBe('middle');
      expect(geometry.labels[index]?.x).toBeCloseTo(geometry.circles[index] as number, 0);
    }
    expect(geometry.labels[0]?.x).toBeGreaterThan(100);
    expect(geometry.labels[4]?.x).toBeLessThan(1_100);
  });
});
