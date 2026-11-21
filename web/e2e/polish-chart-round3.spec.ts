import type { Locator } from '@playwright/test';

import { expect, test, type Page } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 3 轮走查 6 项修复的浏览器回归：
// 1. 20 GB/s 顶部刻度不再被左缘裁掉（第 7 轮起为 "20.00 GB/s" + 动态左留白）；
// 2. 单机指标折线图可聚焦，aria-label/title 含窗口与最新/最小/最大，方向键读数；
// 3. 390 宽 /alerts 与 /inspections 表格横向滚动，主列不再逐字竖排；
// 4. 告警分布 7 天视图 x 轴首尾标签贴边、互不重叠；
// 5. 切 1h/6h/24h 后 hover tooltip 不再停留在旧窗口的“幽灵点”；
// 6. 巡检均分走势跨度 >24h 时标签带 HH:mm，同一天多份报告可区分。

test.use({ storageState: AUTH_STATE_FILE });

const SERVER_ID = '11111111-2222-3333-4444-555555555555';

/** 单机指标查询（overview + /metrics）用同一组采样。 */
function metricPoint(createdAt: string, cpu: number, netIn: number, netOut = 2_000_000_000) {
  return {
    created_at: createdAt,
    cpu_pct: cpu,
    mem_pct: 50,
    disk_pct: 60,
    load_1: 0.6,
    net_in_bps: netIn,
    net_out_bps: netOut,
  };
}

async function mockMonitor(page: Page, seriesByMinutes: Record<number, unknown[]>) {
  const fallbackSeries = seriesByMinutes[60] ?? Object.values(seriesByMinutes)[0] ?? [];
  const first = fallbackSeries[0] as ReturnType<typeof metricPoint> | undefined;
  await page.route('**/api/monitor/overview', (route) =>
    route.fulfill({
      json: {
        servers: [
          {
            server_id: SERVER_ID,
            server_name: 'chart-round3-host',
            // 走查第 15 轮：overview 行带平台，Linux 资产继续用 /proc 口径。
            os: 'linux',
            created_at: first?.created_at,
            cpu_pct: first?.cpu_pct ?? 0,
            mem_pct: first?.mem_pct ?? 0,
            disk_pct: first?.disk_pct ?? 0,
            load_1: first?.load_1 ?? 0,
            net_in_bps: first?.net_in_bps ?? 0,
            net_out_bps: first?.net_out_bps ?? 0,
          },
        ],
        firing: { total: 0, critical: 0, warning: 0, info: 0 },
        last_inspection: null,
        collector: { enabled: false, interval_seconds: 60 },
      },
    }),
  );
  await page.route(`**/api/servers/${SERVER_ID}/metrics*`, (route) => {
    const url = new URL(route.request().url());
    const minutes = Number(url.searchParams.get('minutes') ?? '60');
    const points = seriesByMinutes[minutes] ?? fallbackSeries;
    return route.fulfill({ json: { server_id: SERVER_ID, points, truncated: false } });
  });
}

function metricChartOf(card: Locator) {
  return card
    .locator('svg[role="img"]')
    .filter({ has: card.page().locator('polyline') })
    .first();
}

test.describe('图表第 3 轮：监控 y 轴与无障碍', () => {
  const points = [
    metricPoint('2026-11-19T20:00:00Z', 30, 1_000_000_000),
    metricPoint('2026-11-19T20:30:00Z', 52, 9_876_543_210),
    metricPoint('2026-11-19T21:00:00Z', 45, 7_000_000_000),
    metricPoint('2026-11-19T21:30:00Z', 61, 4_000_000_000),
  ];

  test('20 GB/s 的 y 轴顶部标签完整落在绘图区内', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await mockMonitor(page, { 360: points });
    await page.goto('/monitor');

    const netCard = page.locator('.ant-card', { hasText: '网络入' }).first();
    await expect(netCard).toBeVisible({ timeout: 20_000 });
    const svg = metricChartOf(netCard);
    await expect(svg).toBeVisible();

    const layout = await svg.evaluate((element) => {
      const rect = element.getBoundingClientRect();
      const ticks = [...element.querySelectorAll('text')]
        .filter((node) => node.getAttribute('text-anchor') === 'end')
        .map((node) => {
          const box = node.getBoundingClientRect();
          return {
            text: node.textContent ?? '',
            left: box.left - rect.left,
            right: box.right - rect.left,
          };
        });
      return { width: rect.width, ticks };
    });

    // 9,876,543,210 B/s → 20 GB/s 轴；旧版 "20.0GB/s" 左缘为负，显示成
    // "0.0GB/s"。第 7 轮 low 起轴与卡头共用带 B 的完整单位 "20.00 GB/s"
    // （左留白动态给到 68px），不再压成丢 B 的 "20G/s"。
    const top = layout.ticks.find((tick) => tick.text === '20.00 GB/s');
    expect(
      top,
      `顶部刻度应为 20.00 GB/s，实际 ${layout.ticks.map((t) => t.text).join('|')}`,
    ).toBeTruthy();
    expect(top?.left ?? -1).toBeGreaterThanOrEqual(0);
    for (const tick of layout.ticks) {
      expect(tick.left, `刻度 ${tick.text} 左缘`).toBeGreaterThanOrEqual(0);
    }
  });

  test('图表可聚焦，读屏名字含指标/窗口/最新/最小/最大，方向键逐点读数', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await mockMonitor(page, { 60: points });
    await page.goto('/monitor');

    const cpuCard = page.locator('.ant-card', { hasText: '/proc/stat 使用率' }).first();
    const chart = cpuCard.getByRole('img', { name: /CPU 使用率折线图/ });
    await expect(chart).toBeVisible({ timeout: 20_000 });
    await expect(chart).toHaveAttribute('tabindex', '0');
    const label = await chart.getAttribute('aria-label');
    expect(label).toContain('窗口');
    expect(label).toContain('最新');
    expect(label).toContain('最小');
    expect(label).toContain('最大');
    expect(label).toContain('左右方向键逐点读数');
    await expect(chart.locator('title')).toHaveText(/CPU 使用率折线图/);

    await chart.focus();
    await expect(chart).toBeFocused();
    await page.keyboard.press('ArrowLeft');
    // 方向键应把该点写进 tooltip 与 aria-live（读屏逐点读数）。
    await expect(cpuCard.getByTestId('metric-chart-tooltip')).toBeVisible();
    await expect(cpuCard.getByTestId('metric-chart-announcement')).not.toHaveText('');
  });

  test('切 1h 窗口后旧 hover 点不残留（无幽灵 tooltip）', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    const sixHour = [
      metricPoint('2026-11-19T18:00:00Z', 20, 1_000_000_000),
      metricPoint('2026-11-19T21:25:00Z', 52, 3_000_000_000),
      metricPoint('2026-11-19T22:00:00Z', 44, 2_000_000_000),
    ];
    const oneHour = [
      metricPoint('2026-11-19T23:00:00Z', 31, 1_000_000_000),
      metricPoint('2026-11-19T23:30:00Z', 37, 1_200_000_000),
      metricPoint('2026-11-20T00:00:00Z', 43, 1_400_000_000),
    ];
    await mockMonitor(page, { 60: oneHour, 360: sixHour });
    await page.goto('/monitor');

    const cpuCard = page.locator('.ant-card', { hasText: '/proc/stat 使用率' }).first();
    const chart = metricChartOf(cpuCard);
    await expect(chart).toBeVisible({ timeout: 20_000 });

    await page.locator('.ant-segmented-item', { hasText: /^6 小时$/ }).click();
    await expect(chart).toBeVisible();
    const box = await chart.boundingBox();
    expect(box).not.toBeNull();
    await chart.hover({ position: { x: (box?.width ?? 0) * 0.62, y: 80 } });
    const tooltip = cpuCard.getByTestId('metric-chart-tooltip');
    await expect(tooltip).toBeVisible();
    const staleText = await tooltip.textContent();
    expect(staleText).toBeTruthy();

    // 用 JS 点击（鼠标仍停在图上，不触发 mouseleave）：旧实现会把 6h 的
    // hover 点带到 1h 窗口；修复后旧时间点不存在，tooltip 消失。
    await page.locator('.ant-segmented-item', { hasText: /^1 小时$/ }).evaluate((element) => {
      (element as HTMLElement).click();
    });

    await expect(cpuCard.getByTestId('metric-chart-tooltip')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 3. 390 宽表格横向滚动
// ---------------------------------------------------------------------------

function alertEvent(index: number) {
  return {
    id: 1000 - index,
    rule_id: '22222222-3333-4444-5555-666666666666',
    rule_name: 'monitor.collector.unreachable',
    server_id: SERVER_ID,
    server_name: 'prod-db-01',
    metric: 'cpu_pct',
    status: index % 3 === 0 ? 'resolved' : 'firing',
    severity: ['critical', 'warning', 'info'][index % 3] ?? 'info',
    value: 91.5,
    message: '采集不可达（系统）——服务器连续 3 个采集周期无数据上报，请检查采集器与网络连通性',
    fired_at: '2026-11-19T10:00:00Z',
    resolved_at: index % 3 === 0 ? '2026-11-19T10:10:00Z' : null,
    last_notified_at: null,
    notify_status: 'sent',
    created_at: '2026-11-19T10:00:00Z',
    updated_at: '2026-11-19T10:10:00Z',
  };
}

function severityBuckets(window: '24h' | '7d') {
  const stepHours = window === '7d' ? 6 : 1;
  const count = window === '7d' ? 28 : 24;
  const start = window === '7d' ? Date.UTC(2026, 10, 13) : Date.UTC(2026, 10, 19);
  return Array.from({ length: count }, (_, index) => ({
    bucket_start: new Date(start + index * stepHours * 3_600_000).toISOString(),
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
      const window = url.searchParams.get('window') === '7d' ? '7d' : '24h';
      return route.fulfill({
        json: {
          window,
          bucket_seconds: window === '7d' ? 21600 : 3600,
          from: '2026-11-13T00:00:00Z',
          to: '2026-11-20T00:00:00Z',
          total: 50,
          distinct_rules: 1,
          severity_timeline: severityBuckets(window),
          top_rules: [],
        },
      });
    }
    return route.fulfill({
      json: {
        items: Array.from({ length: 50 }, (_, index) => alertEvent(index)),
        has_more: false,
        next_before_id: null,
      },
    });
  });
}

test.describe('图表第 3 轮：390 宽表格不再逐字竖排', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('/alerts 事件表横向滚动，主列保持可读宽度', async ({ page }) => {
    await mockAlerts(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    // rc-table 在横向滚动时额外渲染一份隐藏的测量表；`:visible` 只取
    // 真正展示给用户的行。
    const firstRow = page.locator('.ant-table-tbody tr.ant-table-row:visible').first();
    await expect(firstRow).toBeVisible({ timeout: 20_000 });

    const metrics = await firstRow.evaluate((row) => {
      const table = row.closest('.ant-table');
      const scroller =
        table?.querySelector('.ant-table-content') ?? table?.querySelector('.ant-table-body');
      const cells = [...row.querySelectorAll('td')];
      return {
        rowHeight: row.getBoundingClientRect().height,
        scrollerClient: scroller?.clientWidth ?? 0,
        scrollerScroll: scroller?.scrollWidth ?? 0,
        targetWidth: cells[2]?.getBoundingClientRect().width ?? 0,
        pageScrollHeight: document.querySelector('.app-content')?.scrollHeight ?? 0,
      };
    });
    expect(metrics.rowHeight).toBeLessThan(110);
    expect(metrics.scrollerScroll).toBeGreaterThan(metrics.scrollerClient);
    expect(metrics.targetWidth).toBeGreaterThan(100);
    // 旧实现 50 行把 .app-content 撑到 14089px。
    expect(metrics.pageScrollHeight).toBeLessThan(9000);
  });

  test('/inspections 报告表横向滚动，任务列保持可读宽度', async ({ page }) => {
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
              last_run_at: '2026-11-19T03:05:00Z',
              next_run_at: '2026-11-20T03:05:00Z',
              created_at: '2026-11-01T00:00:00Z',
              updated_at: '2026-11-19T03:05:00Z',
            },
          ],
        },
      }),
    );
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({
        json: {
          items: Array.from({ length: 20 }, (_, index) => ({
            id: `report-${index}`,
            job_id: jobId,
            job_name: '每日基线巡检',
            trigger: 'cron',
            server_count: 8,
            avg_score: 86,
            anomaly_count: 1,
            started_at: '2026-11-19T03:05:00Z',
            finished_at: '2026-11-19T03:06:00Z',
            created_at: `2026-11-19T${String(index % 24).padStart(2, '0')}:05:00Z`,
          })),
        },
      }),
    );
    await page.route('**/api/inspection/trend**', (route) =>
      route.fulfill({
        json: {
          job_id: jobId,
          points: Array.from({ length: 14 }, (_, index) => ({
            report_id: `report-${index}`,
            created_at: new Date(Date.UTC(2026, 10, 18, index * 2)).toISOString(),
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
    const firstRow = page.locator('.ant-table-tbody tr.ant-table-row:visible').first();
    await expect(firstRow).toBeVisible({ timeout: 20_000 });

    const metrics = await firstRow.evaluate((row) => {
      const table = row.closest('.ant-table');
      const scroller =
        table?.querySelector('.ant-table-content') ?? table?.querySelector('.ant-table-body');
      const cells = [...row.querySelectorAll('td')];
      return {
        rowHeight: row.getBoundingClientRect().height,
        scrollerClient: scroller?.clientWidth ?? 0,
        scrollerScroll: scroller?.scrollWidth ?? 0,
        taskWidth: cells[0]?.getBoundingClientRect().width ?? 0,
        pageScrollHeight: document.querySelector('.app-content')?.scrollHeight ?? 0,
      };
    });
    expect(metrics.rowHeight).toBeLessThan(110);
    expect(metrics.scrollerScroll).toBeGreaterThan(metrics.scrollerClient);
    expect(metrics.taskWidth).toBeGreaterThan(200);
    expect(metrics.pageScrollHeight).toBeLessThan(5000);
  });
});

// ---------------------------------------------------------------------------
// 4. 告警分布 7 天视图 x 轴首尾贴边、互不重叠
// ---------------------------------------------------------------------------

for (const width of [1440, 1024, 390] as const) {
  test(`告警分布 7 天视图 x 轴标签不重叠/不越界（${width}）`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await mockAlerts(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();
    const card = page.locator('.ant-card', { hasText: '严重度时间分布' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    await page.locator('.ant-segmented-item', { hasText: /^7 天$/ }).click();

    const layout = await card.evaluate((element) => {
      const svg = element.querySelector('svg[role="img"]');
      if (!svg) {
        return null;
      }
      const svgBox = svg.getBoundingClientRect();
      const labels = [
        ...element.querySelectorAll('[data-testid="alert-severity-x-axis"] text'),
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
    expect(layout?.labels.length ?? 0).toBeGreaterThanOrEqual(3);
    const labels = layout?.labels ?? [];
    // 首标签贴左缘、末标签贴右缘：不再被裁成 "11/19 20:"。
    expect(labels[0]?.left ?? 999).toBeGreaterThanOrEqual(0);
    expect(labels[labels.length - 1]?.right ?? 999).toBeLessThanOrEqual(
      (layout?.svgWidth ?? 0) + 0.5,
    );
    for (let index = 1; index < labels.length; index++) {
      const previous = labels[index - 1];
      const current = labels[index];
      expect(
        previous?.right ?? 0,
        `标签 ${previous?.text} 与 ${current?.text} 重叠`,
      ).toBeLessThanOrEqual((current?.left ?? 0) + 0.5);
    }
  });
}

// ---------------------------------------------------------------------------
// 6. 巡检均分走势跨度 >24h 时标签带 HH:mm
// ---------------------------------------------------------------------------

for (const width of [1440, 390] as const) {
  test(`巡检均分走势同日多份报告标签可区分（${width}）`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
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
              last_run_at: '2026-11-19T03:05:00Z',
              next_run_at: '2026-11-20T03:05:00Z',
              created_at: '2026-11-01T00:00:00Z',
              updated_at: '2026-11-19T03:05:00Z',
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
            // 11/18 00:00Z 起每 2h 一份：跨两个自然日、同日多份。
            created_at: new Date(Date.UTC(2026, 10, 18, index * 2)).toISOString(),
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
    // 均分走势在「报告」Tab（默认「任务」不渲染该卡）。
    await page.getByRole('tab', { name: '报告' }).click();
    const card = page.locator('.ant-card', { hasText: '均分走势' }).first();
    await expect(card).toBeVisible({ timeout: 20_000 });
    const axis = card.getByTestId('inspection-trend-x-axis');
    await expect(axis).toBeVisible();
    const labels = await axis.evaluate((element) =>
      [...element.querySelectorAll('text')].map((node) => node.textContent ?? ''),
    );
    expect(labels.length).toBeGreaterThanOrEqual(3);
    for (const label of labels) {
      expect(label).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    }
    expect(new Set(labels).size).toBe(labels.length);
  });
}
