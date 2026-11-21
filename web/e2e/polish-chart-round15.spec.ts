// 图表第 15 轮走查的 6 个浏览器可达项：
// 1. Windows 资产的指标卡不再按 Linux 口径（/proc/stat、/proc/meminfo、
//    loadavg 1 分钟）标注，改用 Win32_* / ProcessorQueueLength；
// 2. 停采 12 小时但 24h 窗口内仍有历史点时，卡头不再写「当前」；
// 3. 集群趋势最右侧时间标签不再被 SVG 右缘裁掉（1440/1024/390）；
// 4. 单机指标 hover tooltip 不再盖住覆盖/断档提示行；
// 5. 「最吵规则 Top 8」副标题与 x 轴标题写明柱长是触发次数；
// 6. 7 天窗口只有 6/28 桶有样本时给出覆盖提示。
//
// 纯逻辑回归见 monitorEmptyState.test.ts / chartModel.test.ts /
// chartsRender.test.tsx；这里验证真实页面的布局与文案。

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '99999999-8888-7777-6666-555555555515';

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

interface TrendOption {
  window: '24h' | '7d';
  bucket_seconds: number;
  from: string;
  to: string;
  points: Array<Record<string, unknown>>;
}

interface MonitorMockOptions {
  serverTimeMs: number;
  latest: MetricPoint;
  points: MetricPoint[];
  intervalSeconds?: number;
  os?: 'linux' | 'windows';
  trendFor?: (window: string) => TrendOption;
}

function trendBuckets(
  count: number,
  startMs: number,
  bucketMs: number,
  nonEmptyFrom = -1,
): Array<Record<string, unknown>> {
  return Array.from({ length: count }, (_, index) => {
    const empty = nonEmptyFrom >= 0 && index < nonEmptyFrom;
    return {
      bucket_start: new Date(startMs + index * bucketMs).toISOString(),
      cpu_p95: empty ? null : 40 + index,
      cpu_avg: empty ? null : 30 + index,
      cpu_max: empty ? null : 50 + index,
      servers: empty ? 0 : 2,
      samples: empty ? 0 : 120,
    };
  });
}

async function mockMonitor(page: Page, options: MonitorMockOptions): Promise<void> {
  const collector = { enabled: true, interval_seconds: options.intervalSeconds ?? 60 };
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [
          {
            server_id: SERVER_ID,
            server_name: 'chart-round15-host',
            os: options.os ?? 'linux',
            created_at: options.latest.created_at,
            cpu_pct: options.latest.cpu_pct,
            mem_pct: options.latest.mem_pct,
            disk_pct: options.latest.disk_pct,
            load_1: options.latest.load_1,
            net_in_bps: options.latest.net_in_bps,
            net_out_bps: options.latest.net_out_bps,
          },
        ],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        server_time: new Date(options.serverTimeMs).toISOString(),
        last_inspection: null,
        collector,
      },
    }),
  );
  await page.route('**/api/monitor/collector', (route) => route.fulfill({ json: collector }));
  await page.route('**/api/monitor/trend**', (route) => {
    const url = new URL(route.request().url());
    const window = url.searchParams.get('window') === '7d' ? '7d' : '24h';
    const payload = options.trendFor
      ? options.trendFor(window)
      : {
          window,
          bucket_seconds: window === '7d' ? 21_600 : 3_600,
          from: new Date(options.serverTimeMs - 24 * 3_600_000).toISOString(),
          to: new Date(options.serverTimeMs).toISOString(),
          points: [],
        };
    return route.fulfill({ json: payload });
  });
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: SERVER_ID,
            name: 'chart-round15-host',
            os: options.os ?? 'linux',
            host: '10.0.0.15',
            port: 22,
          },
        ],
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
// 1. Windows 资产按平台标注采集源
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：Windows 指标口径', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('CPU/内存/负载卡写 Win32_* 与 ProcessorQueueLength，不再出现 /proc 与 loadavg', async ({
    page,
  }) => {
    const serverTime = Date.now();
    const latest = point(new Date(serverTime - 60_000).toISOString(), 30);
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest,
      points: [latest],
      os: 'windows',
    });

    await page.goto('/monitor');
    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });

    await expect(cpuCard.getByText('Win32_Processor LoadPercentage（CIM 直取）')).toBeVisible();
    await expect(
      page.getByText('Win32_OperatingSystem（TotalVisibleMemorySize / FreePhysicalMemory）'),
    ).toBeVisible();
    const loadCard = cardOf(page, '处理器队列长度');
    await expect(loadCard).toBeVisible();
    await expect(loadCard.getByText(/ProcessorQueueLength/)).toBeVisible();
    await expect(loadCard.getByText('不是 1 分钟负载均值')).toBeVisible();

    const bodyText = await page.locator('body').innerText();
    expect(bodyText).not.toContain('/proc/stat');
    expect(bodyText).not.toContain('/proc/meminfo');
    expect(bodyText).not.toContain('loadavg 1 分钟');
  });
});

// ---------------------------------------------------------------------------
// 2. 停采 12 小时：卡头不再无条件写「当前」
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：过期样本的卡头标签', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24h 窗口内最后一点在 12 小时前：卡头写「最新（12 小时前）」', async ({ page }) => {
    const serverTime = Date.now();
    // 多给 5 分钟，避免 server_time 与浏览器收到时刻的毫秒差把 12 小时
    // 向下取整成 11 小时（formatSampleAge 按整小时向下取整）。
    const last = serverTime - (12 * 3_600_000 + 300_000);
    const points: MetricPoint[] = [];
    for (let t = serverTime - 23 * 3_600_000; t <= last; t += 300_000) {
      points.push(point(new Date(t).toISOString(), 42));
    }
    const latest = points[points.length - 1] as MetricPoint;

    await mockMonitor(page, { serverTimeMs: serverTime, latest, points, intervalSeconds: 300 });
    await page.goto('/monitor');

    const cpuCard = cardOf(page, 'CPU 使用率');
    await expect(cpuCard).toBeVisible({ timeout: 20_000 });
    // 切到 24 小时：窗口内确实有 12 小时历史点，复现「窗口最后一点 =
    // 12 小时前的值」这一旧实现会写「当前」的场景。默认 1h 窗口内无点，
    // 卡片显示空态（没有 SVG），所以先切窗口再等折线。
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();
    await expect(cpuCard.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await expect(cpuCard.getByText(/最新（12 小时前） 42\.0%/)).toBeVisible({ timeout: 20_000 });
    await expect(cpuCard.getByText('当前 42.0%')).toHaveCount(0);
    // 同页采样摘要仍明确写出过期阈值（300s 间隔 → max(5min, 2×间隔) = 10min）。
    await expect(page.getByText(/已超过过期阈值 10 分钟/)).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 3. 集群趋势末刻度不越出 SVG
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：集群趋势末刻度贴边', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('1440/1024/390 下横轴标签都在 SVG 内（末端不再被裁）', async ({ page }) => {
    const serverTime = Date.parse('2026-11-20T23:40:00Z');
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest: point('2026-11-20T23:39:00Z', 42),
      points: [point('2026-11-20T23:39:00Z', 42)],
      trendFor: (window) => ({
        window: window === '7d' ? '7d' : '24h',
        bucket_seconds: 3_600,
        from: '2026-11-20T00:00:00.000Z',
        to: '2026-11-21T00:00:00.000Z',
        points: trendBuckets(24, Date.parse('2026-11-20T00:00:00Z'), 3_600_000),
      }),
    });

    await page.goto('/monitor');
    const card = cardOf(page, '集群压力趋势');
    const svg = card.locator('svg[role="img"]').first();
    await expect(svg).toBeVisible({ timeout: 20_000 });

    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ width, height: 900 });
      await expect
        .poll(
          () =>
            svg.evaluate((node) => {
              const svgRect = node.getBoundingClientRect();
              let overhang = 0;
              for (const label of node.querySelectorAll(
                '[data-testid="cluster-trend-x-axis"] text',
              )) {
                const rect = label.getBoundingClientRect();
                overhang = Math.max(overhang, rect.right - svgRect.right, svgRect.left - rect.left);
              }
              return overhang;
            }),
          { message: `${width}px 下横轴标签越出 SVG` },
        )
        .toBeLessThanOrEqual(0.5);
      const lastLabel = card.locator('[data-testid="cluster-trend-x-axis"] text').last();
      await expect(lastLabel).toHaveAttribute('text-anchor', 'end');
    }
  });
});

// ---------------------------------------------------------------------------
// 4. tooltip 不覆盖提示行
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：hover tooltip 与提示行', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24h 有 3 小时断档时，tooltip 出现在断档说明下方', async ({ page }) => {
    const serverTime = Date.now();
    const points: MetricPoint[] = [];
    const gapFrom = serverTime - 4 * 3_600_000;
    const gapTo = serverTime - 3_600_000;
    for (let t = serverTime - 24 * 3_600_000; t <= serverTime; t += 300_000) {
      if (t > gapFrom && t < gapTo) {
        continue;
      }
      points.push(point(new Date(t).toISOString(), 30 + ((t / 300_000) % 10)));
    }
    const latest = points[points.length - 1] as MetricPoint;

    await mockMonitor(page, { serverTimeMs: serverTime, latest, points, intervalSeconds: 300 });
    await page.goto('/monitor');

    const cpuCard = cardOf(page, 'CPU 使用率');
    const chart = cpuCard.getByTestId('metric-chart-svg');
    await expect(chart).toBeVisible({ timeout: 20_000 });
    // 断档在 24h 窗口内：先切到 24 小时（默认 1h 只含缺口之后的数据）。
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();
    const gapNote = cpuCard.getByTestId('metric-chart-gap-note');
    await expect(gapNote).toBeVisible({ timeout: 20_000 });
    await expect(gapNote).toContainText('无采样断档');

    const box = await chart.boundingBox();
    expect(box).not.toBeNull();
    await chart.hover({ position: { x: (box?.width ?? 0) * 0.5, y: 90 } });
    const tooltip = cpuCard.getByTestId('metric-chart-tooltip');
    await expect(tooltip).toBeVisible();

    const noteBox = await gapNote.boundingBox();
    const tipBox = await tooltip.boundingBox();
    expect(noteBox).not.toBeNull();
    expect(tipBox).not.toBeNull();
    // tooltip 的顶边必须在提示行底边之下（不再盖住说明文字）。
    expect((tipBox?.y ?? 0) + 0.5).toBeGreaterThanOrEqual(
      (noteBox?.y ?? 0) + (noteBox?.height ?? 0),
    );
  });
});

// ---------------------------------------------------------------------------
// 5. 最吵规则：柱长口径写进副标题与 x 轴
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：最吵规则触发次数口径', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('副标题与 x 轴标题都写明触发次数（次）', async ({ page }) => {
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
            from: '2026-11-20T00:00:00.000Z',
            to: '2026-11-21T00:00:00.000Z',
            server_time: '2026-11-20T23:40:00.000Z',
            total: 120,
            distinct_rules: 2,
            severity_timeline: Array.from({ length: 24 }, (_, index) => ({
              bucket_start: new Date(
                Date.parse('2026-11-20T00:00:00Z') + index * 3_600_000,
              ).toISOString(),
              info: 1,
              warning: 1,
              critical: 0,
            })),
            top_rules: [
              {
                rule_id: 'rule-a',
                rule_name: 'CPU 过高',
                severity: 'critical',
                count: 60,
                server_count: 3,
                last_fired_at: '2026-11-20T22:00:00.000Z',
              },
              {
                rule_id: 'rule-b',
                rule_name: '内存过高',
                severity: 'warning',
                count: 40,
                server_count: 2,
                last_fired_at: '2026-11-20T21:00:00.000Z',
              },
            ],
          },
        });
      }
      return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
    });

    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    const card = cardOf(page, '最吵规则 Top 8');
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });

    await expect(card.getByText(/柱长为窗口内触发次数（按次数降序）/)).toBeVisible();
    await expect(card.getByTestId('noisy-rule-x-title')).toHaveText('触发次数（次）');
    // 刻度仍是 0/20/40/60，轴标题补足单位。
    const axisTexts = await card.locator('svg text').allTextContents();
    expect(axisTexts).toContain('60');
  });
});

// ---------------------------------------------------------------------------
// 6. 7 天窗口部分空桶的覆盖提示
// ---------------------------------------------------------------------------

test.describe('图表第 15 轮：7 天窗口覆盖提示', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('28 桶只有 6 桶有样本时写「有样本 6/28 桶」并解释空桶', async ({ page }) => {
    const serverTime = Date.parse('2026-11-20T22:40:00Z');
    const trendStart = Date.parse('2026-11-14T00:00:00Z');
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      latest: point('2026-11-20T22:39:00Z', 42),
      points: [point('2026-11-20T22:39:00Z', 42)],
      trendFor: (window) =>
        window === '7d'
          ? {
              window: '7d',
              bucket_seconds: 21_600,
              from: new Date(trendStart).toISOString(),
              to: new Date(trendStart + 28 * 21_600_000).toISOString(),
              points: trendBuckets(28, trendStart, 21_600_000, 22),
            }
          : {
              window: '24h',
              bucket_seconds: 3_600,
              from: new Date(serverTime - 24 * 3_600_000).toISOString(),
              to: new Date(serverTime).toISOString(),
              points: trendBuckets(24, serverTime - 24 * 3_600_000, 3_600_000),
            },
    });

    await page.goto('/monitor');
    const card = cardOf(page, '集群压力趋势');
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await expect(card.getByTestId('cluster-trend-coverage-note')).toHaveCount(0);

    await card.locator('.ant-segmented-item', { hasText: '近 7 天' }).click();
    const note = card.getByTestId('cluster-trend-coverage-note');
    await expect(note).toBeVisible({ timeout: 20_000 });
    await expect(note).toContainText('有样本 6/28 桶');
    await expect(note).toContainText('空桶可能是服务刚上线');
  });
});
