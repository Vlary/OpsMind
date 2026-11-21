// 图表第 11 轮走查的 6 个浏览器可达项：
// 1. 390 宽下告警分布/集群趋势的桶命中改为整张绘图区 nearest-x（不再由
//    约 11px 的透明矩形决定点按下钻的时段）；
// 2. 采集器接口慢/失败时概要卡不再谎报「运行中」，用 overview 同源配置兜底；
// 3. 「最近巡检均分」补报告时间与年龄，5 天前的旧结果不再像刚跑过；
// 4. 999,999,999,999 B/s 显示 1.00 TB/s，不再出现 1000.00 GB/s；
// 5. 单机 1/6/24 小时切换的 radiogroup 有中文 accessible name；
// 6. 断网（fetch TypeError）显示「网络异常」而不是「服务器内部错误」。

import type { Page, Route } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '44444444-5555-6666-7777-888888888888';

function localMonthDayTime(value: string | number): string {
  const date = new Date(value);
  const pad = (part: number) => String(part).padStart(2, '0');
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(
    date.getMinutes(),
  )}`;
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

function metricPoint(createdAt: string, overrides: Partial<MetricPoint> = {}): MetricPoint {
  return {
    created_at: createdAt,
    cpu_pct: 42.5,
    mem_pct: 40,
    disk_pct: 50,
    load_1: 0.5,
    net_in_bps: 1_024,
    net_out_bps: 2_048,
    ...overrides,
  };
}

function serverRow(point: MetricPoint) {
  return {
    server_id: SERVER_ID,
    server_name: 'chart-round11-host',
    created_at: point.created_at,
    cpu_pct: point.cpu_pct,
    mem_pct: point.mem_pct,
    disk_pct: point.disk_pct,
    load_1: point.load_1,
    net_in_bps: point.net_in_bps,
    net_out_bps: point.net_out_bps,
  };
}

const DEFAULT_COLLECTOR = { enabled: true, interval_seconds: 60 };

interface MonitorMockOptions {
  overviewServers?: ReturnType<typeof serverRow>[];
  overviewCollector?: { enabled: boolean; interval_seconds: number };
  lastInspection?: { report_id: string; avg_score: number; created_at: string } | null;
  metricsPoints?: MetricPoint[];
  trendPoints?: Array<Record<string, unknown>>;
  /** collector 路由：延迟、直接失败或正常返回。 */
  collector?: {
    delayMs?: number;
    abort?: boolean;
    body?: { enabled: boolean; interval_seconds: number };
  };
  overviewDelayMs?: number;
}

async function mockMonitor(page: Page, options: MonitorMockOptions = {}): Promise<void> {
  const overview = {
    servers: options.overviewServers ?? [],
    firing: { total: 0, critical: 0, warning: 0, info: 0 },
    last_inspection: options.lastInspection ?? null,
    collector: options.overviewCollector ?? DEFAULT_COLLECTOR,
  };
  await page.route('**/api/monitor/overview', async (route) => {
    if (options.overviewDelayMs) {
      await new Promise((resolve) => setTimeout(resolve, options.overviewDelayMs));
    }
    await route.fulfill({ json: overview });
  });
  await page.route('**/api/monitor/collector', async (route) => {
    if (options.collector?.abort) {
      await route.abort('failed');
      return;
    }
    if (options.collector?.delayMs) {
      await new Promise((resolve) => setTimeout(resolve, options.collector?.delayMs));
    }
    await route.fulfill({
      json: options.collector?.body ?? options.overviewCollector ?? DEFAULT_COLLECTOR,
    });
  });
  await page.route('**/api/monitor/trend**', (route) =>
    route.fulfill({
      json: {
        window: '24h',
        bucket_seconds: 3_600,
        from: '2026-11-19T00:00:00Z',
        to: '2026-11-20T00:00:00Z',
        points: options.trendPoints ?? [],
        truncated: false,
      },
    }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: [{ id: SERVER_ID, name: 'chart-round11-host', host: '10.0.0.11', port: 22 }],
      },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: {
        server_id: SERVER_ID,
        minutes: 60,
        points: options.metricsPoints ?? [],
        truncated: false,
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

/** 在命中面上点按第 index 个桶的中心（命中面覆盖整张绘图区）。 */
async function tapBucket(surface: ReturnType<Page['getByTestId']>, index: number, count: number) {
  const box = await surface.boundingBox();
  expect(box).not.toBeNull();
  if (!box) {
    throw new Error('bucket hit surface has no box');
  }
  await surface
    .page()
    .mouse.click(box.x + ((index + 0.5) / count) * box.width, box.y + box.height / 2);
  return box;
}

// ---------------------------------------------------------------------------
// 1. 390 宽：整张绘图区 nearest-x 命中
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：390 宽桶命中', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('告警分布：命中面覆盖整个绘图区，点按最近桶写入正确时段', async ({ page }) => {
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/alert-rules', (route) => route.fulfill({ json: { items: [] } }));
    await page.route('**/api/alert-events**', (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith('/stats')) {
        const isWeek = url.searchParams.get('window') === '7d';
        const count = isWeek ? 28 : 24;
        const bucketSeconds = isWeek ? 21_600 : 3_600;
        return route.fulfill({
          json: {
            window: isWeek ? '7d' : '24h',
            bucket_seconds: bucketSeconds,
            from: '2026-11-13T00:00:00Z',
            to: '2026-11-20T00:00:00Z',
            total: 99,
            distinct_rules: 1,
            severity_timeline: Array.from({ length: count }, (_, index) => ({
              bucket_start: new Date(
                Date.UTC(2026, 10, 13) + index * bucketSeconds * 1_000,
              ).toISOString(),
              info: 1 + (index % 2),
              warning: index % 3,
              critical: index % 5 === 0 ? 1 : 0,
            })),
            top_rules: [],
          },
        });
      }
      return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
    });

    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    const section = page.locator('section[aria-label="告警分布"]');
    await section.locator('.ant-segmented-item', { hasText: '7 天' }).click();

    const surface = page.getByTestId('alert-severity-hit-surface');
    await expect(surface).toBeVisible({ timeout: 20_000 });
    // 旧实现每个桶一个约 11px 的透明 rect；新命中面应覆盖整个绘图区。
    const box = await tapBucket(surface, 17, 28);
    expect(box.width).toBeGreaterThan(240);

    const after = new Date(Date.UTC(2026, 10, 13) + 17 * 21_600 * 1_000);
    const before = new Date(after.getTime() + 6 * 3_600_000);
    const expected = `时段：${localMonthDayTime(after)}–${localMonthDayTime(before)}`;
    await expect(page.getByText(expected, { exact: false })).toBeVisible();
  });

  test('集群趋势：命中面覆盖整个绘图区，点按最近桶钉住对应 tooltip', async ({ page }) => {
    const start = Date.UTC(2026, 10, 19);
    const points = Array.from({ length: 24 }, (_, index) => ({
      bucket_start: new Date(start + index * 3_600_000).toISOString(),
      cpu_p95: 30 + index,
      cpu_avg: 20 + index,
      cpu_max: 40 + index,
      servers: 2,
      samples: 60,
    }));
    const latest = metricPoint(new Date().toISOString());
    await mockMonitor(page, {
      overviewServers: [serverRow(latest)],
      metricsPoints: [latest],
      trendPoints: points,
    });

    await page.goto('/monitor');
    const card = page.locator('.ant-card', { hasText: '集群压力趋势' }).first();
    const surface = card.getByTestId('cluster-trend-hit-surface');
    await expect(surface).toBeVisible({ timeout: 20_000 });
    const box = await tapBucket(surface, 9, points.length);
    expect(box.width).toBeGreaterThan(240);

    const tooltip = page.getByTestId('cluster-trend-tooltip');
    await expect(tooltip).toBeVisible();
    const selected = points[9];
    expect(selected).toBeDefined();
    await expect(tooltip).toContainText(
      localMonthDayTime(Date.parse(String(selected?.bucket_start))),
    );
  });
});

// ---------------------------------------------------------------------------
// 2. 采集器概要卡：加载/失败不得谎报运行中
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：采集器概要卡状态', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  function collectorStat(page: Page) {
    return page.locator('.ant-statistic', { hasText: '采集器' }).first();
  }

  test('采集器缓慢且实际已暂停：加载时显示 —，数据到达后显示已暂停', async ({ page }) => {
    const paused = { enabled: false, interval_seconds: 60 };
    await mockMonitor(page, {
      overviewCollector: paused,
      overviewDelayMs: 3_000,
      collector: { delayMs: 4_000, body: paused },
    });

    await page.goto('/monitor');
    const stat = collectorStat(page);
    await expect(stat).toBeVisible();
    // 两个查询都未就绪：不允许先渲染「运行中」。
    await expect(stat).toContainText('—');
    await expect(stat).not.toContainText('运行中');

    // overview 先返回（3s），其 collector 已是 paused：卡片必须立即纠正。
    await expect(stat).toContainText('已暂停', { timeout: 10_000 });
    await expect(stat).not.toContainText('运行中');
  });

  test('采集器接口失败但 overview 返回已暂停：仍显示已暂停', async ({ page }) => {
    const paused = { enabled: false, interval_seconds: 60 };
    await mockMonitor(page, {
      overviewCollector: paused,
      collector: { abort: true },
    });

    await page.goto('/monitor');
    const stat = collectorStat(page);
    await expect(stat).toContainText('已暂停', { timeout: 10_000 });
    await expect(stat).not.toContainText('运行中');
  });
});

// ---------------------------------------------------------------------------
// 3. 最近巡检均分：显示报告时间与年龄
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：巡检报告时间', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('5 天前的报告显示时间与「5 天前」，不再只显示 87分', async ({ page }) => {
    const createdAt = new Date(Date.now() - 5 * 24 * 3_600_000 - 3_600_000).toISOString();
    await mockMonitor(page, {
      lastInspection: {
        report_id: '55555555-6666-7777-8888-999999999999',
        avg_score: 87,
        created_at: createdAt,
      },
    });

    await page.goto('/monitor');
    const card = page.locator('.ant-card', { hasText: '最近巡检均分' }).first();
    const time = card.getByTestId('inspection-report-time');
    await expect(time).toContainText(`报告 ${localMonthDayTime(createdAt)}`);
    await expect(time).toContainText('5 天前');
    // 超过 24h 用 warning 色，提示这是旧结果。
    await expect(time).toHaveClass(/warning/);
  });
});

// ---------------------------------------------------------------------------
// 4. 网络速率单位边界
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：网络速率单位边界', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('999,999,999,999 B/s 显示 1.00 TB/s，不出现 1000.00 GB/s', async ({ page }) => {
    const point = metricPoint(new Date().toISOString(), {
      net_in_bps: 1_000_000_000_000,
      net_out_bps: 999_999_999_999,
    });
    await mockMonitor(page, {
      overviewServers: [serverRow(point)],
      metricsPoints: [point],
    });

    await page.goto('/monitor');
    const outCard = cardOf(page, '网络出');
    await expect(outCard).toContainText('当前 1.00 TB/s', { timeout: 20_000 });
    await expect(outCard).not.toContainText('1000.00 GB/s');
    await expect(cardOf(page, '网络入')).toContainText('当前 1.00 TB/s');
  });
});

// ---------------------------------------------------------------------------
// 5. 单机时间范围切换的中文分组名
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：时间范围切换可访问名', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('radiogroup 的 accessible name 是「选择单机时间范围」', async ({ page }) => {
    await mockMonitor(page);
    await page.goto('/monitor');

    const group = page.getByRole('radiogroup', { name: '选择单机时间范围' });
    await expect(group).toBeVisible();
    await expect(group).toContainText('1 小时');
    await expect(group).toContainText('24 小时');
  });
});

// ---------------------------------------------------------------------------
// 6. 客户端网络失败不再冒充服务端 5xx
// ---------------------------------------------------------------------------

test.describe('图表第 11 轮：网络失败文案', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('overview/collector 断网显示「网络异常」而非「服务器内部错误」', async ({ page }) => {
    await page.route('**/api/monitor/overview', (route: Route) => route.abort('failed'));
    await page.route('**/api/monitor/collector', (route: Route) => route.abort('failed'));

    await page.goto('/monitor');
    await expect(page.getByText('网络异常，请检查连接后重试').first()).toBeVisible({
      timeout: 30_000,
    });
    const text = await page.locator('body').innerText();
    expect(text).not.toContain('服务器内部错误');
    expect(text).not.toContain('Failed to fetch');
  });
});
