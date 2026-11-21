import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 12 轮走查的 2 个浏览器可达项：
// 1. 规则下钻与柱子同口径：24h 图上「触发 21 次」，事件表此前只带 rule_id，
//    展示全时 71 条（首屏 50 + 加载更早），且无任何窗口提示。现在点击规则会
//    同时带上 stats.from/to，Tag 写「范围：近 24 小时」；默认全时列表显式标
//    「全部时间」并说明图上只统计当前窗口。切窗口时规则下钻一并清空。
// 2. 390 宽 Top 12 的过期圆点与服务器名首字符重叠：过期行标签再让 12px，
//    ASCII 按 6px/字符保守估算，标签左缘不进入圆点的 3–9px 区间。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';
const RULE_ID = '22222222-3333-4444-5555-666666666666';
const RULE_NAME = 'CPU 五分钟均值告警';

interface AlertEventRow {
  id: number;
  rule_id: string;
  rule_name: string;
  server_id: string;
  server_name: string;
  metric: string;
  status: 'firing' | 'resolved';
  severity: 'info' | 'warning' | 'critical';
  value: number;
  message: string;
  fired_at: string;
  resolved_at: string | null;
  last_notified_at: string | null;
  notify_status: 'none' | 'pending' | 'sent' | 'failed' | 'skipped';
  created_at: string;
  updated_at: string;
}

function alertEvent(
  id: number,
  firedAt: string,
  ruleId = RULE_ID,
  ruleName = RULE_NAME,
): AlertEventRow {
  return {
    id,
    rule_id: ruleId,
    rule_name: ruleName,
    server_id: SERVER_ID,
    server_name: 'web-prod-01',
    metric: 'cpu_pct',
    status: 'resolved',
    severity: 'warning',
    value: 92.5,
    message: 'CPU 五分钟均值超过阈值 90%',
    fired_at: firedAt,
    resolved_at: null,
    last_notified_at: null,
    notify_status: 'sent',
    created_at: firedAt,
    updated_at: firedAt,
  };
}

/** 24h 窗口内该规则的 21 条事件（01:00–21:00 每小时一条）。 */
function ruleWindowEvents(): AlertEventRow[] {
  return Array.from({ length: 21 }, (_, index) => {
    const hour = String(index + 1).padStart(2, '0');
    return alertEvent(1000 - index, `2026-11-19T${hour}:00:00Z`);
  });
}

/** 全时列表首屏 50 条：最早一条落在 11/18，证明它不属于 24h 窗口。 */
function allTimeEvents(): AlertEventRow[] {
  return Array.from({ length: 50 }, (_, index) =>
    alertEvent(
      900 - index,
      new Date(Date.UTC(2026, 10, 19, 23, 59) - index * 3_600_000).toISOString(),
    ),
  );
}

function statsBody(window: '24h' | '7d') {
  const is7d = window === '7d';
  return {
    window,
    bucket_seconds: is7d ? 21_600 : 3_600,
    from: is7d ? '2026-11-13T00:00:00Z' : '2026-11-19T00:00:00Z',
    to: '2026-11-20T00:00:00Z',
    total: is7d ? 70 : 26,
    distinct_rules: is7d ? 9 : 6,
    severity_timeline: is7d
      ? Array.from({ length: 28 }, (_, index) => ({
          bucket_start: new Date(Date.UTC(2026, 10, 13) + index * 21_600_000).toISOString(),
          info: index % 3,
          warning: index % 4,
          critical: index % 7 === 0 ? 1 : 0,
        }))
      : Array.from({ length: 24 }, (_, index) => ({
          bucket_start: new Date(Date.UTC(2026, 10, 19) + index * 3_600_000).toISOString(),
          info: index % 2,
          warning: index % 3,
          critical: index % 5 === 0 ? 1 : 0,
        })),
    top_rules: [
      {
        rule_id: RULE_ID,
        rule_name: RULE_NAME,
        severity: 'warning',
        count: is7d ? 40 : 21,
        server_count: 3,
        last_fired_at: '2026-11-19T21:00:00Z',
      },
      ...Array.from({ length: 7 }, (_, index) => ({
        rule_id: `rule-${index}`,
        rule_name: `业务规则 ${index + 1}`,
        severity: ['critical', 'warning', 'info'][index % 3],
        count: 18 - index,
        server_count: 2,
        last_fired_at: '2026-11-19T18:00:00Z',
      })),
    ],
  };
}

async function mockAlerts(page: Page) {
  const listRequests: URL[] = [];
  await page.route('**/api/notification-channels', (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route('**/api/alert-rules', (route) => route.fulfill({ json: { items: [] } }));
  await page.route('**/api/alert-events**', (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/stats')) {
      const window = url.searchParams.get('window') === '7d' ? '7d' : '24h';
      return route.fulfill({ json: statsBody(window) });
    }
    listRequests.push(url);
    if (url.searchParams.get('rule_id') === RULE_ID) {
      return route.fulfill({
        json: { items: ruleWindowEvents(), has_more: false, next_before_id: null },
      });
    }
    return route.fulfill({
      json: { items: allTimeEvents(), has_more: true, next_before_id: 851 },
    });
  });
  return listRequests;
}

test.describe('图表第 12 轮：规则下钻与柱子同口径', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('默认列表标「全部时间」；点规则后带窗口边界、21 行且无更早页', async ({ page }) => {
    const listRequests = await mockAlerts(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();

    // 图表计数写明窗口，默认列表显式标「全部时间」，两者不再无说明并列。
    await expect(
      page.getByText('近 24 小时共 26 条事件 / 6 条规则。', { exact: false }),
    ).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('.ant-tag', { hasText: '全部时间' })).toBeVisible();
    await expect(
      page.getByText('事件表为全部时间；上方图表只统计近 24 小时。 触发中事件自动刷新（15s）。', {
        exact: false,
      }),
    ).toBeVisible();

    const rows = page.locator('.ant-table-tbody .ant-table-row');
    await expect(rows).toHaveCount(50);
    await expect(page.getByRole('button', { name: '加载更早事件' })).toBeVisible();
    const firstList = listRequests[0];
    expect(firstList).toBeTruthy();
    expect(firstList?.searchParams.get('fired_after')).toBeNull();
    expect(firstList?.searchParams.get('rule_id')).toBeNull();

    // 点击最吵规则第一条：事件表应带上图表窗口 from/to，与柱上 21 次一致。
    const card = page.locator('.ant-card', { hasText: '最吵规则 Top 8' }).first();
    const ruleButton = card.locator('svg rect[role="button"]').first();
    await expect(ruleButton).toBeVisible({ timeout: 20_000 });
    await ruleButton.click();

    await expect(page.locator('.ant-tag', { hasText: `规则：${RULE_NAME}` })).toBeVisible();
    await expect(page.locator('.ant-tag', { hasText: '范围：近 24 小时' })).toBeVisible();
    await expect(
      page.getByText(
        '事件表与图表同窗口（近 24 小时），与柱上触发次数同口径。 触发中事件自动刷新（15s）。',
        { exact: false },
      ),
    ).toBeVisible();

    await expect(rows).toHaveCount(21);
    await expect(page.getByRole('button', { name: '加载更早事件' })).toHaveCount(0);

    const ruleRequest = listRequests.find((url) => url.searchParams.get('rule_id') === RULE_ID);
    expect(ruleRequest, '规则下钻必须发出带 rule_id 的事件查询').toBeTruthy();
    expect(ruleRequest?.searchParams.get('fired_after')).toBe('2026-11-19T00:00:00Z');
    // 第 13 轮起后端 fired_before 与 stats 同为半开区间：终点原样下发。
    expect(ruleRequest?.searchParams.get('fired_before')).toBe('2026-11-20T00:00:00Z');

    // 切 7 天：规则下钻绑定窗口，必须一并清空，不能保留后展示全时 71 条。
    await page.locator('.ant-segmented-item', { hasText: /^7 天$/ }).click();
    await expect(page.locator('.ant-tag', { hasText: '规则：' })).toHaveCount(0);
    await expect(page.locator('.ant-tag', { hasText: '范围：' })).toHaveCount(0);
    await expect(page.locator('.ant-tag', { hasText: '全部时间' })).toBeVisible();
    await expect(
      page.getByText('近 7 天共 70 条事件 / 9 条规则。', { exact: false }),
    ).toBeVisible();
    await expect(
      page.getByText('事件表为全部时间；上方图表只统计近 7 天。 触发中事件自动刷新（15s）。', {
        exact: false,
      }),
    ).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 2. 390 宽 Top 12：过期圆点不再压住服务器名首字符
// ---------------------------------------------------------------------------

function monitoredServer(name: string, id: string, cpu: number, createdAt: string) {
  return {
    server_id: id,
    server_name: name,
    created_at: createdAt,
    cpu_pct: cpu,
    mem_pct: 40,
    disk_pct: 30,
    load_1: 0.5,
    net_in_bps: 1_024,
    net_out_bps: 512,
  };
}

async function mockMonitor(page: Page, servers: unknown[]) {
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers,
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
    route.fulfill({ json: { window: '24h', bucket_seconds: 3_600, points: [], truncated: false } }),
  );
  await page.route('**/api/servers', (route) =>
    route.fulfill({
      json: { items: [{ id: SERVER_ID, name: 'web-prod-01', host: '10.0.0.8', port: 22 }] },
    }),
  );
  await page.route('**/api/servers/*/metrics*', (route) =>
    route.fulfill({
      json: {
        server_id: SERVER_ID,
        points: [
          {
            created_at: new Date(Date.now() - 3_600_000).toISOString(),
            cpu_pct: 40,
            mem_pct: 40,
            disk_pct: 30,
            load_1: 0.5,
            net_in_bps: 1_024,
            net_out_bps: 512,
          },
        ],
        truncated: false,
      },
    }),
  );
}

test.describe('图表第 12 轮：390 宽过期圆点与标签', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('过期行标签左缘 ≥ 圆点右缘 + 4px，完整名仍在 aria 中', async ({ page }) => {
    await mockMonitor(page, [
      monitoredServer(
        'web-prod-01',
        'stale-web',
        90,
        new Date(Date.now() - 3 * 3_600_000).toISOString(),
      ),
      monitoredServer('web-prod-02', 'fresh-web', 80, new Date().toISOString()),
    ]);
    await page.goto('/monitor');

    const card = page.locator('.ant-card', { hasText: '最紧张服务器 Top 12' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    const svg = card.locator('svg[role="img"]').first();
    await expect(svg).toBeVisible({ timeout: 20_000 });
    await expect(card.locator('[data-testid="top-server-stale-marker"]')).toHaveCount(1);

    const geometry = await svg.evaluate((element) => {
      const texts = [...element.querySelectorAll('text')];
      const staleLabel = texts.find((node) => node.textContent === 'web-…od-01') ?? null;
      const freshLabel = texts.find((node) => node.textContent === 'web-prod-02') ?? null;
      const dot = element.querySelector('[data-testid="top-server-stale-marker"] circle');
      const box = (node: Element | null) => {
        if (node === null) {
          return null;
        }
        const rect = (node as SVGGraphicsElement).getBBox();
        return { left: rect.x, right: rect.x + rect.width, width: rect.width };
      };
      const aria = element.getAttribute('aria-label') ?? '';
      return {
        staleText: staleLabel?.textContent ?? null,
        stale: box(staleLabel),
        fresh: box(freshLabel),
        dot: box(dot),
        rowAria:
          element
            .querySelector('[role="img"][aria-label*="web-prod-01"]')
            ?.getAttribute('aria-label') ?? '',
        aria,
      };
    });

    expect(geometry.staleText).toBe('web-…od-01');
    expect(geometry.stale).not.toBeNull();
    expect(geometry.fresh).not.toBeNull();
    expect(geometry.dot).not.toBeNull();
    // 过期圆点 cx=6/r=3 覆盖 3–9px；旧版标签宽 66.05px、左缘 1.95px。
    expect(geometry.dot?.right ?? 0).toBeCloseTo(9, 5);
    expect(geometry.stale?.left ?? -1).toBeGreaterThanOrEqual((geometry.dot?.right ?? 0) + 4);
    expect(geometry.stale?.left ?? -1).toBeGreaterThanOrEqual(12);
    // 非过期行同样不得越过左缘 4px。
    expect(geometry.fresh?.left ?? -1).toBeGreaterThanOrEqual(4);
    // 截断只影响可见标签，行 aria（读屏/tooltip 同源）仍能拿到完整机器名。
    expect(geometry.rowAria).toContain('web-prod-01');
    expect(geometry.aria).toContain('Top 2');
  });
});
