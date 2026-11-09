import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 四期冲刺 2 打磨（两项）：
//  1) 审计页 container.action 中文标签：契约 9.7 四期唯一新增审计动作
//     （console 审批 decide 批准后执行 / container_action 工具执行两路径
//     落审计）。修复前 AUDIT_ACTIONS 无该条目——动作列主行显示英文原码、
//     动作筛选下拉（数据源即 AUDIT_ACTIONS）搜不到容器操作记录。
//  2) 容器操作审批弹窗 pending 态按钮：原「稍后处理」承诺了无法兑现的
//     操作——关闭弹窗后该 console 审批在全产品无任何再决策入口（无审批
//     列表页），只能静默过期。修复为中性「关闭」+ 旁注明示过期后果。

test.use({ storageState: AUTH_STATE_FILE });

const ORIGIN = 'http://localhost:4173';

test.describe('审计页 container.action 中文标签与筛选项', () => {
  test('行内显示「容器操作」，动作筛选下拉可选并按该动作发请求', async ({ page }) => {
    // route mock 一条 container.action 记录（detail 口径同契约 9.7：
    // runtime/container_id/action/exit_code/source）。
    const auditPayload = {
      items: [
        {
          id: 9_001,
          created_at: '2026-10-27T08:00:00Z',
          actor_type: 'user',
          actor_id: '5b0c7b6a-9e59-4d34-93e2-2c87a5831101',
          actor_name: 'admin',
          action: 'container.action',
          resource_type: 'server',
          resource_id: '8f1a6f43-0d61-4b9e-8bc3-37c95d01e4a7',
          request_id: 'req-e2e-p4s2',
          conversation_id: null,
          server_id: '8f1a6f43-0d61-4b9e-8bc3-37c95d01e4a7',
          ip: '127.0.0.1',
          detail: {
            runtime: 'docker',
            container_id: 'c1d2e3f4a5b6',
            action: 'stop',
            exit_code: 0,
            source: 'console',
          },
          prev_hash: 'prev',
          hash: 'hash',
        },
      ],
      has_more: false,
      next_before_id: null,
    };
    const actionQueries: string[] = [];
    await page.route('**/api/audit*', async (route) => {
      const url = new URL(route.request().url());
      actionQueries.push(url.searchParams.get('action') ?? '');
      await route.fulfill({ json: auditPayload });
    });

    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();
    const table = page.locator('.ant-table');
    await expect(table).toBeVisible();

    // 动作单元格：主行必须是中文标签（修复前是英文原码 container.action）；
    // 副行保留原码是既有设计（同操作者列 UUID 副行），断言主行精确文本。
    const row = table.locator('.ant-table-row').first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    const actionCell = row.locator('td', { hasText: 'container.action' }).first();
    await expect(actionCell.locator('div > div').first()).toHaveText('容器操作');

    // 动作筛选下拉（数据源即 AUDIT_ACTIONS）：修复前搜不到容器操作。
    // 键盘过滤 + 选中（同 polish-audit-actor-name 先例：placeholder 搜索
    // 输入会拦截点击）。
    await page.locator('.opsmind-admin-toolbar .ant-select').first().click();
    await page.keyboard.type('容器操作');
    const option = page.locator('.ant-select-item-option', { hasText: '容器操作' }).first();
    await expect(option).toBeVisible();
    await option.click();
    await expect(page.locator('.opsmind-admin-toolbar .ant-select').first()).toContainText(
      '容器操作',
    );

    // 选中后按 action=container.action 重新发请求，mock 下发同一条记录仍渲染。
    await expect.poll(() => actionQueries[actionQueries.length - 1]).toBe('container.action');
    await expect(table.locator('.ant-table-row').first()).toBeVisible();
    await expect(
      table.locator('.ant-table-row').first().locator('td', { hasText: 'container.action' }),
    ).toBeVisible();
  });
});

test.describe('容器操作弹窗 pending 态按钮为中性「关闭」+ 过期旁注', () => {
  test('「稍后处理」不再出现，旁注明示自动过期，点关闭弹窗收起', async ({ page }) => {
    // 真实创建服务器（127.0.0.1:1，SSH 立拒不影响——列表与审批均 route mock）。
    const created = await page.request.post('/api/servers', {
      headers: { Origin: ORIGIN },
      data: {
        name: `p4s2-modal-close-${Date.now()}`,
        host: '127.0.0.1',
        port: 1,
        os: 'linux',
        auth_method: 'password',
        username: 'e2e',
        password: 'e2e-pass',
        auto_exec_enabled: false,
      },
    });
    expect(created.status()).toBe(201);
    const serverId = ((await created.json()) as { id: string }).id;

    try {
      await page.route(`**/api/servers/${serverId}/containers`, async (route) => {
        await route.fulfill({
          json: {
            runtime: 'docker',
            items: [
              {
                id: 'c1d2e3f4a5b6',
                name: 'p4s2-web',
                image: 'nginx:1.27',
                state: 'running',
                status: 'Up 2 hours (healthy)',
                ports: [],
                created: '2026-10-26T12:00:00Z',
              },
            ],
          },
        });
      });
      // POST action 不真执行：mock 返回 source=console 的 pending 审批
      //（TTL 与生产 120s 同量级，保证倒计时远未到零）。
      await page.route(`**/api/servers/${serverId}/containers/*/action`, async (route) => {
        await route.fulfill({
          json: {
            approval: {
              id: '3f2b1c9a-7c64-4e0f-9a1d-58e0f4a2b6c8',
              conversation_id: null,
              tool_call_id: null,
              source: 'console',
              server_id: serverId,
              server_name: 'p4s2-modal-close',
              command: 'docker stop c1d2e3f4a5b6',
              risk_level: 'needs_approval',
              rule_id: 'CONTAINER_CHANGE',
              reason: '容器变更需人工批准',
              status: 'pending',
              requested_at: new Date().toISOString(),
              expires_at: new Date(Date.now() + 120_000).toISOString(),
              decided_at: null,
              decided_by: null,
              note: '',
            },
          },
        });
      });

      await page.goto('/containers');
      await expect(page.getByRole('heading', { name: '容器' })).toBeVisible();

      // antd Select 虚拟滚动：方向键滚到目标项再点选（同 containers.spec 先例）。
      const option = page
        .locator('.ant-select-item-option', { hasText: 'p4s2-modal-close-' })
        .first();
      await page.getByRole('combobox', { name: '选择服务器' }).click();
      for (let i = 0; i < 80 && !(await option.isVisible()); i++) {
        await page.keyboard.press('ArrowDown');
      }
      await option.click();

      // mock 列表渲染后发起「停止」→ confirm 态「创建审批」→ pending 态。
      await expect(page.getByText('p4s2-web').first()).toBeVisible();
      await page.getByRole('button', { name: '停止' }).first().click();
      const dialog = page.getByRole('dialog');
      await expect(dialog.getByRole('button', { name: '创建审批' })).toBeVisible();
      await dialog.getByRole('button', { name: '创建审批' }).click();

      // pending 态：按钮是中性「关闭」，旧「稍后处理」不复存在；旁注明示
      // 关闭后果；批准/拒绝仍在（回归）。限定 .ant-modal-body：antd zh_CN
      // locale 下右上角 X 的 aria-label 也是「关闭」，须与 body 内按钮区分。
      const body = dialog.locator('.ant-modal-body');
      await expect(dialog.getByText('剩余')).toBeVisible();
      // antd 对两字按钮自动插空格（accessible name「关 闭」「拒 绝」「批 准」，
      // 同登录按钮「登 录」先例），正则兼容；.ant-modal-body 限定避开右上角
      // X（zh_CN locale 下 aria-label 也是「关闭」）。
      await expect(body.getByRole('button', { name: /关\s*闭/ })).toBeVisible();
      await expect(body.getByRole('button', { name: '稍后处理' })).toHaveCount(0);
      await expect(body.getByRole('button', { name: /拒\s*绝/ })).toBeVisible();
      await expect(body.getByRole('button', { name: /批\s*准/ })).toBeVisible();
      await expect(
        dialog.getByText(
          '关闭后审批将在倒计时结束时自动过期；如仍需操作，请重新发起并创建新审批。',
        ),
      ).toBeVisible();

      // 点「关闭」仅收起弹窗（不 decide、不执行）。
      await body.getByRole('button', { name: /关\s*闭/ }).click();
      await expect(dialog).toBeHidden();
      await expect(page.getByText('p4s2-web').first()).toBeVisible();
    } finally {
      await page.request.delete(`/api/servers/${serverId}`, { headers: { Origin: ORIGIN } });
    }
  });
});
