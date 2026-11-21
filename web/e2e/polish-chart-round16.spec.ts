// 图表第 16 轮走查的 2 个浏览器可达项：
// 1. 一台机器时钟超前 10 分钟：集群趋势当前桶不得把它的未来样本算成
//    P95/峰值 100%；趋势卡副标题要说明「N 台采样时间超前服务端时钟，
//    未参与趋势统计」，与 Top 12 的「未参与排名」同口径。
// 2. 末个采样比窗口终点早 11 分钟：覆盖提示不得再四舍五入成「24 小时」
//    （与「24 小时窗口」同值），要写「23 小时 49 分钟（…缺 11 分钟采样）」。
//
// 后端 SQL 钳制由 internal/store/postgres 集成测试与 handler 单测覆盖；
// 这里验证真实页面把 overview 的超前台数、trend 的已钳制桶和覆盖文案
// 正确呈现出来。

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

test.use({ storageState: AUTH_STATE_FILE });

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

interface OverviewServer {
  server_id: string;
  server_name: string;
  os: 'linux';
  created_at: string;
  cpu_pct: number;
  mem_pct: number;
  disk_pct: number;
  load_1: number;
  net_in_bps: number;
  net_out_bps: number;
}

interface TrendPoint {
  bucket_start: string;
  cpu_p95: number | null;
  cpu_avg: number | null;
  cpu_max: number | null;
  servers: number;
  samples: number;
}

interface MonitorMockOptions {
  serverTimeMs: number;
  servers: OverviewServer[];
  points: MetricPoint[];
  intervalSeconds?: number;
  trend: {
    window: '24h' | '7d';
    bucket_seconds: number;
    from: string;
    to: string;
    points: TrendPoint[];
  };
}

function overviewServer(
  serverTimeMs: number,
  id: string,
  name: string,
  cpu: number,
): OverviewServer {
  const latest = point(new Date(serverTimeMs - 30_000).toISOString(), cpu);
  return {
    server_id: id,
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

async function mockMonitor(page: Page, options: MonitorMockOptions): Promise<void> {
  const collector = { enabled: true, interval_seconds: options.intervalSeconds ?? 60 };
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: options.servers,
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        server_time: new Date(options.serverTimeMs).toISOString(),
        last_inspection: null,
        collector,
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) => route.fulfill({ json: collector }));
  await page.route('**/api/monitor/trend**', (route) => route.fulfill({ json: options.trend }));
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: options.servers.map((server, index) => ({
          id: server.server_id,
          name: server.server_name,
          os: server.os,
          host: `10.0.0.${index + 16}`,
          port: 22,
        })),
      },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: {
        server_id: options.servers[0]?.server_id,
        minutes: 1440,
        points: options.points,
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

// ---------------------------------------------------------------------------
// 1. 时钟超前的机器：不制造趋势假峰值，且口径写在副标题
// ---------------------------------------------------------------------------

test.describe('图表第 16 轮：时钟超前的未来样本', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('未来样本不入当前桶；趋势卡与 Top 12 都写明 1 台未参与', async ({ page }) => {
    const serverTime = Date.parse('2026-11-21T02:34:56Z');
    const servers: OverviewServer[] = [
      ...Array.from({ length: 5 }, (_, index) =>
        overviewServer(
          serverTime,
          `00000000-0000-0000-0000-00000000010${index}`,
          `batch-worker-0${index + 1}`,
          70 - index,
        ),
      ),
      // 时钟超前 10 分钟：overview 的 latest 是未来时间，CPU 100。
      {
        ...overviewServer(
          serverTime,
          '00000000-0000-0000-0000-000000000111',
          'future-agent-11',
          100,
        ),
        created_at: new Date(serverTime + 10 * 60_000).toISOString(),
      },
    ];
    // 服务端 now 已钳到 02:34:56：当前桶只剩 5 台 150 个样本，
    // P95/峰值来自真实历史点，不再出现 future-agent-11 的 100%。
    const bucketStarts = Array.from({ length: 23 }, (_, index) =>
      new Date(Date.parse('2026-11-20T03:00:00Z') + index * 3_600_000).toISOString(),
    );
    const trend: MonitorMockOptions['trend'] = {
      window: '24h',
      bucket_seconds: 3_600,
      from: '2026-11-20T03:00:00.000Z',
      to: '2026-11-21T03:00:00.000Z',
      points: [
        ...bucketStarts.map((bucketStart) => ({
          bucket_start: bucketStart,
          cpu_p95: 50,
          cpu_avg: 40,
          cpu_max: 60,
          servers: 6,
          samples: 180,
        })),
        {
          bucket_start: '2026-11-21T02:00:00Z',
          cpu_p95: 60,
          cpu_avg: 52.4,
          cpu_max: 94.05,
          servers: 5,
          samples: 150,
        },
      ],
    };
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      servers,
      points: [point(new Date(serverTime - 60_000).toISOString(), 42)],
      trend,
    });

    await page.goto('/monitor');
    const trendCard = cardOf(page, '集群压力趋势');
    const svg = trendCard.locator('svg[role="img"]').first();
    await expect(svg).toBeVisible({ timeout: 20_000 });

    // 趋势副标题说明超前机器未参与统计（与 Top 12 同口径）。
    const futureNote = trendCard.getByTestId('cluster-trend-future-note');
    await expect(futureNote).toBeVisible();
    await expect(futureNote).toContainText(
      '1 台采样时间超前服务端时钟，未参与趋势统计（与 Top 12 同口径）',
    );

    // 聚焦当前桶：tooltip 报 5 台 / 150 样本，峰值 94.1%，没有 100%。
    const lastBucket = trendCard.locator('rect[tabindex="0"]').last();
    await lastBucket.focus();
    const tooltip = trendCard.getByTestId('cluster-trend-tooltip');
    await expect(tooltip).toBeVisible();
    await expect(tooltip).toContainText('采样台数 5 · 样本数 150');
    await expect(tooltip).toContainText(/峰值\s*94\.\d%/);
    await expect(tooltip).not.toContainText('100.0%');

    // 同页 Top 12 仍按同一口径排除这 1 台（副标题与 SVG title 都含这句）。
    const topCard = cardOf(page, '最紧张服务器 Top 12');
    await expect(topCard.getByText('1 台采样时间超前服务端时钟，未参与排名').first()).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 2. 23h49m 覆盖：分钟精度 + 明说缺 11 分钟
// ---------------------------------------------------------------------------

test.describe('图表第 16 轮：覆盖提示分钟精度', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('六张卡的覆盖提示都写 23 小时 49 分钟，不再与 24 小时窗口同值', async ({ page }) => {
    const serverTime = Date.parse('2026-11-21T02:34:56Z');
    const lastSample = serverTime - 11 * 60_000;
    const points: MetricPoint[] = [];
    for (let t = serverTime - 24 * 3_600_000; t <= lastSample; t += 60_000) {
      points.push(point(new Date(t).toISOString(), 42));
    }
    const latest = points[points.length - 1] as MetricPoint;
    const servers = [
      {
        ...overviewServer(
          serverTime,
          '00000000-0000-0000-0000-000000000121',
          'batch-worker-03',
          42,
        ),
        created_at: latest.created_at,
      },
    ];
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      servers,
      points,
      intervalSeconds: 60,
      trend: {
        window: '24h',
        bucket_seconds: 3_600,
        from: '2026-11-20T03:00:00.000Z',
        to: '2026-11-21T03:00:00.000Z',
        points: [
          {
            bucket_start: '2026-11-21T02:00:00Z',
            cpu_p95: 50,
            cpu_avg: 42,
            cpu_max: 60,
            servers: 1,
            samples: 49,
          },
        ],
      },
    });

    await page.goto('/monitor');
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();

    const notes = page.getByTestId('metric-chart-coverage-note');
    await expect(notes).toHaveCount(6, { timeout: 20_000 });
    for (let index = 0; index < 6; index++) {
      await expect(notes.nth(index)).toContainText(
        '数据仅覆盖 23 小时 49 分钟（24 小时窗口内缺 11 分钟采样）',
      );
    }
    // 不能再出现与窗口同值的「覆盖 24 小时」。
    await expect(page.getByText('数据仅覆盖 24 小时')).toHaveCount(0);
  });
});
