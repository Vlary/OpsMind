import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 七期验收走查的 6 项修复的回归护栏（两套主题都跑，浅色为默认）：
// 1. 窄屏（360/375/390）顶栏不再被品牌文字 + 主题切换器挤爆；
// 2. 输入框/Select 外框用 --color-border-strong（可操作边界 ≥ 3:1）；
// 3. 监控图表 hover tooltip 在浅色下也有描边 + 阴影（不再与卡片同色消失）；
// 4. 回放进度条未播放轨道用 --color-border-strong（不再 ≈1.07:1）；
// 5. 表格空态提示用三级文本（不再用禁用色）；
// 6. 容器页不再默认选中 runtime=none 的服务器并发明知 400 的请求。

test.use({ storageState: AUTH_STATE_FILE });

/** 读取一个语义色变量在当前主题下的解析值（rgb(...)）。 */
async function resolveColor(page: Page, variable: string): Promise<string> {
  return page.evaluate((name) => {
    const probe = document.createElement('span');
    probe.style.color = `var(${name})`;
    document.body.appendChild(probe);
    const value = getComputedStyle(probe).color;
    probe.remove();
    return value;
  }, variable);
}

async function useTheme(page: Page, mode: 'light' | 'dark'): Promise<void> {
  await page.addInitScript((value) => {
    window.localStorage.setItem('opsmind.theme', value);
  }, mode);
  await page.emulateMedia({ colorScheme: mode });
}

/** 表头之外的「服务器」资产 payload（契约 3.2 全字段）。 */
function serverPayload(id: string, name: string, runtime: string, kind = 'server') {
  return {
    id,
    name,
    device_kind: kind,
    os: kind === 'network_device' ? 'network' : 'linux',
    netdev_brand: '',
    access_proto: 'ssh',
    host: '127.0.0.1',
    port: 1,
    winrm_port: 5985,
    username: 'e2e',
    auth_method: 'password',
    tags: [],
    auto_exec_enabled: false,
    has_password: true,
    has_private_key: false,
    has_enable_secret: false,
    container_runtime: runtime,
    host_key_fingerprint: '',
    host_key_verified_at: null,
    created_at: '2026-11-08T08:00:00Z',
    updated_at: '2026-11-08T08:00:00Z',
  };
}

// ---------------------------------------------------------------------------
// 1. 窄屏顶栏不溢出（360 / 375 / 390 / 414）
// ---------------------------------------------------------------------------
test.describe('七期：窄屏顶栏单行且不溢出', () => {
  for (const width of [360, 375, 390, 414]) {
    test(`${width}px 顶栏无纵向溢出、无横向溢出、品牌文字不出现`, async ({ page }) => {
      await page.setViewportSize({ width, height: 780 });
      await useTheme(page, 'light');
      await page.goto('/servers');
      await expect(page.locator('.app-header')).toBeVisible({ timeout: 20_000 });

      const metrics = await page.evaluate(() => {
        const header = document.querySelector('.app-header') as HTMLElement | null;
        if (!header) {
          return null;
        }
        const userButton = header.querySelector('.ant-dropdown-trigger') as HTMLElement | null;
        const segmented = header.querySelector('.ant-segmented') as HTMLElement | null;
        const items = Array.from(header.querySelectorAll<HTMLElement>('.ant-segmented-item'));
        return {
          scrollHeight: header.scrollHeight,
          clientHeight: header.clientHeight,
          scrollWidth: header.scrollWidth,
          clientWidth: header.clientWidth,
          userRight: userButton ? userButton.getBoundingClientRect().right : null,
          viewportWidth: window.innerWidth,
          segmentedWidth: segmented ? segmented.getBoundingClientRect().width : 0,
          segmentedItemTexts: items.map((item) => (item.textContent ?? '').trim()),
          headerText: header.textContent ?? '',
        };
      });
      expect(metrics).not.toBeNull();
      const m = metrics as NonNullable<typeof metrics>;

      // 纵向：单行 48px（修复前 216 vs 47）。
      expect(m.scrollHeight, '顶栏不得纵向溢出').toBeLessThanOrEqual(m.clientHeight + 1);
      // 横向：内容不得超出可视宽度（修复前 375 下用户名按钮右溢 6px）。
      expect(m.scrollWidth, '顶栏不得横向溢出').toBeLessThanOrEqual(m.clientWidth + 1);
      expect(m.userRight ?? 0).toBeLessThanOrEqual(m.viewportWidth);
      // 移动端不再渲染品牌文字（修复前品牌 rect 14×356px，被裁成竖排单字）。
      expect(m.headerText).not.toContain('云枢 OpsMind');
      // 三态切换器保留，但窄屏为图标态（无「浅色/深色/跟随系统」文字）。
      expect(m.segmentedWidth).toBeGreaterThan(0);
      expect(m.segmentedItemTexts.join('')).toBe('');
      // 控件仍可访问：aria-label 与每项 title 都在（读屏与悬停说明不丢）。
      await expect(page.locator('.app-header .ant-segmented')).toHaveAttribute(
        'aria-label',
        '主题',
      );
      await expect(page.locator('.app-header [title="浅色主题"]')).toBeVisible();
      await expect(page.locator('.app-header [title="深色主题"]')).toBeVisible();
      await expect(page.locator('.app-header [title="跟随系统明暗设置"]')).toBeVisible();
    });
  }

  test('宽屏保留文字态三态切换器（可点切换主题）', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await useTheme(page, 'light');
    await page.goto('/servers');
    const segmented = page.locator('.app-header .ant-segmented');
    await expect(segmented).toBeVisible({ timeout: 20_000 });
    await expect(segmented).toContainText('浅色');
    await expect(segmented).toContainText('跟随系统');
    await segmented.locator('.ant-segmented-item', { hasText: '深色' }).click();
    await expect
      .poll(() => page.evaluate(() => document.documentElement.dataset['theme']))
      .toBe('dark');
  });
});

// ---------------------------------------------------------------------------
// 2. 输入框 / Select 外框 ≥ 3:1（用 --color-border-strong）
// ---------------------------------------------------------------------------
test.describe('七期：输入类控件外框用可操作边界令牌', () => {
  for (const theme of ['light', 'dark'] as const) {
    test(`服务器页搜索框与 Select 外框 = --color-border-strong（${theme}）`, async ({ page }) => {
      await useTheme(page, theme);
      await page.goto('/servers');
      await expect(page.getByRole('heading', { name: '服务器' })).toBeVisible({ timeout: 20_000 });

      const expected = await resolveColor(page, '--color-border-strong');
      expect(expected).toMatch(/^rgb\(/);
      const weak = await resolveColor(page, '--color-border');
      expect(expected, 'border-strong 必须与装饰线 border 不同').not.toBe(weak);

      // 搜索框是 affix wrapper（Input prefix），Select 是 ant-select 选择器。
      const search = page.locator('.ant-input-affix-wrapper').first();
      const select = page.locator('.ant-select').first();
      await expect(search).toBeVisible();
      await expect(select).toBeVisible();
      await expect(search).toHaveCSS('border-top-color', expected);
      await expect(select).toHaveCSS('border-top-color', expected);

      // 全局装饰线不变：表格分隔/卡片描边仍是 --color-border（不误伤）。
      const card = page.locator('.ant-card').first();
      if ((await card.count()) > 0) {
        await expect(card).not.toHaveCSS('border-top-color', expected);
      }
    });
  }
});

// ---------------------------------------------------------------------------
// 3. 监控 tooltip 在浅色下不消失（描边 + 阴影）
// ---------------------------------------------------------------------------
test.describe('七期：监控图表 hover tooltip 两主题都有可见边界', () => {
  for (const theme of ['light', 'dark'] as const) {
    test(`tooltip 有 1px 描边与阴影（${theme}）`, async ({ page }) => {
      await useTheme(page, theme);
      const serverId = '11111111-2222-3333-4444-555555555555';
      const points = [10, 22, 35, 28, 40].map((value, index) => ({
        created_at: new Date(Date.UTC(2026, 10, 8, 9, index * 5)).toISOString(),
        cpu_pct: value,
        mem_pct: 40,
        disk_pct: 55,
        load_1: 0.4,
        net_in_bps: 1000,
        net_out_bps: 2000,
      }));
      await page.route('**/api/monitor/overview', (route) =>
        route.fulfill({
          json: {
            servers: [
              {
                server_id: serverId,
                server_name: 'p7-chart-host',
                created_at: points[points.length - 1]?.created_at,
                cpu_pct: 40,
                mem_pct: 40,
                disk_pct: 55,
                load_1: 0.4,
                net_in_bps: 1000,
                net_out_bps: 2000,
              },
            ],
            firing: { total: 0, critical: 0, warning: 0, info: 0 },
            last_inspection: null,
            collector: { enabled: false, interval_seconds: 60 },
          },
        }),
      );
      await page.route(`**/api/servers/${serverId}/metrics*`, (route) =>
        route.fulfill({ json: { server_id: serverId, points, truncated: false } }),
      );

      await page.goto('/monitor');
      const cpuCard = page.locator('.ant-card', { hasText: 'CPU 使用率' }).first();
      await expect(cpuCard).toBeVisible({ timeout: 20_000 });
      // 折线图是卡内的 <svg> 之一（另有一个「复制当前值」的图标 svg，14×14），
      // 按 polyline 精确定位绘图区。
      const svg = cpuCard
        .locator('svg')
        .filter({ has: page.locator('polyline') })
        .first();
      await expect(svg).toBeVisible();

      // 悬停在折线中段（x 约 60%），触发 hover tooltip。
      const box = await svg.boundingBox();
      expect(box).not.toBeNull();
      if (!box) {
        return;
      }
      await page.mouse.move(box.x + box.width * 0.6, box.y + box.height / 2);
      const tooltip = cpuCard.getByTestId('metric-chart-tooltip');
      await expect(tooltip).toBeVisible();

      const style = await tooltip.evaluate((element) => {
        const computed = getComputedStyle(element);
        return {
          borderTopWidth: computed.borderTopWidth,
          borderTopColor: computed.borderTopColor,
          borderTopStyle: computed.borderTopStyle,
          boxShadow: computed.boxShadow,
          background: computed.backgroundColor,
          cardBackground: getComputedStyle(element.closest('.ant-card') as HTMLElement)
            .backgroundColor,
          color: computed.color,
        };
      });
      const expectedBorder = await resolveColor(page, '--color-border');
      expect(style.borderTopWidth).toBe('1px');
      expect(style.borderTopStyle).toBe('solid');
      expect(style.borderTopColor).toBe(expectedBorder);
      expect(style.boxShadow, 'tooltip 必须有阴影').not.toBe('none');
      // 阴影 + 描边同时存在：浅色即使与卡片同底色也能看出浮层。
      expect(style.boxShadow.length).toBeGreaterThan(10);
      // 文字仍可读：tooltip 前景/背景必须不同。
      expect(style.color).not.toBe(style.background);
      expect(style.cardBackground).toBeTruthy();
    });
  }
});

// ---------------------------------------------------------------------------
// 4. 回放进度条轨道用 --color-border-strong
// ---------------------------------------------------------------------------
test.describe('七期：回放进度条未播放轨道可见', () => {
  const RECORDING_ID = 'e2e-p7-0000-4000-8000-0000000000aa';
  const CAST = [
    JSON.stringify({ version: 2, width: 120, height: 40 }),
    JSON.stringify([0.0, 'o', '$ uptime\r\n']),
    JSON.stringify([6.0, 'o', 'up 10 days\r\n']),
  ].join('\n');

  function recording() {
    const now = new Date().toISOString();
    return {
      id: RECORDING_ID,
      server_id: 'seed-server',
      server_name: 'e2e-fake-host',
      os: 'linux',
      username: 'admin',
      bytes_written: CAST.length,
      started_at: now,
      ended_at: now,
      created_at: now,
      updated_at: now,
    };
  }

  for (const theme of ['light', 'dark'] as const) {
    test(`rail 背景 = --color-border-strong，track = --color-primary（${theme}）`, async ({
      page,
    }) => {
      await useTheme(page, theme);
      await page.route('**/api/term/recordings**', async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname === `/api/term/recordings/${RECORDING_ID}`) {
          await route.fulfill({
            status: 200,
            contentType: 'application/octet-stream',
            body: CAST,
          });
          return;
        }
        await route.fulfill({ json: { items: [recording()] } });
      });
      await page.route('**/api/rdp/recordings**', (route) =>
        route.fulfill({ json: { items: [] } }),
      );

      await page.goto('/recordings');
      await page.getByRole('button', { name: '回放' }).click();
      const slider = page.locator('.opsmind-player-slider');
      await expect(slider).toBeVisible({ timeout: 20_000 });
      await expect(page.locator('.opsmind-player-time')).toHaveText('0:00 / 0:06');

      const expectedRail = await resolveColor(page, '--color-border-strong');
      const expectedTrack = await resolveColor(page, '--color-primary');
      await expect(slider.locator('.ant-slider-rail')).toHaveCSS('background-color', expectedRail);
      // 播放位置 0 时 track 宽度为 0，但背景色仍取自 primary（不是默认灰）。
      await expect(slider.locator('.ant-slider-track')).toHaveCSS(
        'background-color',
        expectedTrack,
      );
      // 修复前 rail 是 rgba(0,0,0,.04) 压白 ≈1.07:1——显式排除。
      const rail = await slider.locator('.ant-slider-rail').evaluate((element) => {
        return getComputedStyle(element).backgroundColor;
      });
      expect(rail).not.toContain('rgba');
    });
  }
});

// ---------------------------------------------------------------------------
// 5. 表格空态提示不得用禁用色（4 个页面同源 token）
// ---------------------------------------------------------------------------
test.describe('七期：表格空态提示用三级文本', () => {
  for (const theme of ['light', 'dark'] as const) {
    test(`回放页两个空表提示 = --color-text-tertiary（${theme}）`, async ({ page }) => {
      await useTheme(page, theme);
      await page.route('**/api/term/recordings**', (route) =>
        route.fulfill({ json: { items: [] } }),
      );
      await page.route('**/api/rdp/recordings**', (route) =>
        route.fulfill({ json: { items: [] } }),
      );

      await page.goto('/recordings');
      await expect(page.getByText('暂无终端录制')).toBeVisible({ timeout: 20_000 });
      const expected = await resolveColor(page, '--color-text-tertiary');
      const disabled = await resolveColor(page, '--color-text-disabled');
      expect(expected).not.toBe(disabled);

      const placeholder = page.locator('.ant-table-placeholder').first();
      await expect(placeholder).toHaveCSS('color', expected);
      await expect(placeholder).not.toHaveCSS('color', disabled);

      // RDP 页签同源（同一 components.Table.colorTextDisabled）。
      await page.getByTestId('rdp-recordings-tab').click();
      await expect(page.getByText('暂无 RDP 录制')).toBeVisible();
      const rdpPlaceholder = page.locator('.ant-table-placeholder').first();
      await expect(rdpPlaceholder).toHaveCSS('color', expected);
    });
  }
});

// ---------------------------------------------------------------------------
// 6. 容器页不默认选中无运行时服务器
// ---------------------------------------------------------------------------
test.describe('七期：容器页不默认选中 runtime=none 的服务器', () => {
  test('仅 none/containerd 资产：不发容器请求、无红色错误横幅、给出选择指引', async ({ page }) => {
    const noneId = 'aaaaaaaa-1111-4111-8111-111111111111';
    const containerdId = 'bbbbbbbb-2222-4222-8222-222222222222';
    await page.route('**/api/servers', async (route) => {
      if (route.request().method() !== 'GET') {
        await route.fallback();
        return;
      }
      await route.fulfill({
        json: {
          items: [
            serverPayload(noneId, 'p7-none-host', 'none'),
            serverPayload(containerdId, 'p7-containerd-host', 'containerd'),
          ],
        },
      });
    });
    const containerCalls: string[] = [];
    await page.route('**/api/servers/*/containers', async (route) => {
      containerCalls.push(route.request().url());
      await route.fulfill({
        status: 400,
        json: {
          error: {
            code: 'CONTAINER_UNSUPPORTED',
            message: 'server has no supported container runtime (container_runtime=none)',
            request_id: 'req-p7-none',
            details: { runtime: 'none' },
          },
        },
      });
    });

    const consoleErrors: string[] = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') {
        consoleErrors.push(msg.text());
      }
    });

    await page.goto('/containers');
    await expect(page.getByRole('heading', { name: '容器' })).toBeVisible({ timeout: 20_000 });
    // 空态说明「请先选择支持容器运行时的服务器」。
    await expect(page.getByText(/请先选择支持容器运行时的服务器/).first()).toBeVisible();
    // 「重新探测」入口保留（真实探测走 POST test）。
    await expect(page.getByRole('button', { name: '重新探测' })).toBeEnabled();
    // 不得发出任何容器列表请求。
    expect(containerCalls, '不得预选中无运行时服务器并请求容器列表').toEqual([]);
    // 不得出现红色错误横幅 / 400。
    await expect(page.locator('.ant-alert-error')).toHaveCount(0);
    expect(consoleErrors.filter((text) => text.includes('400'))).toEqual([]);
  });

  test('存在 docker 服务器时默认选中它（跳过更靠前的 none）', async ({ page }) => {
    const noneId = 'cccccccc-3333-4333-8333-333333333333';
    const dockerId = 'dddddddd-4444-4444-8444-444444444444';
    await page.route('**/api/servers', async (route) => {
      if (route.request().method() !== 'GET') {
        await route.fallback();
        return;
      }
      await route.fulfill({
        json: {
          items: [
            serverPayload(noneId, 'p7-first-none', 'none'),
            serverPayload(dockerId, 'p7-second-docker', 'docker'),
          ],
        },
      });
    });
    await page.route(`**/api/servers/${dockerId}/containers`, (route) =>
      route.fulfill({
        json: {
          runtime: 'docker',
          items: [
            {
              id: 'a1b2c3d4e5f6',
              name: 'p7-web-1',
              image: 'nginx:1.27',
              state: 'running',
              status: 'Up 2 hours',
              ports: [],
              created: '2026-11-08T08:00:00Z',
            },
          ],
        },
      }),
    );
    const requests: string[] = [];
    page.on('request', (request) => {
      if (/\/api\/servers\/[^/]+\/containers$/.test(new URL(request.url()).pathname)) {
        requests.push(request.url());
      }
    });

    await page.goto('/containers');
    await expect(page.getByText('p7-web-1').first()).toBeVisible({ timeout: 20_000 });
    expect(requests.every((url) => url.includes(dockerId))).toBe(true);
    expect(requests.some((url) => url.includes(noneId))).toBe(false);
    // 页头运行时徽标为 Docker，且下拉当前值是 docker 那台。
    await expect(page.locator('.ant-tag', { hasText: 'Docker' }).first()).toBeVisible();
    await expect(
      page.locator('.ant-select-content', { hasText: 'p7-second-docker' }),
    ).toBeVisible();
  });

  test('用户显式选中 docker 服务器后列表照常加载', async ({ page }) => {
    const noneId = 'eeeeeeee-5555-4555-8555-555555555555';
    const dockerId = 'ffffffff-6666-4666-8666-666666666666';
    await page.route('**/api/servers', async (route) => {
      if (route.request().method() !== 'GET') {
        await route.fallback();
        return;
      }
      await route.fulfill({
        json: {
          items: [
            serverPayload(noneId, 'p7-explicit-none', 'none'),
            serverPayload(dockerId, 'p7-explicit-docker', 'docker'),
          ],
        },
      });
    });
    await page.route(`**/api/servers/${dockerId}/containers`, (route) =>
      route.fulfill({
        json: {
          runtime: 'docker',
          items: [
            {
              id: '0f1e2d3c4b5a',
              name: 'p7-explicit-web',
              image: 'redis:7',
              state: 'running',
              status: 'Up 1 hour',
              ports: [],
              created: '2026-11-08T09:00:00Z',
            },
          ],
        },
      }),
    );

    await page.goto('/containers');
    await page.getByRole('combobox', { name: '选择服务器' }).click();
    const option = page
      .locator('.ant-select-item-option', { hasText: 'p7-explicit-docker' })
      .first();
    for (let i = 0; i < 40 && !(await option.isVisible()); i++) {
      await page.keyboard.press('ArrowDown');
    }
    await option.click();
    await expect(page.getByText('p7-explicit-web').first()).toBeVisible({ timeout: 20_000 });
  });
});
