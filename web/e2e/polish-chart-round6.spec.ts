import type { Page } from '@playwright/test';

import { expect, test } from './fixtures';

import { AUTH_STATE_FILE } from './helpers';

// 图表第 6 轮走查的 4 个浏览器可达项：
// 1. 只有即时巡检报告（job_id 为空）时 /inspections「报告」页的均分走势
//    仍按全部报告出图；任务下拉提供「全部任务」；选中无报告任务时文案为
//    「该任务暂无报告」；
// 2. Top 12 的中文服务器名按像素宽度截断，1440/1024/390 左缘都不越界；
// 3. 「最吵规则 Top 8」的系统规则中文名在 1024/390 左缘不越界；
// 4. 390 宽 /alerts 的规则表 / 渠道表固定 900px 横向滚动，表头不再逐字竖排。

test.use({ storageState: AUTH_STATE_FILE });

const JOB_ID = '33333333-4444-5555-6666-777777777777';
const EMPTY_JOB_ID = '44444444-5555-6666-7777-888888888888';
const SERVER_ID = '11111111-2222-3333-4444-555555555555';

function job(id: string, name: string) {
  return {
    id,
    name,
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
  };
}

function report(id: string, jobId: string | null, jobName: string, trigger: string) {
  return {
    id,
    job_id: jobId,
    job_name: jobName,
    trigger,
    server_count: 8,
    avg_score: 86,
    anomaly_count: 1,
    started_at: '2026-11-20T02:13:00Z',
    finished_at: '2026-11-20T02:14:00Z',
    created_at: '2026-11-20T02:13:00Z',
  };
}

function immediateReports(count: number) {
  return Array.from({ length: count }, (_, index) =>
    report(`immediate-${index}`, null, '', 'manual'),
  );
}

function trendPoints(count: number) {
  const start = Date.UTC(2026, 10, 20, 0);
  return Array.from({ length: count }, (_, index) => ({
    report_id: `point-${index}`,
    created_at: new Date(start + index * 3_600_000).toISOString(),
    avg_score: 78 + (index % 12),
    server_count: 8,
    anomaly_count: index % 3,
    trigger: 'manual',
  }));
}

async function mockInspectionShell(page: Page) {
  await page.route('**/api/notification-channels', (route) =>
    route.fulfill({ json: { items: [] } }),
  );
}

async function openReportsTab(page: Page) {
  await page.goto('/inspections');
  await page.getByRole('tab', { name: '报告' }).click();
  const card = page.locator('.ant-card', { hasText: '均分走势' }).first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  return card;
}

test.describe('图表第 6 轮：巡检走势包含即时巡检报告', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('只有即时巡检报告时不带 job_id 查全部并画出 12 份', async ({ page }) => {
    await mockInspectionShell(page);
    await page.route('**/api/inspection/jobs', (route) => route.fulfill({ json: { items: [] } }));
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({ json: { items: immediateReports(12) } }),
    );
    const trendSearches: URLSearchParams[] = [];
    await page.route('**/api/inspection/trend**', (route) => {
      const url = new URL(route.request().url());
      trendSearches.push(url.searchParams);
      return route.fulfill({
        json: { job_id: null, points: trendPoints(12), truncated: false },
      });
    });

    const card = await openReportsTab(page);
    // 走查 high：旧页面根本不发 trend 请求，图显示「暂无巡检报告」而
    // 下方表格有 12 行。现在必须出图且请求缺省 job_id。
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await expect(card.locator('svg rect[tabindex="0"]')).toHaveCount(12);
    await expect(card.getByText('暂无巡检报告')).toHaveCount(0);
    expect(trendSearches.length).toBeGreaterThan(0);
    expect(trendSearches[0]?.get('job_id')).toBeNull();
    expect(trendSearches[0]?.get('limit')).toBe('30');
  });

  test('唯一任务 + 最新为即时报告：默认「全部任务」，切换任务后按 job_id 重查', async ({
    page,
  }) => {
    await mockInspectionShell(page);
    await page.route('**/api/inspection/jobs', (route) =>
      route.fulfill({ json: { items: [job(JOB_ID, '每日基线巡检')] } }),
    );
    // 最新一份是即时巡检（job_id=null），其前还有一份定时任务报告。
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({
        json: {
          items: [
            report('immediate-latest', null, '', 'manual'),
            report('cron-1', JOB_ID, '每日基线巡检', 'cron'),
          ],
        },
      }),
    );
    const trendSearches: URLSearchParams[] = [];
    await page.route('**/api/inspection/trend**', (route) => {
      const url = new URL(route.request().url());
      trendSearches.push(url.searchParams);
      const jobId = url.searchParams.get('job_id');
      const points = trendPoints(jobId === JOB_ID ? 1 : 2);
      return route.fulfill({ json: { job_id: jobId, points, truncated: false } });
    });

    const card = await openReportsTab(page);
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    // 默认「全部任务」：2 个点（即时 1 + 定时 1），请求不带 job_id。
    await expect(card.locator('svg rect[tabindex="0"]')).toHaveCount(2);
    // 图表 <title> 的 aria 文案也含「全部任务」，这里只断言下拉的选中值。
    await expect(card.getByText('全部任务', { exact: true })).toBeVisible();
    expect(trendSearches[0]?.get('job_id')).toBeNull();

    // 手动选择唯一任务：按 job_id 重查，仅 1 个点。
    await card.locator('.ant-select').first().click();
    await page.locator('.ant-select-item-option', { hasText: '每日基线巡检' }).first().click();
    await expect
      .poll(() => trendSearches.some((search) => search.get('job_id') === JOB_ID))
      .toBe(true);
    await expect(card.locator('svg rect[tabindex="0"]')).toHaveCount(1);
  });

  test('选中无报告的任务时提示「该任务暂无报告」', async ({ page }) => {
    await mockInspectionShell(page);
    await page.route('**/api/inspection/jobs', (route) =>
      route.fulfill({
        json: { items: [job(JOB_ID, '每日基线巡检'), job(EMPTY_JOB_ID, '空任务')] },
      }),
    );
    await page.route('**/api/inspection/reports**', (route) =>
      route.fulfill({ json: { items: immediateReports(3) } }),
    );
    const trendSearches: URLSearchParams[] = [];
    await page.route('**/api/inspection/trend**', (route) => {
      const url = new URL(route.request().url());
      trendSearches.push(url.searchParams);
      const jobId = url.searchParams.get('job_id');
      return route.fulfill({
        json: {
          job_id: jobId,
          points: jobId === EMPTY_JOB_ID ? [] : trendPoints(3),
          truncated: false,
        },
      });
    });

    const card = await openReportsTab(page);
    await expect(card.locator('svg[role="img"]')).toBeVisible({ timeout: 20_000 });
    await card.locator('.ant-select').first().click();
    await page.locator('.ant-select-item-option', { hasText: '空任务' }).first().click();
    await expect
      .poll(() => trendSearches.some((search) => search.get('job_id') === EMPTY_JOB_ID))
      .toBe(true);
    await expect(card.getByText('该任务暂无报告')).toBeVisible({ timeout: 20_000 });
    await expect(card.getByText('暂无巡检报告')).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 2. Top 12 中文名按像素宽度截断
// ---------------------------------------------------------------------------

function monitoredServer(name: string, index: number, cpu: number) {
  return {
    server_id: index === 0 ? SERVER_ID : `srv-cjk-${index}`,
    server_name: name,
    created_at: '2026-11-20T02:00:00Z',
    cpu_pct: cpu,
    mem_pct: 40 + index,
    disk_pct: 30 + index,
    load_1: index / 10,
    net_in_bps: 1024 * index,
    net_out_bps: 512 * index,
  };
}

for (const width of [1440, 1024, 390] as const) {
  test(`Top 12 中文服务器名左缘不越界（${width}）`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const servers = [
      monitoredServer('生产数据库主节点-华东一区', 0, 99),
      monitoredServer('生产数据库主节点-华东二区', 1, 90),
      monitoredServer('prod-web-01', 2, 80),
      monitoredServer('prod-web-02', 3, 70),
    ];
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
              created_at: '2026-11-20T02:00:00Z',
              cpu_pct: 99,
              mem_pct: 40,
              disk_pct: 30,
              load_1: 0,
              net_in_bps: 0,
              net_out_bps: 0,
            },
          ],
          truncated: false,
        },
      }),
    );

    await page.goto('/monitor');
    const card = page.locator('.ant-card', { hasText: '最紧张服务器 Top 12' }).first();
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
    expect(layout?.labels.length).toBe(4);
    for (const label of layout?.labels ?? []) {
      expect(label.left, `标签「${label.text}」左缘越界`).toBeGreaterThanOrEqual(0);
      expect(label.right, `标签「${label.text}」右缘越界`).toBeLessThanOrEqual(
        (layout?.svgWidth ?? 0) + 0.5,
      );
    }
    // 区域后缀保留，能辨认是哪台机器。
    expect(layout?.labels[0]?.text).toContain('华东一区');
    expect(layout?.labels[1]?.text).toContain('华东二区');
  });
}

// ---------------------------------------------------------------------------
// 3. 最吵规则中文名左缘不越界
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

async function mockAlertStats(page: Page) {
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
          distinct_rules: 2,
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
            {
              rule_id: 'rule-1',
              rule_name: '磁盘空间不足',
              severity: 'critical',
              count: 18,
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
  test(`最吵规则中文名左缘不越界（${width}）`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await mockAlertStats(page);
    await page.goto('/alerts');
    await page.getByRole('tab', { name: '事件' }).click();

    const card = page.locator('.ant-card', { hasText: '最吵规则 Top 8' }).first();
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
    const sentinel = layout?.labels.find((label) => label.text.includes('系统'));
    expect(sentinel, `未找到系统规则标签：${JSON.stringify(layout?.labels)}`).toBeTruthy();
    expect(sentinel?.left ?? -1).toBeGreaterThanOrEqual(0);
    expect(sentinel?.right ?? 999).toBeLessThanOrEqual((layout?.svgWidth ?? 0) + 0.5);
    // 中文展示名与「系统」后缀至少保留可辨认的后半段。
    expect(sentinel?.text).toContain('系统）');
  });
}

// ---------------------------------------------------------------------------
// 4. 390 宽规则 / 渠道表横向滚动
// ---------------------------------------------------------------------------

function alertRule(id: string, name: string) {
  return {
    id,
    name,
    metric: 'cpu_pct',
    comparator: 'gt',
    threshold: 90,
    duration_windows: 3,
    cooldown_seconds: 300,
    severity: 'warning',
    silent_until: null,
    notify_channel_id: null,
    enabled: true,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-20T00:00:00Z',
  };
}

function notificationChannel(id: string, name: string) {
  return {
    id,
    name,
    kind: 'webhook',
    config_hint: 'https://example.com/hooks/opsmind-alerts',
    has_config: true,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-20T00:00:00Z',
  };
}

test.describe('图表第 6 轮：390 宽告警表格不再逐字竖排', () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test('规则表与渠道表固定 900px 横向滚动，表头保持单行', async ({ page }) => {
    await page.route('**/api/notification-channels', (route) =>
      route.fulfill({
        json: {
          items: [
            notificationChannel('55555555-6666-7777-8888-999999999999', '生产告警 Webhook'),
            notificationChannel('66666666-7777-8888-9999-aaaaaaaaaaaa', '测试回执 Webhook'),
          ],
        },
      }),
    );
    await page.route('**/api/alert-rules', (route) =>
      route.fulfill({
        json: {
          items: [
            alertRule('77777777-8888-9999-aaaa-bbbbbbbbbbbb', '磁盘空间不足'),
            alertRule('88888888-9999-aaaa-bbbb-cccccccccccc', '内存压力'),
          ],
        },
      }),
    );

    await page.goto('/alerts');

    // 默认 Tab 是「规则」（走查 medium 的现场）。
    const ruleRow = page.locator('.ant-table-tbody tr.ant-table-row:visible').first();
    await expect(ruleRow).toBeVisible({ timeout: 20_000 });
    const ruleMetrics = await ruleRow.evaluate((row) => {
      const table = row.closest('.ant-table');
      const scroller =
        table?.querySelector('.ant-table-content') ?? table?.querySelector('.ant-table-body');
      const headerCells = [...(table?.querySelectorAll('.ant-table-thead th') ?? [])].map(
        (cell) => ({
          text: cell.textContent ?? '',
          width: cell.getBoundingClientRect().width,
          height: cell.getBoundingClientRect().height,
        }),
      );
      return {
        rowHeight: row.getBoundingClientRect().height,
        scrollerClient: scroller?.clientWidth ?? 0,
        scrollerScroll: scroller?.scrollWidth ?? 0,
        headerCells,
      };
    });
    expect(ruleMetrics.scrollerScroll).toBeGreaterThan(ruleMetrics.scrollerClient);
    expect(ruleMetrics.rowHeight).toBeLessThan(110);
    expect(ruleMetrics.headerCells.length).toBe(8);
    for (const cell of ruleMetrics.headerCells) {
      // 逐字竖排时表头宽度只有 ~14px、高度 >80px。
      expect(cell.width, `规则表头「${cell.text}」被压窄`).toBeGreaterThan(50);
      expect(cell.height, `规则表头「${cell.text}」竖排`).toBeLessThan(60);
    }

    await page.getByRole('tab', { name: '通知渠道' }).click();
    const channelRow = page.locator('.ant-table-tbody tr.ant-table-row:visible').first();
    await expect(channelRow).toBeVisible({ timeout: 20_000 });
    const channelMetrics = await channelRow.evaluate((row) => {
      const table = row.closest('.ant-table');
      const scroller =
        table?.querySelector('.ant-table-content') ?? table?.querySelector('.ant-table-body');
      const headerCells = [...(table?.querySelectorAll('.ant-table-thead th') ?? [])].map(
        (cell) => ({
          text: cell.textContent ?? '',
          width: cell.getBoundingClientRect().width,
          height: cell.getBoundingClientRect().height,
        }),
      );
      return {
        rowHeight: row.getBoundingClientRect().height,
        scrollerClient: scroller?.clientWidth ?? 0,
        scrollerScroll: scroller?.scrollWidth ?? 0,
        headerCells,
      };
    });
    expect(channelMetrics.scrollerScroll).toBeGreaterThan(channelMetrics.scrollerClient);
    expect(channelMetrics.rowHeight).toBeLessThan(110);
    expect(channelMetrics.headerCells.length).toBe(4);
    for (const cell of channelMetrics.headerCells) {
      expect(cell.width, `渠道表头「${cell.text}」被压窄`).toBeGreaterThan(50);
      expect(cell.height, `渠道表头「${cell.text}」竖排`).toBeLessThan(60);
    }
  });
});
