import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 7 轮走查的 3 个浏览器可达项：
// 1. 新接入/采集稀疏的机器选「24 小时」时，横轴按窗口边界铺满而不是把
//    仅有 2 小时数据拉满整宽，并明确提示数据覆盖范围；
// 2. 巡检走势 1–4 份报告时，x 轴日期标签与绘图区内的报告点对齐；
// 3. 网络卡 y 轴/0 刻度/卡头使用同一套带 B 的字节单位；极端值走 EB/s
//    与 3 位有效数字，不再出现 5000000000G/s / 3.3999999…e+38。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';
const JOB_ID = '33333333-4444-5555-6666-777777777777';

interface MetricRow {
  created_at: string;
  cpu_pct: number;
  mem_pct: number;
  disk_pct: number;
  load_1: number;
  net_in_bps: number;
  net_out_bps: number;
}

function metricPoint(
  createdAt: string,
  values: Partial<Omit<MetricRow, 'created_at'>> = {},
): MetricRow {
  return {
    created_at: createdAt,
    cpu_pct: 30,
    mem_pct: 40,
    disk_pct: 50,
    load_1: 0.6,
    net_in_bps: 1_024,
    net_out_bps: 2_048,
    ...values,
  };
}

function serverRow(latest: MetricRow, serverId = SERVER_ID, name = 'chart-round7-host') {
  return {
    server_id: serverId,
    server_name: name,
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

async function mockMonitor(
  page: Page,
  options: {
    points: MetricRow[];
    latest: MetricRow;
    servers?: unknown[];
  },
): Promise<void> {
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: options.servers ?? [serverRow(options.latest)],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        last_inspection: null,
        collector: { enabled: false, interval_seconds: 60 },
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) =>
    route.fulfill({ json: { enabled: false, interval_seconds: 60 } }),
  );
  await page.route('**/api/monitor/trend**', (route) =>
    route.fulfill({ json: { window: '24h', bucket_seconds: 3600, points: [], truncated: false } }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: [{ id: SERVER_ID, name: 'chart-round7-host', host: '10.0.0.7', port: 22 }],
      },
    }),
  );
  await page.route(`**/api/servers/${SERVER_ID}/metrics*`, (route) =>
    route.fulfill({
      json: { server_id: SERVER_ID, points: options.points, truncated: false },
    }),
  );
}

function cardOf(page: Page, title: string) {
  // 按卡片标题精确匹配：集群趋势卡正文也含「CPU 使用率分桶」，用
  // hasText 会先命中没有指标 SVG 的集群卡。
  return page
    .locator('.ant-card', {
      has: page.locator('.ant-card-head-title', { hasText: title }),
    })
    .first();
}

test.describe('图表第 7 轮：监控折线按选中窗口', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24 小时窗口内只有 2 小时数据：轴铺满窗口 + 覆盖范围提示', async ({ page }) => {
    const now = Date.now();
    const points = [
      metricPoint(new Date(now - 2 * 3_600_000).toISOString(), { cpu_pct: 10 }),
      metricPoint(new Date(now - 3_600_000).toISOString(), { cpu_pct: 20 }),
      metricPoint(new Date(now).toISOString(), { cpu_pct: 30 }),
    ];
    await mockMonitor(page, {
      points,
      latest: points[points.length - 1] as MetricRow,
    });

    await page.goto('/monitor');
    const card = cardOf(page, 'CPU 使用率');
    await expect(card).toBeVisible({ timeout: 20_000 });
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();

    const svg = card.locator('svg[role="img"]');
    await expect(svg).toBeVisible({ timeout: 20_000 });
    // 覆盖提示是可见文本 + aria 的一部分。
    await expect(card.getByTestId('metric-chart-coverage-note')).toHaveText(
      '数据仅覆盖 2 小时（24 小时窗口内其余时段无采样）',
    );
    expect(await svg.getAttribute('aria-label')).toContain('数据仅覆盖 2 小时');

    const geometry = await svg.evaluate((element) => {
      const polyline = element.querySelector('polyline');
      const points = (polyline?.getAttribute('points') ?? '').split(' ').filter(Boolean);
      return {
        width: element.getBoundingClientRect().width,
        xs: points.map((pair) => Number(pair.split(',')[0])),
        labels: [...element.querySelectorAll('[data-testid="metric-chart-time-axis"] text')].map(
          (node) => node.textContent ?? '',
        ),
      };
    });
    expect(geometry.xs).toHaveLength(3);
    // 2h/24h 的数据位于窗口右端 8.3%，旧版会从绘图区左缘 0 起步拉满。
    expect(geometry.xs[0]).toBeGreaterThan(geometry.width * 0.8);
    // 有窗口时按窗口边界等距打刻度（窄卡 3 个、宽卡 5 个），点只有 3 个
    // 时旧实现最多 3 个标签。
    expect(geometry.labels).toHaveLength(geometry.width < 420 ? 3 : 5);
    expect(new Set(geometry.labels).size).toBe(geometry.labels.length);
  });

  test('网络卡单位统一：0 B/s 与 MB/s 同轴，卡头也是带 B 的字节单位', async ({ page }) => {
    const now = Date.now();
    const points = [
      metricPoint(new Date(now - 3_600_000).toISOString(), { net_in_bps: 12_500_000 }),
      metricPoint(new Date(now).toISOString(), { net_in_bps: 50_000_000 }),
    ];
    await mockMonitor(page, { points, latest: points[points.length - 1] as MetricRow });

    await page.goto('/monitor');
    const card = cardOf(page, '网络入');
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });

    const labels = await card.locator('[data-testid="metric-chart-y-tick"]').allTextContents();
    expect(labels).toContain('0 B/s');
    expect(labels.some((label) => label.endsWith(' MB/s'))).toBe(true);
    // 旧版把 12.5M/s 写成丢 B 的短标签。
    expect(labels.some((label) => /^[\d.]+[KMG]\/s$/.test(label))).toBe(false);
    await expect(card.getByText('当前 50.00 MB/s')).toBeVisible();
  });

  test('极端值：轴走 EB/s，卡头走 3 位有效数字', async ({ page }) => {
    const now = Date.now();
    const extreme: Partial<MetricRow> = {
      net_in_bps: 2 ** 63,
      net_out_bps: 2 ** 63,
      load_1: 3.4028235e38,
      cpu_pct: 30,
    };
    const points = [
      metricPoint(new Date(now - 3_600_000).toISOString(), { ...extreme, net_in_bps: 8e18 }),
      metricPoint(new Date(now).toISOString(), extreme),
    ];
    await mockMonitor(page, { points, latest: points[points.length - 1] as MetricRow });

    await page.goto('/monitor');
    const netCard = cardOf(page, '网络入');
    await expect(netCard).toBeVisible({ timeout: 20_000 });
    await expect(netCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const netLabels = await netCard
      .locator('[data-testid="metric-chart-y-tick"]')
      .allTextContents();
    expect(netLabels.some((label) => label.includes('EB/s'))).toBe(true);
    expect(netLabels.some((label) => label.includes('G/s'))).toBe(false);
    await expect(netCard.getByText('当前 9.22 EB/s')).toBeVisible();

    const loadCard = cardOf(page, '负载（1m）');
    await expect(loadCard.getByText('当前 3.40e38')).toBeVisible({ timeout: 20_000 });
    await expect(loadCard.getByText(/3\.3999999/)).toHaveCount(0);
  });
});

function job() {
  return {
    id: JOB_ID,
    name: '每日基线巡检',
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

function trendPoint(index: number) {
  return {
    report_id: `point-${index}`,
    created_at: new Date(Date.UTC(2026, 10, 20, 2, 13 + index)).toISOString(),
    avg_score: 80 + index,
    server_count: 8,
    anomaly_count: 1,
    trigger: 'cron',
  };
}

async function mockInspectionShell(page: Page, pointCount: number): Promise<void> {
  const points = Array.from({ length: pointCount }, (_, index) => trendPoint(index));
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
          job_name: '每日基线巡检',
          trigger: point.trigger,
          server_count: point.server_count,
          avg_score: point.avg_score,
          anomaly_count: point.anomaly_count,
          started_at: point.created_at,
          finished_at: point.created_at,
          created_at: point.created_at,
        })),
      },
    }),
  );
  await page.route('**/api/inspection/trend**', (route) =>
    route.fulfill({ json: { job_id: JOB_ID, points, truncated: false } }),
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

test.describe('图表第 7 轮：巡检小样本轴标签对齐', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('单份报告：日期标签与居中的报告点对齐', async ({ page }) => {
    await mockInspectionShell(page, 1);
    const { svg } = await openTrendChart(page);
    const geometry = await svg.evaluate((element) => {
      const labels = [
        ...element.querySelectorAll('[data-testid="inspection-trend-x-axis"] text'),
      ].map((node) => Number(node.getAttribute('x')));
      const circles = [...element.querySelectorAll('circle')]
        .filter((node) => Number(node.getAttribute('cy')) > 0)
        .map((node) => Number(node.getAttribute('cx')));
      return { labels, circles, width: element.getBoundingClientRect().width };
    });
    expect(geometry.labels).toHaveLength(1);
    expect(geometry.circles).toHaveLength(1);
    // 旧版标签 x=PADDING_LEFT(38)、点在绘图区中心。
    expect(geometry.labels[0]).toBeCloseTo(geometry.circles[0] as number, 0);
    expect(geometry.labels[0]).toBeGreaterThan(geometry.width * 0.4);
  });

  test('3 份报告：每个日期标签都对齐各自的点', async ({ page }) => {
    await mockInspectionShell(page, 3);
    const { svg } = await openTrendChart(page);
    const geometry = await svg.evaluate((element) => {
      const labels = [
        ...element.querySelectorAll('[data-testid="inspection-trend-x-axis"] text'),
      ].map((node) => Number(node.getAttribute('x')));
      const circles = [...element.querySelectorAll('circle')]
        .filter((node) => Number(node.getAttribute('cy')) > 0)
        .map((node) => Number(node.getAttribute('cx')));
      return { labels, circles, width: element.getBoundingClientRect().width };
    });
    expect(geometry.labels).toHaveLength(3);
    expect(geometry.circles).toHaveLength(3);
    for (let index = 0; index < 3; index++) {
      expect(geometry.labels[index]).toBeCloseTo(geometry.circles[index] as number, 0);
    }
    // 首/末标签不再钉在绘图区左右缘。
    expect(geometry.labels[0]).toBeGreaterThan(geometry.width * 0.15);
    expect(geometry.labels[2]).toBeLessThan(geometry.width * 0.9);
  });
});
