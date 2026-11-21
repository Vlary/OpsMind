// 图表第 13 轮走查的 2 个浏览器可达项：
// 1. 跨会话暂停采集：A 的监控页以 30s 轮询的 overview.collector 为准，
//    另一会话暂停后「采集已暂停」横幅、概要卡、过期阈值在一个周期内
//    同源更新；单机折线与集群趋势停止自动轮询。
// 2. 暂停采集后客户端时钟前移 2 小时：单机图时间窗冻结在最后一个
//    采样点，横轴时间与采样时间一致，点不再被钳到绘图区左缘；
//    「最新采样 N 分钟前」仍按真实 now 计算。
//
// 第 3 项（桶末 500µs 事件的 stats 桶数 = 下钻行数）是同口径的 SQL
// 语义，回归测试落在 internal/store/postgres/monitor_agg_integration_test.go
// （真实 PostgreSQL），不在浏览器层重复造桩。

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '66666666-7777-8888-9999-000000000013';

interface CollectorState {
  enabled: boolean;
  interval_seconds: number;
}

interface MetricPoint {
  created_at: string;
  cpu_pct: number;
  mem_pct: number;
  disk_pct: number;
  load_1: number;
  net_in_bps: number;
  net_out_bps: number;
}

function metricPoint(createdAt: string, cpu = 42.5): MetricPoint {
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

function serverRow(point: MetricPoint) {
  return {
    server_id: SERVER_ID,
    server_name: 'chart-round13-host',
    created_at: point.created_at,
    cpu_pct: point.cpu_pct,
    mem_pct: point.mem_pct,
    disk_pct: point.disk_pct,
    load_1: point.load_1,
    net_in_bps: point.net_in_bps,
    net_out_bps: point.net_out_bps,
  };
}

/** 本地时区的 HH:mm（与 MetricChart 的 1h 窗口刻度格式一致）。 */
function localHourMinute(ms: number): string {
  const date = new Date(ms);
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

/** 本地时区的 MM/DD HH:mm:ss（与 formatSampleTime 一致）。 */
function localDateTime(ms: number): string {
  const date = new Date(ms);
  const pad = (value: number) => String(value).padStart(2, '0');
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(
    date.getMinutes(),
  )}:${pad(date.getSeconds())}`;
}

function cardOf(page: Page, title: string) {
  return page
    .locator('.ant-card', {
      has: page.locator('.ant-card-head-title', { hasText: title }),
    })
    .first();
}

/**
 * 监控页桩。collector 接口固定返回页面首屏时的配置（模拟本会话的
 * 一次性快照）；overview 每次请求读取 mutable 的 overviewCollector，
 * 模拟「另一会话此后暂停/恢复」。
 */
async function mockMonitor(
  page: Page,
  options: {
    firstScreenCollector: CollectorState;
    overviewCollector: () => CollectorState;
    points: MetricPoint[];
    onMetricsRequest?: () => void;
    onTrendRequest?: () => void;
  },
): Promise<void> {
  const server = serverRow(options.points[options.points.length - 1] as MetricPoint);
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [server],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        last_inspection: null,
        collector: options.overviewCollector(),
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) =>
    route.fulfill({ json: options.firstScreenCollector }),
  );
  await page.route('**/api/monitor/trend**', (route) => {
    options.onTrendRequest?.();
    return route.fulfill({
      json: {
        window: '24h',
        bucket_seconds: 3_600,
        from: '2026-11-19T00:00:00Z',
        to: '2026-11-20T00:00:00Z',
        points: [],
        truncated: false,
      },
    });
  });
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: [{ id: SERVER_ID, name: 'chart-round13-host', host: '10.0.0.13', port: 22 }],
      },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) => {
    options.onMetricsRequest?.();
    return route.fulfill({
      json: { server_id: SERVER_ID, minutes: 60, points: options.points, truncated: false },
    });
  });
}

// ---------------------------------------------------------------------------
// 1. 跨会话暂停采集：30s 内所有口径同源更新，折线/趋势停止轮询
// ---------------------------------------------------------------------------

test.describe('图表第 13 轮：跨会话暂停采集', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('另一会话暂停后 30s 内横幅/概要卡/阈值更新，折线与趋势不再轮询', async ({ page }) => {
    test.setTimeout(150_000);
    // 3 小时前的采样：60s 间隔阈值 5 分钟、300s 间隔阈值 10 分钟，
    // 两者都会判过期，文案中的阈值数字可区分配置是否已刷新。
    const lastSample = new Date(Date.now() - 3 * 3_600_000).toISOString();
    const points = [metricPoint(lastSample)];
    let overviewCollector: CollectorState = { enabled: true, interval_seconds: 60 };
    let metricsRequests = 0;
    let trendRequests = 0;

    await mockMonitor(page, {
      firstScreenCollector: { enabled: true, interval_seconds: 60 },
      overviewCollector: () => overviewCollector,
      points,
      onMetricsRequest: () => {
        metricsRequests += 1;
      },
      onTrendRequest: () => {
        trendRequests += 1;
      },
    });

    await page.goto('/monitor');
    const collectorStat = page.locator('.ant-statistic', { hasText: '采集器' }).first();
    await expect(collectorStat).toContainText('运行中', { timeout: 20_000 });
    await expect(collectorStat).toContainText('每 60s 一轮');
    await expect(page.getByText(/已超过过期阈值 5 分钟/)).toBeVisible();

    // 另一会话（B）暂停采集并把间隔改为 300s；A 本会话的 collector
    // 接口快照仍是「运行中」。
    overviewCollector = { enabled: false, interval_seconds: 300 };

    // overview 30s 轮询：不刷新页面，横幅与概要卡必须同源更新。
    await expect(page.getByText('采集已暂停：折线展示历史数据，不再自动刷新。')).toBeVisible({
      timeout: 45_000,
    });
    await expect(collectorStat).toContainText('已暂停');
    await expect(collectorStat).toContainText('每 300s 一轮');
    await expect(page.getByText(/已超过过期阈值 10 分钟/)).toBeVisible();
    // 底部「指标采集」卡与概要卡共享同一份新鲜配置（写回 qk.collector）。
    await expect(page.getByText('采集已暂停', { exact: true })).toBeVisible();

    // 暂停生效后折线/趋势不再自动刷新：等过一个完整的 30s/60s 周期。
    // 先让与 overview 同拍触发的最后一轮请求落地，再记录基线。
    await page.waitForTimeout(2_000);
    const metricsBase = metricsRequests;
    const trendBase = trendRequests;
    await page.waitForTimeout(31_000);
    expect(metricsRequests, '采集暂停后单机折线仍在轮询').toBe(metricsBase);
    expect(trendRequests, '采集暂停后集群趋势仍在轮询').toBe(trendBase);
  });
});

// ---------------------------------------------------------------------------
// 2. 暂停采集 + 客户端时钟前移：时间窗冻结在最后一个采样点
// ---------------------------------------------------------------------------

test.describe('图表第 13 轮：暂停后时钟前移', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('暂停 2 小时后打开监控页：窗口终点是最后采样而非当前时钟', async ({ page }) => {
    const lastSampleMs = Date.parse('2026-11-20T12:34:27Z');
    // 70 个点、每分钟一个：最晚 12:34:27Z，最早 11:25:27Z。
    const points = Array.from({ length: 70 }, (_, index) =>
      metricPoint(new Date(lastSampleMs - (69 - index) * 60_000).toISOString(), 1.5),
    );
    const fakeNowMs = lastSampleMs + 2 * 3_600_000;
    await page.clock.install({ time: fakeNowMs });

    await mockMonitor(page, {
      firstScreenCollector: { enabled: false, interval_seconds: 60 },
      overviewCollector: () => ({ enabled: false, interval_seconds: 60 }),
      points,
    });

    await page.goto('/monitor');
    const cpuCard = cardOf(page, 'CPU 使用率');
    const svg = cpuCard.getByTestId('metric-chart-svg');
    await expect(svg).toBeVisible({ timeout: 20_000 });

    const label = (await svg.getAttribute('aria-label')) ?? '';
    // 窗口 1 小时且暂停：终点 = 最后采样时刻，不是 now+2h。
    expect(label).toContain(
      `窗口 ${localHourMinute(lastSampleMs - 3_600_000)} 至 ${localHourMinute(lastSampleMs)}`,
    );
    expect(label).not.toContain(localHourMinute(fakeNowMs));

    // x 轴刻度同样落在真实采样时间上。
    const ticks = await cpuCard
      .locator('[data-testid="metric-chart-time-axis"] text')
      .allTextContents();
    expect(ticks).toContain(localHourMinute(lastSampleMs));
    expect(ticks).not.toContain(localHourMinute(fakeNowMs));

    // 折线不再把 70 个点全钳到左缘：去重后的 x 坐标必须覆盖绘图区。
    const distinctX = await cpuCard
      .locator('[data-testid="metric-chart-series-segment"]')
      .first()
      .evaluate((element) => {
        const pointsAttr = element.getAttribute('points') ?? '';
        const xs = pointsAttr
          .split(' ')
          .filter(Boolean)
          .map((pair) => pair.split(',')[0]);
        return new Set(xs).size;
      });
    expect(distinctX).toBeGreaterThan(10);

    // 年龄仍按真实 now 计算：2 小时前，不是「刚刚」。
    await expect(
      page.getByText(new RegExp(`最新采样 ${localDateTime(lastSampleMs)}（2 小时前`)),
    ).toBeVisible();
    await expect(page.getByText(/已超过过期阈值 5 分钟/)).toBeVisible();
  });
});
