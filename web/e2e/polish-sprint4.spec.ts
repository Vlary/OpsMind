import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE, SCENARIO_RISKY, sendMessage } from './helpers';

// 冲刺 4（第 4 轮新序列）打磨项的端到端验证：
//  - 停止含未决审批的回答：审批卡立即终态「已取消（回答流已中断，审批
//    未生效）」，不再死锁「待处理」倒计时、批准按钮不再可点（修复前可点
//    但必返回 409，本地倒计时归零后又误显「已超时」）；
//  - 404 页：自定义 icon 替换 antd v6 内置插画自带的拼写错误英文
//    「No Found」（正确英文应为 Not Found），保留中文 title/subTitle；
//  - 审计目标 ID 筛选：粘贴全大写 UUID 归一化为小写后命中库内记录
//    （修复前 0 行且无任何提示）；
//  - 数据库重名 409：API message 细化为 "database name already exists"，
//    UI 表单错误条拼接展示具体原因（对齐 servers/providers）。

const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

test.describe('冲刺 4 打磨', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('停止含未决审批的回答：审批卡立即「已取消」，批准按钮消失', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    // 前置确认：停止前审批确实 pending（倒计时 + 可点批准）。
    await expect(approvalCard).toContainText(/剩余 \d+:\d{2}/);
    await expect(approvalCard.getByRole('button', { name: '批准' })).toBeEnabled();

    // 停止并确认（弹窗交互由 polish-round9 用例保证，这里直接走到确认）。
    await page.locator('.chat-composer').getByRole('button', { name: '停止' }).click();
    const confirm = page.getByRole('dialog', { name: '停止当前回答？' });
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: /停\s*止/ }).click();
    await expect(confirm).toBeHidden();

    // 修复点 1（stop() 同步写终态）：确认停止后卡片立即显示「已取消」，
    // 不再显示「待处理」倒计时；批准按钮随终态消失（点了也只会 409）。
    // 断言限定在审批卡作用域：live 条目撤除与持久化分组落地之间存在
    // 毫秒级过渡（两份卡片同屏），全局 getByText 会撞 strict mode。
    const cancelledAlert = approvalCard.getByText('本轮已取消（回答流已中断，审批未生效）');
    await expect(cancelledAlert).toBeVisible({ timeout: 10_000 });
    await expect(approvalCard).not.toContainText('剩余');
    await expect(approvalCard.getByRole('button', { name: '批准' })).toHaveCount(0);
    await expect(approvalCard.getByRole('button', { name: /拒\s*绝/ })).toHaveCount(0);

    // 落库消息落地（live 回合撤除、审批卡改为持久化渲染）后，终态保持
    // 「已取消」而不是本地倒计时归零误显「已超时」。
    await expect(page.getByRole('button', { name: '发送' })).toBeVisible({ timeout: 20_000 });
    await expect(
      page.locator('.chat-message--assistant', { hasText: '已停止' }).first(),
    ).toBeVisible({
      timeout: 30_000,
    });
    await expect(cancelledAlert).toBeVisible();
    await expect(approvalCard).not.toContainText('剩余');
    await expect(approvalCard).not.toContainText('已超时');
  });

  test('404 页：不出现内置插画英文「No Found」，中文文案与返回入口完整', async ({ page }) => {
    await page.goto('/no-such-route');

    await expect(page.getByText('页面不存在或已被移动，请检查地址是否输入正确。')).toBeVisible();
    // 修复前：antd v6 404 内置插画自带拼写错误英文「No Found」。
    await expect(page.getByText('No Found')).toHaveCount(0);
    expect(await page.textContent('body')).not.toContain('No Found');

    const back = page.getByRole('button', { name: '返回对话' });
    await expect(back).toBeVisible();
    await back.click();
    await expect(page).toHaveURL(/\/chat$/);
  });

  test('审计目标 ID 筛选：大写 UUID 归一化小写后命中记录', async ({ page }) => {
    // 种一条 database.create 审计并取真实资源 UUID（恒小写）。
    const created = await page.request.post('/api/databases', {
      headers: ORIGIN_HEADERS,
      data: {
        name: 'e2e-sprint4-uuid-case',
        engine: 'sqlite',
        sqlite_path: '/nonexistent-sprint4-uuid-case/seed.db',
      },
    });
    expect(created.status()).toBe(201);
    const resourceId = ((await created.json()) as { id: string }).id;

    try {
      await page.goto('/audit');
      await expect(page.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

      // 归一化提交的证据：实际发出的 resource_id 查询参数是小写。
      const requestedIds: string[] = [];
      page.on('request', (request) => {
        const match = /[?&]resource_id=([0-9a-fA-F-]+)/.exec(request.url());
        if (match) {
          requestedIds.push(match[1] ?? '');
        }
      });

      // 修复前：全大写通过 /i 校验但按字面过滤 → 0 行且无提示。
      const search = page.getByPlaceholder('目标 ID（资源 UUID）');
      await search.fill(resourceId.toUpperCase());
      await page
        .locator('.opsmind-admin-toolbar')
        .getByRole('button', { name: /筛\s*选/ })
        .nth(0)
        .click();

      const rows = page.locator('.ant-table-row');
      await expect(rows.first()).toBeVisible({ timeout: 20_000 });
      expect(await rows.count()).toBeGreaterThanOrEqual(1);
      await expect(rows.first()).toContainText('新增数据库');
      expect(requestedIds).toContain(resourceId.toLowerCase());
    } finally {
      await page.request.delete(`/api/databases/${resourceId}`, { headers: ORIGIN_HEADERS });
    }
  });

  test('数据库重名 409：message 细化为名称冲突，UI 表单错误条展示具体原因', async ({ page }) => {
    // API 层：创建同库重名 → 409 + 具体名称冲突文案（对齐 servers）。
    const created = await page.request.post('/api/databases', {
      headers: ORIGIN_HEADERS,
      data: {
        name: 'e2e-sprint4-dup-name',
        engine: 'sqlite',
        sqlite_path: '/nonexistent-sprint4-dup/seed.db',
      },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };

    try {
      const conflict = await page.request.post('/api/databases', {
        headers: ORIGIN_HEADERS,
        data: {
          name: 'e2e-sprint4-dup-name',
          engine: 'sqlite',
          sqlite_path: '/nonexistent-sprint4-dup/other.db',
        },
      });
      expect(conflict.status()).toBe(409);
      const body = (await conflict.json()) as { error: { code: string; message: string } };
      expect(body.error.code).toBe('CONFLICT');
      expect(body.error.message).toBe('database name already exists');

      // UI 侧：表单弹窗的错误条把具体 message 拼进中文标题展示给用户
      // （走查场景：修复前只剩通用文案「资源冲突或当前状态不允许该操作」）。
      // 默认引擎 postgres：名称 + 主机/数据库名/用户名即可提交。
      await page.goto('/databases');
      await page.getByRole('button', { name: '新增数据库' }).click();
      const dialog = page.getByRole('dialog', { name: '新增数据库' });
      await expect(dialog).toBeVisible();
      await dialog.getByPlaceholder('例如 orders-prod').fill('e2e-sprint4-dup-name');
      await dialog.getByPlaceholder('db.example.com').fill('127.0.0.9');
      await dialog.getByPlaceholder('app').fill('app');
      await dialog.getByPlaceholder('readonly').fill('readonly');
      await dialog.getByRole('button', { name: /保\s*存/ }).click();

      await expect(
        dialog.getByText('资源冲突或当前状态不允许该操作（database name already exists）'),
      ).toBeVisible({ timeout: 20_000 });
    } finally {
      await page.request.delete(`/api/databases/${id}`, { headers: ORIGIN_HEADERS });
    }
  });
});
