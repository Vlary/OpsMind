import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 8 轮走查的 3 个浏览器可达项：
// 1. 单机 24h 折线中采集中断 3 小时：折线在缺口处断开（不再一条直线跨过
//    空白期），缺口处与图下方给出缺失时长，覆盖率按实际样本算（21/24h）；
// 2. Top12 里 0.0% 且采样过期的机器与刚采到的 0.0% 机器仍可区分：过期行
//    有固定大小的行首警告标记与 warning 色文字，不依赖 0 宽柱体；
// 3. 「最吵规则」下钻后事件表筛选 Tag 用中文展示名「采集不可达（系统）」，
//    不再显示被截断的英文规则键。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';

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

function serverRow(latest: MetricRow, serverId = SERVER_ID, name = 'chart-round8-host') {
  return {
    server_id: serverId,
    server_name: name,
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
    servers?: unknown[];
  },
): Promise<void> {
  const latest = options.points[options.points.length - 1] as MetricRow;
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: options.servers ?? [serverRow(latest)],
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
      json: {
        items: [{ id: SERVER_ID, name: 'chart-round8-host', host: '10.0.0.8', port: 22 }],
      },
    }),
  );
  // 服务器下拉切换/默认选中不改变 mock：任何 id 都返回同一组采样。
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: { server_id: SERVER_ID, points: options.points, truncated: false },
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

test.describe('图表第 8 轮：单机折线断档', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('24h 内断采 3 小时：折线断成两段、提示缺失时长、覆盖率 21 小时', async ({ page }) => {
    const now = Date.now();
    const start = now - 24 * 3_600_000;
    const gapFrom = start + 13 * 3_600_000;
    const gapTo = start + 16 * 3_600_000;
    const points: MetricRow[] = [];
    for (let t = start; t <= now; t += 5 * 60_000) {
      if (t > gapFrom && t < gapTo) {
        continue;
      }
      points.push(metricPoint(new Date(t).toISOString(), { cpu_pct: 20 + ((t / 60_000) % 30) }));
    }
    // 289 个 5 分钟槽位，缺口内（13:05–15:55）跳过 35 个。
    expect(points.length).toBe(254);

    await mockMonitor(page, { points });
    await page.goto('/monitor');
    const card = cardOf(page, 'CPU 使用率');
    await expect(card).toBeVisible({ timeout: 20_000 });
    await page.locator('.ant-segmented-item', { hasText: /^24 小时$/ }).click();
    const svg = card.locator('svg[role="img"]');
    await expect(svg).toBeVisible({ timeout: 20_000 });

    // 缺口说明：不依赖屏幕宽度，直接写出缺失时长。
    const gapNote = card.getByTestId('metric-chart-gap-note');
    await expect(gapNote).toBeVisible();
    await expect(gapNote).toContainText('无采样断档');
    await expect(gapNote).toContainText('3 小时');
    // 覆盖率按实际样本：24h - 3h = 21h（旧口径恒等于首末跨度 24h）。
    await expect(card.getByTestId('metric-chart-coverage-note')).toHaveText(
      '数据仅覆盖 21 小时（24 小时窗口内其余时段无采样）',
    );

    const geometry = await svg.evaluate((element) => {
      const segments = [...element.querySelectorAll('[data-testid="metric-chart-series-segment"]')];
      const maxDx = segments.flatMap((segment) => {
        const xs = (segment.getAttribute('points') ?? '')
          .split(' ')
          .filter(Boolean)
          .map((pair) => Number(pair.split(',')[0]));
        return xs.slice(1).map((x, index) => Math.abs(x - (xs[index] as number)));
      });
      return {
        segments: segments.length,
        gapBands: element.querySelectorAll('[data-testid="metric-chart-gap-band"]').length,
        maxDx,
        aria: element.getAttribute('aria-label') ?? '',
      };
    });

    // 旧版只有一条 polyline，253 点中含一段跨 3 小时空白期的 ~40px 直线。
    expect(geometry.segments).toBe(2);
    expect(geometry.gapBands).toBe(1);
    for (const dx of geometry.maxDx) {
      expect(dx).toBeLessThan(8);
    }
    expect(geometry.aria).toContain('断档');
    expect(geometry.aria).toContain('数据仅覆盖 21 小时');
  });
});

test.describe('图表第 8 轮：Top12 过期零值可见', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('0.0% 的过期机器有行首警告标记与 warning 色，刚采到的 0.0% 没有', async ({ page }) => {
    const now = Date.now();
    const staleRow = {
      server_id: 'stale-idle-id',
      server_name: 'idle-standby',
      cpu_pct: 0,
      mem_pct: 0,
      disk_pct: 0,
      created_at: new Date(now - 9 * 3_600_000).toISOString(),
    };
    const freshRow = {
      server_id: 'fresh-batch-id',
      server_name: 'batch-04',
      cpu_pct: 0,
      mem_pct: 0,
      disk_pct: 0,
      created_at: new Date(now).toISOString(),
    };
    const latest = points(now);
    await mockMonitor(page, { points: latest, servers: [staleRow, freshRow] });

    await page.goto('/monitor');
    const card = cardOf(page, '最紧张服务器 Top 12');
    await expect(card).toBeVisible({ timeout: 20_000 });
    const svg = card.locator('svg[role="img"]');
    await expect(svg).toBeVisible({ timeout: 20_000 });

    const marker = svg.locator('[data-testid="top-server-stale-marker"]');
    await expect(marker).toHaveCount(1, { timeout: 20_000 });

    const styles = await svg.evaluate((element) => {
      const markerCircle = element.querySelector(
        '[data-testid="top-server-stale-marker"] circle',
      ) as SVGCircleElement | null;
      const texts = [...element.querySelectorAll('text')];
      const idle = texts.find((node) => node.textContent === 'idle-standby') ?? null;
      const fresh = texts.find((node) => node.textContent === 'batch-04') ?? null;
      const barWidths = [...element.querySelectorAll('rect[rx="3"]')].map((node) =>
        Number(node.getAttribute('width')),
      );
      return {
        markerFill: markerCircle === null ? null : getComputedStyle(markerCircle).fill,
        idleFill: idle === null ? null : getComputedStyle(idle).fill,
        freshFill: fresh === null ? null : getComputedStyle(fresh).fill,
        barWidths,
      };
    });

    // 两行都是 0.0%，柱体宽度为 0（旧版唯一能看出来源差异的样式不可见）。
    expect(styles.barWidths.length).toBeGreaterThanOrEqual(2);
    expect(styles.barWidths.every((width) => width === 0)).toBe(true);
    // 过期行：行首标记与标签文字都取 warning 色；刚采到的一行不是。
    expect(styles.idleFill).not.toBeNull();
    expect(styles.idleFill).toBe(styles.markerFill);
    expect(styles.idleFill).not.toBe(styles.freshFill);

    // 读屏同样能区分：只有过期行带「已过期」。
    const staleRowNode = svg.locator('[role="img"][aria-label*="idle-standby"]');
    await expect(staleRowNode).toHaveAttribute('aria-label', /已过期 9 小时/);
    const freshRowNode = svg.locator('[role="img"][aria-label*="batch-04"]');
    await expect(freshRowNode).not.toHaveAttribute('aria-label', /已过期/);
  });
});

function points(now: number): MetricRow[] {
  return [
    metricPoint(new Date(now - 3_600_000).toISOString(), { cpu_pct: 0 }),
    metricPoint(new Date(now).toISOString(), { cpu_pct: 0 }),
  ];
}

// ---------------------------------------------------------------------------
// 3. 最吵规则下钻 Tag 使用中文展示名
// ---------------------------------------------------------------------------

const SENTINEL = 'monitor.collector.unreachable';

async function mockAlerts(page: Page): Promise<void> {
  await page.route('**/api/notification-channels', (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route('**/api/alert-rules', (route) => route.fulfill({ json: { items: [] } }));
  await page.route('**/api/alert-events**', (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/stats')) {
      return route.fulfill({
        json: {
          window: '7d',
          bucket_seconds: 21_600,
          from: '2026-11-13T00:00:00Z',
          to: '2026-11-20T00:00:00Z',
          total: 21,
          distinct_rules: 1,
          severity_timeline: [],
          top_rules: [
            {
              rule_id: 'rule-sentinel',
              rule_name: SENTINEL,
              severity: 'warning',
              count: 21,
              server_count: 3,
              last_fired_at: '2026-11-19T19:00:00Z',
            },
          ],
        },
      });
    }
    return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
  });
}

test.describe('图表第 8 轮：规则下钻 Tag 展示名', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('点击「采集不可达（系统）」后筛选 Tag 不再显示英文规则键', async ({ page }) => {
    await mockAlerts(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    await page.locator('.ant-segmented-item', { hasText: /^7 天$/ }).click();

    const card = page.locator('.ant-card', { hasText: '最吵规则 Top 8' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    const ruleButton = card.locator('svg rect[role="button"]').first();
    await expect(ruleButton).toBeVisible({ timeout: 20_000 });
    await ruleButton.click();

    const tag = page.locator('.ant-tag', { hasText: '规则：' }).first();
    await expect(tag).toBeVisible({ timeout: 20_000 });
    await expect(tag).toHaveText('规则：采集不可达（系统）');
    await expect(tag).toHaveAttribute('aria-label', '清除规则筛选 采集不可达（系统）');
    await expect(tag).not.toContainText('monitor.');
  });
});
