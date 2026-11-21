import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 4 轮走查 4 个浏览器可达项的回归：
// 1. 390 宽 Top 12 的 prod-cache-01 / prod-idle-01 不再截成同一标签，
//    触屏点按能打开（再点关闭）完整名称 tooltip；
// 2. 390 宽巡检均分走势 x 轴末标签贴右缘，不再被 SVG 裁成 "11/20 02:1"；
// 3. 「最吵规则」复用 ruleDisplayName 的中文展示名，原始英文名进 tooltip；
// 4. aria-label 用 distinct_rules 全量总数，而不是后端 LIMIT 8 的条数。
//
// 第 5 项（antd 6 弃用属性）的运行时告警只在开发构建出现，vite preview
// 产物会被 antd 编译期裁掉；对应回归放在单测 antd6Deprecations.test.ts
// 静态扫描源码，这里不重复造一个永远为绿的浏览器断言。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';

function monitoredServer(name: string, index: number, cpu: number) {
  return {
    server_id: index === 0 ? SERVER_ID : `srv-${index}`,
    server_name: name,
    // 第 12 轮起过期行标签会额外让出圆点预算；这里用「刚刚采到」保证
    // 测试覆盖的是非过期的常规排版，而不是时钟漂移后的过期分支。
    created_at: new Date().toISOString(),
    cpu_pct: cpu,
    mem_pct: 40 + index,
    disk_pct: 30 + index,
    load_1: index / 10,
    net_in_bps: 1024 * index,
    net_out_bps: 512 * index,
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
  await page.route(/\/api\/servers\/[^/]+\/metrics/, (route) =>
    route.fulfill({
      json: {
        server_id: SERVER_ID,
        points: [
          {
            created_at: '2026-11-20T01:00:00Z',
            cpu_pct: 72,
            mem_pct: 51,
            disk_pct: 44,
            load_1: 0.8,
            net_in_bps: 1_000_000,
            net_out_bps: 500_000,
          },
          {
            created_at: '2026-11-20T02:00:00Z',
            cpu_pct: 68,
            mem_pct: 49,
            disk_pct: 44,
            load_1: 0.7,
            net_in_bps: 900_000,
            net_out_bps: 450_000,
          },
        ],
        truncated: false,
      },
    }),
  );
}

test.describe('图表第 4 轮：390 宽 Top 12 与触屏 tooltip', () => {
  test('两台 prod 机器标签可区分，点按 tooltip 显示完整名称并可再次点按关闭', async ({
    browser,
  }) => {
    const context = await browser.newContext({
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
      locale: 'zh-CN',
      storageState: AUTH_STATE_FILE,
      baseURL: 'http://localhost:4173',
    });
    try {
      const page = await context.newPage();
      // prod-cache-01 排第 6、prod-idle-01 排第 9：两者都落在 Top 12 内。
      const servers = [
        monitoredServer('prod-cache-01', 0, 96),
        ...Array.from({ length: 7 }, (_, index) =>
          monitoredServer(`app-0${index + 1}`, index + 1, 90 - index * 5),
        ),
        monitoredServer('prod-idle-01', 8, 20),
        ...Array.from({ length: 3 }, (_, index) =>
          monitoredServer(`staging-0${index + 1}`, index + 9, 14 - index),
        ),
      ];
      await mockMonitor(page, servers);
      await page.goto('/monitor');

      const card = page.locator('.ant-card', { hasText: '最紧张服务器 Top 12' }).first();
      await expect(card).toBeVisible({ timeout: 20_000 });
      const labels = await card.locator('svg text[text-anchor="end"]').allTextContents();
      expect(new Set(labels).size, `服务器标签应唯一：${labels.join('|')}`).toBe(labels.length);
      expect(labels).toContain('prod-…che-01');
      expect(labels).toContain('prod-idle-01');
      expect(labels).not.toContain('prod…e-01');

      const cacheRow = card.locator('svg rect[tabindex="0"][aria-label*="prod-cache-01"]');
      await expect(cacheRow).toHaveCount(1);
      await cacheRow.tap();
      const tooltip = card.getByTestId('top-server-tooltip');
      await expect(tooltip).toBeVisible();
      await expect(tooltip).toContainText('prod-cache-01');
      await expect(tooltip).toContainText('CPU 使用率');

      // 再次点按同一行：取消钉住（触屏无法 hover，需要可关闭）。
      await cacheRow.tap();
      await expect(card.getByTestId('top-server-tooltip')).toHaveCount(0);
    } finally {
      await context.close();
    }
  });
});

test.describe('图表第 4 轮：巡检均分走势 x 轴贴边', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('末标签右缘不越出 SVG，最新报告时间完整可见', async ({ page }) => {
    const jobId = '33333333-4444-5555-6666-777777777777';
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/inspection/jobs', (route) =>
      route.fulfill({
        json: {
          items: [
            {
              id: jobId,
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
            },
          ],
        },
      }),
    );
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({ json: { items: [] } }),
    );
    await page.route('**/api/inspection/trend**', (route) =>
      route.fulfill({
        json: {
          job_id: jobId,
          points: Array.from({ length: 14 }, (_, index) => ({
            report_id: `report-${index}`,
            created_at: new Date(Date.UTC(2026, 10, 19, index * 2 + 14)).toISOString(),
            avg_score: 80 + index,
            server_count: 8,
            anomaly_count: 1,
            trigger: 'cron',
          })),
          truncated: false,
        },
      }),
    );

    await page.goto('/inspections');
    await page.getByRole('tab', { name: '报告' }).click();
    const card = page.locator('.ant-card', { hasText: '均分走势' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    const axis = card.getByTestId('inspection-trend-x-axis');
    await expect(axis).toBeVisible();

    const layout = await card.evaluate((element) => {
      const svg = element.querySelector('svg[role="img"]');
      if (!svg) {
        return null;
      }
      const svgBox = svg.getBoundingClientRect();
      const labels = [
        ...element.querySelectorAll('[data-testid="inspection-trend-x-axis"] text'),
      ].map((node) => {
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
    const labels = layout?.labels ?? [];
    expect(labels.length).toBeGreaterThanOrEqual(3);
    // 旧实现末标签右缘 319.6px > SVG 316px，显示成 "11/20 02:1"。
    expect(labels[labels.length - 1]?.right ?? 999).toBeLessThanOrEqual(
      (layout?.svgWidth ?? 0) + 0.5,
    );
    expect(labels[0]?.left ?? -1).toBeGreaterThanOrEqual(0);
    expect(labels[labels.length - 1]?.text).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    for (const label of labels) {
      expect(label.text).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    }
  });
});

// ---------------------------------------------------------------------------
// 3 + 4. 最吵规则：展示名与全量规则数
// ---------------------------------------------------------------------------

const SENTINEL = 'monitor.collector.unreachable';

function severityBuckets() {
  const start = Date.UTC(2026, 10, 19);
  return Array.from({ length: 24 }, (_, index) => ({
    bucket_start: new Date(start + index * 3_600_000).toISOString(),
    info: index % 2,
    warning: index % 3,
    critical: index % 5 === 0 ? 1 : 0,
  }));
}

async function mockAlerts(page: Page) {
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
          total: 65,
          // 窗口内实际有 9 条规则触发，top_rules 被后端 LIMIT 8。
          distinct_rules: 9,
          severity_timeline: severityBuckets(),
          top_rules: [
            {
              rule_id: 'rule-sentinel',
              rule_name: SENTINEL,
              severity: 'warning',
              count: 21,
              server_count: 3,
              last_fired_at: '2026-11-19T19:00:00Z',
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
        },
      });
    }
    return route.fulfill({ json: { items: [], has_more: false, next_before_id: null } });
  });
}

test.describe('图表第 4 轮：最吵规则展示名与全量计数', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('系统规则显示中文名，tooltip 保留原始名，aria 使用 distinct_rules', async ({ page }) => {
    await mockAlerts(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();

    const card = page.locator('.ant-card', { hasText: '最吵规则 Top 8' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    const svg = card.locator('svg[role="img"]').first();
    await expect(svg).toBeVisible();

    // 走查第 4 轮 low：旧版可见标签是截断的 monitor.coll…，与同页
    // 事件表/规则表的「采集不可达（系统）」对不上。
    const labels = await svg.locator('text[text-anchor="end"]').allTextContents();
    expect(labels).toContain('采集不可达（系统）');
    expect(labels.join('|')).not.toContain('monitor.coll');

    // 第 4 项：aria 全量 9 条，并说明图中只显示触发最多的 8 条。
    const ariaLabel = await svg.getAttribute('aria-label');
    expect(ariaLabel).toContain('共 9 条有触发记录的规则');
    expect(ariaLabel).toContain('图中显示触发最多的 8 条');

    const sentinelHit = card.locator(
      'svg rect[tabindex="0"][aria-label*="monitor.collector.unreachable"]',
    );
    await expect(sentinelHit).toHaveCount(1);
    await sentinelHit.hover();
    const tooltip = card.getByTestId('noisy-rule-tooltip');
    await expect(tooltip).toBeVisible();
    await expect(tooltip).toContainText('采集不可达（系统）');
    await expect(tooltip).toContainText('原始规则名 monitor.collector.unreachable');
  });
});
