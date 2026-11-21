// 图表第 14 轮走查的 6 个浏览器可达项：
// 1. 24h 窗口混有 60s 历史与 600s 新采集间隔（换挡）时不再报几十处断档；
// 2. 浏览器时钟落后 20 分钟：未来样本标「采样时间超前…请检查时钟」，
//    卡片「当前」改用折线窗口内最后一点，Top12 排除超前样本；
// 3. 停采 3 天但 latest 仍在：卡片显示「最新（72 小时前）」，
//    空态显示「窗口内无采样（最近一次采样 72 小时前）」；
// 4. 24h 集群趋势/告警分布横轴补月/日；
// 5. 未走完的最后一桶标「进行中」；
// 6. 600s 采样、1h 窗口完整采集不再误报「仅覆盖 50 分钟」。
//
// 断档识别本身的纯逻辑回归在 metricSeries.test.ts；这里验证页面文案与
// 服务端时间偏移的端到端效果。

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '99999999-8888-7777-6666-555555555514';

interface MetricPoint {
  created_at: string;
  cpu_pct: number;
  mem_pct: number;
  disk_pct: number;
  load_1: number;
  net_in_bps: number;
  net_out_bps: number;
}

function point(createdAt: string, cpu = 42): MetricPoint {
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

function serverRow(latest: MetricPoint, name = 'chart-round14-host') {
  return {
    server_id: SERVER_ID,
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

interface MonitorMockOptions {
  serverTimeMs: number;
  latest: MetricPoint;
  points: MetricPoint[];
  intervalSeconds?: number;
  trendPoints?: Array<Record<string, unknown>>;
  trendFrom?: string;
  trendTo?: string;
}

async function mockMonitor(page: Page, options: MonitorMockOptions): Promise<void> {
  const collector = { enabled: true, interval_seconds: options.intervalSeconds ?? 60 };
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [serverRow(options.latest)],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        server_time: new Date(options.serverTimeMs).toISOString(),
        last_inspection: null,
        collector,
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) => route.fulfill({ json: collector }));
  await page.route('**/api/monitor/trend**', (route) =>
    route.fulfill({
      json: {
        window: '24h',
        bucket_seconds: 3_600,
        from: options.trendFrom ?? new Date(options.serverTimeMs - 24 * 3_600_000).toISOString(),
        to: options.trendTo ?? new Date(options.serverTimeMs).toISOString(),
        points: options.trendPoints ?? [],
      },
    }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: [{ id: SERVER_ID, name: 'chart-round14-host', host: '10.0.0.14', port: 22 }],
      },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: { server_id: SERVER_ID, minutes: 60, points: options.points, truncated: false },
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

// ---------------------------------------------------------------------------
// 1. 采集间隔换挡（60s → 600s）不再误报断档
// ---------------------------------------------------------------------------

test.describe('图表第 14 轮：24h 采集间隔换挡', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('687×60s + 72×600s（12h+12h）不报断档，也不提示覆盖不足', async ({ page }) => {
    const serverTime = Date.now();
    const points: MetricPoint[] = [];
    // 前 12 小时每 60s，后 12 小时每 600s，正好覆盖 24h 窗口。
    let t = serverTime - 24 * 3_600_000;
    points.push(point(new Date(t).toISOString()));
    for (let index = 0; index < 720; index++) {
      t += 60_000;
      points.push(point(new Date(t).toISOString()));
    }
    for (let index = 0; index < 72; index++) {
      t += 600_000;
      points.push(point(new Date(t).toISOString()));
    }
    const latest = points[points.length - 1] as MetricPoint;

    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest,
      points,
      intervalSeconds: 600,
    });

    await page.goto('/monitor');
    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });

    const pageText = await page.locator('body').innerText();
    expect(pageText).not.toContain('无采样断档');
    expect(pageText).not.toContain('其余时段无采样');
    await expect(cpuCard.getByTestId('metric-chart-gap-note')).toHaveCount(0);
    await expect(cpuCard.getByTestId('metric-chart-coverage-note')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 2. 浏览器时钟落后 20 分钟：未来样本不冒充当前值
// ---------------------------------------------------------------------------

test.describe('图表第 14 轮：未来样本与时钟偏差', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('未来 20 分钟的 latest 标「请检查时钟」；卡片当前值取折线最后一点', async ({ page }) => {
    const serverTime = Date.now();
    // 浏览器时钟比服务端慢 20 分钟（页面内所有 Date.now 都走假时钟）。
    await page.clock.install({ time: serverTime - 20 * 60_000 });
    const points = Array.from({ length: 60 }, (_, index) =>
      point(new Date(serverTime - (59 - index) * 60_000).toISOString(), 20),
    );
    const latest = point(new Date(serverTime + 20 * 60_000).toISOString(), 11.1);

    await mockMonitor(page, { serverTimeMs: serverTime, latest, points });
    await page.goto('/monitor');

    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/采样时间超前 20 分钟，请检查时钟/)).toBeVisible();
    // 卡片「当前」与折线同源（20%），不是 overview 里未来样本的 11.1%。
    await expect(cpuCard.getByText('当前 20.0%')).toBeVisible();
    await expect(page.getByText('当前 11.1%')).toHaveCount(0);
    // Top12 排除超前样本；唯一一台机器被排除后给出可读空态。
    await expect(
      page.getByText('暂无可用采样：1 台采样时间超前服务端时钟，请检查时钟'),
    ).toBeVisible({ timeout: 20_000 });
  });
});

// ---------------------------------------------------------------------------
// 3. 停采 3 天：区分「停采」与「从未采集」
// ---------------------------------------------------------------------------

test.describe('图表第 14 轮：停采 3 天的 latest', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('卡头写「最新（72 小时前）」，空态写「最近一次采样 72 小时前」', async ({ page }) => {
    const serverTime = Date.now();
    // 多给 5 分钟，避免 server_time 与浏览器收到时刻的毫秒差把 72 小时
    // 向下取整成 71 小时（formatSampleAge 按整小时向下取整）。
    const latest = point(new Date(serverTime - (72 * 3_600_000 + 300_000)).toISOString(), 70);
    await mockMonitor(page, { serverTimeMs: serverTime, latest, points: [] });

    await page.goto('/monitor');
    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard.getByText(/最新（72 小时前） 70\.0%/)).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('窗口内无采样（最近一次采样 72 小时前）').first()).toBeVisible();
    await expect(page.getByText('暂无采集数据')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 4/5. 24h 横轴补日期 + 未走完桶标「进行中」
// ---------------------------------------------------------------------------

function trendPointsAround() {
  const lastBucket = Date.parse('2026-11-20T22:00:00Z');
  return Array.from({ length: 24 }, (_, index) => ({
    bucket_start: new Date(lastBucket - (23 - index) * 3_600_000).toISOString(),
    cpu_p95: 40 + index,
    cpu_avg: 30 + index,
    cpu_max: 50 + index,
    servers: 2,
    samples: 120,
  }));
}

test.describe('图表第 14 轮：24h 横轴与进行中桶', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('集群趋势横轴带月/日，最后一桶 aria 标「进行中」', async ({ page }) => {
    const serverTime = Date.parse('2026-11-20T22:40:00Z');
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest: point('2026-11-20T22:39:00Z', 42),
      points: [point('2026-11-20T22:39:00Z', 42)],
      trendPoints: trendPointsAround(),
      trendFrom: '2026-11-19T22:00:00.000Z',
      trendTo: '2026-11-20T23:00:00.000Z',
    });

    await page.goto('/monitor');
    const trendCard = cardOf(page, '集群压力趋势');
    await expect(trendCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const texts = await trendCard.locator('svg text').allTextContents();
    expect(texts.some((text) => /\d{2}\/\d{2} \d{2}:\d{2}/.test(text))).toBe(true);
    await expect(trendCard.locator('[data-in-progress="true"]')).toHaveCount(1);
    const aria = await trendCard.locator('[data-in-progress="true"]').getAttribute('aria-label');
    expect(aria).toContain('（进行中）');
  });
});

test.describe('图表第 14 轮：告警分布横轴与进行中桶', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24h 横轴带月/日，未走完的最后一桶标「进行中」', async ({ page }) => {
    const buckets = trendPointsAround();
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/alert-rules', (route) => route.fulfill({ json: { items: [] } }));
    await page.route('**/api/alert-events**', (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith('/stats')) {
        return route.fulfill({
          json: {
            window: '24h',
            bucket_seconds: 3_600,
            from: '2026-11-19T22:00:00.000Z',
            to: '2026-11-20T23:00:00.000Z',
            server_time: '2026-11-20T22:40:00.000Z',
            total: 24,
            distinct_rules: 1,
            severity_timeline: buckets.map((bucket) => ({
              bucket_start: bucket.bucket_start,
              info: 1,
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
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    // <g> 不是 HTMLElement，innerText 不可用；取子 <text> 的文本内容。
    const axisTexts = await card
      .getByTestId('alert-severity-x-axis')
      .locator('text')
      .allTextContents();
    expect(axisTexts.join(' ')).toMatch(/\d{2}\/\d{2} \d{2}:\d{2}/);
    await expect(card.locator('[data-in-progress="true"]')).toHaveCount(1);
    const aria = await card.locator('[data-in-progress="true"]').getAttribute('aria-label');
    expect(aria).toContain('（进行中）');
  });
});

// ---------------------------------------------------------------------------
// 6. 600s 采样、1h 窗口完整采集
// ---------------------------------------------------------------------------

test.describe('图表第 14 轮：600s 采样的覆盖率容差', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('1h 窗口按 600s 完整采集（覆盖 50 分钟）不再提示缺采', async ({ page }) => {
    const serverTime = Date.now();
    // 首点比窗口起点晚 10 分钟，末点正好在窗口末尾：覆盖 50 分钟。
    const points = Array.from({ length: 6 }, (_, index) =>
      point(new Date(serverTime - (5 - index) * 600_000).toISOString(), 42),
    );
    const latest = points[points.length - 1] as MetricPoint;
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest,
      points,
      intervalSeconds: 600,
    });

    await page.goto('/monitor');
    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await expect(cpuCard.getByTestId('metric-chart-coverage-note')).toHaveCount(0);
    expect(await page.locator('body').innerText()).not.toContain('其余时段无采样');
  });
});
