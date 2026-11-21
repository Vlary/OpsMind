// 图表第 17 轮走查的 4 个浏览器可达项：
// 1. latest 时钟超前且 24h 窗口内还有 12 小时前的旧点时，六张单机卡的
//    卡头必须按旧点自己的年龄写「最新（12 小时前）」，不能复用
//    overview.latest 的 selectedSampleStale 写成「当前」；复制按钮同步
//    「最新值」。
// 2. 采集间隔 600s 下 +9 分钟的样本：Top12 不参与排名、年龄文案写
//    「采样时间超前 9 分钟」、趋势卡写明「1 台未参与趋势统计」，三处
//    与趋势 SQL 的 now 钳制同口径。
// 3. 7 天只有约 1.5 天数据时，覆盖提示用 first_sample/last_sample 的
//    实际跨度（1.5 天），不再把首末未满桶当整桶（1.8 天）。
// 4. 1024/390 宽下「网络入流量异常」与「网络出流量异常」截断后仍可
//    区分（前缀保留，入/出不丢）。
//
// 纯逻辑回归见 chartModel.test.ts / chartsRender.test.tsx /
// monitorEmptyState.test.ts；后端字段见 monitor_handlers_test.go 与
// monitor_agg_integration_test.go。

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

interface TrendPayload {
  window: '24h' | '7d';
  bucket_seconds: number;
  from: string;
  to: string;
  first_sample: string | null;
  last_sample: string | null;
  points: Array<{
    bucket_start: string;
    cpu_p95: number | null;
    cpu_avg: number | null;
    cpu_max: number | null;
    servers: number;
    samples: number;
  }>;
}

interface MonitorMockOptions {
  serverTimeMs: number;
  servers: OverviewServer[];
  points: MetricPoint[];
  intervalSeconds?: number;
  trendFor?: (window: string) => TrendPayload;
}

function overviewServer(
  serverTimeMs: number,
  id: string,
  name: string,
  cpu: number,
  overrides: Partial<OverviewServer> = {},
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
    ...overrides,
  };
}

function bucket(
  startMs: number,
  overrides: Partial<TrendPayload['points'][number]> = {},
): TrendPayload['points'][number] {
  return {
    bucket_start: new Date(startMs).toISOString(),
    cpu_p95: 50,
    cpu_avg: 40,
    cpu_max: 60,
    servers: 1,
    samples: 60,
    ...overrides,
  };
}

function trend24h(serverTimeMs: number): TrendPayload {
  const start = Date.parse('2026-11-20T03:00:00Z');
  return {
    window: '24h',
    bucket_seconds: 3_600,
    from: new Date(start).toISOString(),
    to: new Date(start + 24 * 3_600_000).toISOString(),
    first_sample: new Date(start + 10 * 60_000).toISOString(),
    last_sample: new Date(serverTimeMs).toISOString(),
    points: Array.from({ length: 24 }, (_, index) => bucket(start + index * 3_600_000)),
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
  await page.route('**/api/monitor/trend**', (route) => {
    const url = new URL(route.request().url());
    const window = url.searchParams.get('window') === '7d' ? '7d' : '24h';
    const payload = options.trendFor ? options.trendFor(window) : trend24h(options.serverTimeMs);
    return route.fulfill({ json: payload });
  });
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: {
        items: options.servers.map((server, index) => ({
          id: server.server_id,
          name: server.server_name,
          os: server.os,
          host: `10.0.0.${index + 17}`,
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
// 1. 时钟超前 + 窗口内旧历史点：卡头按旧点年龄标「最新」
// ---------------------------------------------------------------------------

test.describe('图表第 17 轮：时钟超前时的卡头年龄', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24h 窗口最后可用点在 12 小时前：卡头写最新（12 小时前），不写当前', async ({ page }) => {
    const serverTime = Date.parse('2026-11-21T06:10:00Z');
    const futureMs = serverTime + 9 * 60_000;
    // 多给 5 分钟：server_time 与浏览器收到时刻的毫秒差会把 12 小时向下
    // 取整成 11 小时（formatSampleAge 按整小时向下取整）。
    const oldMs = serverTime - (12 * 3_600_000 + 300_000);
    const server: OverviewServer = {
      ...overviewServer(serverTime, '00000000-0000-0000-0000-000000000171', 'future-agent-11', 100),
      created_at: new Date(futureMs).toISOString(),
      cpu_pct: 100,
    };
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      servers: [server],
      points: [
        point(new Date(oldMs).toISOString(), 42),
        point(new Date(futureMs).toISOString(), 100),
      ],
    });

    await page.goto('/monitor');
    await page
      .getByLabel('选择单机时间范围')
      .locator('.ant-segmented-item', { hasText: /^24 小时$/ })
      .click();

    // 同页采样行仍指出 latest 时钟超前。
    await expect(page.getByText(/采样时间超前 9 分钟，请检查时钟/)).toBeVisible({
      timeout: 20_000,
    });

    const cpuCard = cardOf(page, 'CPU 使用率');
    const head = cpuCard.locator('.ant-card-head-title');
    await expect(head).toContainText('最新（12 小时前） 42.0%');
    await expect(head).not.toContainText('当前 42.0%');
    // 复制按钮说明同步改「最新值」，不再称当前。
    await expect(cpuCard.getByRole('button', { name: '复制CPU 使用率最新值' })).toBeVisible();
    await expect(cpuCard.getByRole('button', { name: '复制CPU 使用率当前值' })).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 2. 600s 采集间隔 + 9 分钟超前：Top12 / 年龄 / 趋势三处同口径
// ---------------------------------------------------------------------------

test.describe('图表第 17 轮：采集间隔不放大超前容差', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('+9 分钟样本不参与 Top12 与趋势统计，年龄文案写超前', async ({ page }) => {
    const serverTime = Date.parse('2026-11-21T02:08:00Z');
    const futureMs = serverTime + 9 * 60_000;
    const servers: OverviewServer[] = [
      ...Array.from({ length: 12 }, (_, index) =>
        overviewServer(
          serverTime,
          `00000000-0000-0000-0000-00000000018${String(index).padStart(2, '0')}`,
          `batch-worker-${String(index + 1).padStart(2, '0')}`,
          70 - index,
        ),
      ),
      {
        ...overviewServer(
          serverTime,
          '00000000-0000-0000-0000-000000000111',
          'future-agent-11',
          100,
        ),
        created_at: new Date(futureMs).toISOString(),
        cpu_pct: 100,
      },
    ];
    const lastBucketStart = Date.parse('2026-11-21T02:00:00Z');
    const points = Array.from({ length: 24 }, (_, index) =>
      bucket(lastBucketStart - (23 - index) * 3_600_000, {
        servers: index === 23 ? 12 : 12,
        samples: index === 23 ? 12 : 720,
        cpu_p95: index === 23 ? 81.6 : 50,
        cpu_max: index === 23 ? 99.9 : 60,
      }),
    );
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      servers,
      points: [point(new Date(serverTime - 30_000).toISOString(), 42)],
      intervalSeconds: 600,
      trendFor: () => ({
        window: '24h',
        bucket_seconds: 3_600,
        from: new Date(lastBucketStart - 23 * 3_600_000).toISOString(),
        to: new Date(lastBucketStart + 3_600_000).toISOString(),
        first_sample: new Date(lastBucketStart - 23 * 3_600_000 + 60_000).toISOString(),
        last_sample: new Date(serverTime).toISOString(),
        points,
      }),
    });

    await page.goto('/monitor');

    // 趋势卡与 Top12 都写明同 1 台超前机器未参与。
    const trendCard = cardOf(page, '集群压力趋势');
    await expect(trendCard.getByTestId('cluster-trend-future-note')).toContainText(
      '1 台采样时间超前服务端时钟，未参与趋势统计（与 Top 12 同口径）',
      { timeout: 20_000 },
    );
    const topCard = cardOf(page, '最紧张服务器 Top 12');
    await expect(topCard.getByText('1 台采样时间超前服务端时钟，未参与排名').first()).toBeVisible();
    // 未来机器不得出现在排名行（SVG 可见文本与行 aria-label）。
    await expect(topCard.getByText('future-agent-11')).toHaveCount(0);
    const firstRow = topCard.locator('rect[role="img"]').first();
    await expect(firstRow).toHaveAttribute('aria-label', /^第 1 名 batch-worker-/);
    // 末桶 tooltip 与趋势 SQL 一致：12 台/12 样本，没有 100%。
    await trendCard.locator('rect[tabindex="0"]').last().focus();
    const tooltip = trendCard.getByTestId('cluster-trend-tooltip');
    await expect(tooltip).toContainText('采样台数 12 · 样本数 12');
    await expect(tooltip).not.toContainText('100.0%');

    // 手动选中超前机器：年龄文案写「采样时间超前 9 分钟」，而不是
    // staleAgeMs 钳成 0 后的「1 秒前」。
    await page.getByRole('combobox', { name: '选择服务器' }).click();
    await page.getByRole('combobox', { name: '选择服务器' }).fill('future-agent-11');
    await page.locator('.ant-select-item-option', { hasText: 'future-agent-11' }).first().click();
    await expect(page.getByText(/采样时间超前 9 分钟，请检查时钟/)).toBeVisible();
    await expect(page.getByText(/最新采样 .*1 秒前/)).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 3. 7 天覆盖提示用实际首末采样跨度
// ---------------------------------------------------------------------------

test.describe('图表第 17 轮：覆盖提示的实际数据跨度', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('7 天只有 7/28 桶有样本：显示 1.5 天，而不是桶跨度 1.8 天', async ({ page }) => {
    const serverTime = Date.parse('2026-11-21T02:19:11Z');
    const start = Date.parse('2026-11-14T18:00:00Z');
    const firstSample = Date.parse('2026-11-19T09:38:51Z');
    const lastSample = Date.parse('2026-11-20T22:19:11Z');
    const server = overviewServer(
      serverTime,
      '00000000-0000-0000-0000-000000000191',
      'batch-worker-07',
      42,
    );
    await mockMonitor(page, {
      serverTimeMs: serverTime,
      servers: [server],
      points: [point(new Date(serverTime - 60_000).toISOString(), 42)],
      trendFor: (window) => {
        if (window !== '7d') {
          return trend24h(serverTime);
        }
        return {
          window: '7d',
          bucket_seconds: 21_600,
          from: new Date(start).toISOString(),
          to: new Date(start + 28 * 21_600_000).toISOString(),
          first_sample: new Date(firstSample).toISOString(),
          last_sample: new Date(lastSample).toISOString(),
          points: Array.from({ length: 28 }, (_, index) =>
            bucket(
              start + index * 21_600_000,
              index >= 21
                ? {}
                : {
                    cpu_p95: null,
                    cpu_avg: null,
                    cpu_max: null,
                    servers: 0,
                    samples: 0,
                  },
            ),
          ),
        };
      },
    });

    await page.goto('/monitor');
    const trendCard = cardOf(page, '集群压力趋势');
    await trendCard.locator('.ant-segmented-item', { hasText: /^近 7 天$/ }).click();

    const note = trendCard.getByTestId('cluster-trend-coverage-note');
    await expect(note).toBeVisible({ timeout: 20_000 });
    await expect(note).toContainText('有样本 7/28 桶');
    await expect(note).toContainText('数据跨度 1.5 天');
    await expect(note).not.toContainText('1.8 天');
    await expect(note).not.toContainText('覆盖桶跨度');
  });
});

// ---------------------------------------------------------------------------
// 4. 网络入/出流量异常在窄屏仍可区分
// ---------------------------------------------------------------------------

function severityBuckets() {
  const start = Date.UTC(2026, 10, 19);
  return Array.from({ length: 24 }, (_, index) => ({
    bucket_start: new Date(start + index * 3_600_000).toISOString(),
    info: index % 2,
    warning: index % 3,
    critical: index % 5 === 0 ? 1 : 0,
  }));
}

async function mockAlertStats(page: Page): Promise<void> {
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
          bucket_seconds: 3600,
          from: '2026-11-19T00:00:00Z',
          to: '2026-11-20T00:00:00Z',
          total: 23,
          distinct_rules: 2,
          severity_timeline: severityBuckets(),
          top_rules: [
            {
              rule_id: 'rule-net-in',
              rule_name: '网络入流量异常',
              severity: 'info',
              count: 13,
              server_count: 2,
              last_fired_at: '2026-11-19T19:00:00Z',
            },
            {
              rule_id: 'rule-net-out',
              rule_name: '网络出流量异常',
              severity: 'info',
              count: 10,
              server_count: 2,
              last_fired_at: '2026-11-19T18:00:00Z',
            },
          ],
        },
      });
    }
    return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
  });
}

for (const width of [1024, 390] as const) {
  test(`网络入/出流量异常的标签可区分（${width}）`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await mockAlertStats(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();

    const card = cardOf(page, '最吵规则 Top 8');
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    const layout = await card.evaluate((element) => {
      const svg = element.querySelector('svg[role="img"]');
      if (!svg) {
        return null;
      }
      const svgBox = svg.getBoundingClientRect();
      const labels = [...element.querySelectorAll('svg text[text-anchor="end"]')].map((node) => {
        const box = node.getBoundingClientRect();
        return {
          text: node.textContent ?? '',
          left: box.left - svgBox.left,
          right: box.right - svgBox.left,
        };
      });
      return { svgWidth: svgBox.width, labels };
    });
    expect(layout).not.toBeNull();
    const netLabels = (layout?.labels ?? []).filter((label) => label.text.includes('网络'));
    expect(netLabels.length).toBe(2);
    expect(netLabels[0]?.text).not.toBe(netLabels[1]?.text);
    const joined = netLabels.map((label) => label.text).join('|');
    expect(joined).toContain('入');
    expect(joined).toContain('出');
    // 截断后仍落在绘图区左侧，不越界。
    for (const label of layout?.labels ?? []) {
      expect(label.left).toBeGreaterThanOrEqual(0);
      expect(label.right).toBeLessThanOrEqual((layout?.svgWidth ?? 0) + 0.5);
    }
  });
}
