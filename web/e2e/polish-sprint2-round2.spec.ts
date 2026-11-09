import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 冲刺 2 第 2 轮打磨验证：审计页「按动作筛选」的选项可输入关键词定位。
// （六期打磨 sprint 4 补齐五期/六期 18 个动作后去掉写死的 44 项口径——
// 选项数随契约增长，断言只锚定 label 子集。）
// 走查反馈「无搜索框、只能滚动/ArrowDown 逐项导航」；代码中该 Select 自
// 初始提交起就有 showSearch + optionFilterProp="label"（AuditPage.tsx
// 「按动作筛选」），antd 的搜索输入就是 Select 本体（打开后直接键入过滤，
// 下拉面板顶部没有独立搜索框，易被误读为无搜索能力）。本用例在真实 UI
// 上实证：键入中文关键词后选项即刻过滤到匹配子集，防将来回归。

test.describe('审计页按动作筛选支持关键词过滤', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('键入「审批」只剩 5 个审批动作；「删除」只剩 6 个删除动作；Enter 可选中', async ({
    page,
  }) => {
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();

    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    const dropdown = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)');
    const options = dropdown.locator('.ant-select-item-option');

    // 打开下拉，直接键入「审批」（showSearch 生效时 Select 本体即搜索输入）。
    await actionSelect.click();
    await expect(dropdown).toBeVisible();
    // 过滤前选项含登录/服务器等（虚拟滚动只渲染可视区，不依赖具体数量）。
    await expect(options.filter({ hasText: '登录成功' })).toBeVisible();

    await page.keyboard.type('审批');
    // AUDIT_ACTIONS 中 label 含「审批」的恰好 5 项：创建审批/审批通过/
    // 审批拒绝/审批过期/审批随流取消。
    await expect(options).toHaveCount(5);
    for (const label of ['创建审批', '审批通过', '审批拒绝', '审批过期', '审批随流取消']) {
      await expect(options.filter({ hasText: label })).toBeVisible();
    }
    // 不含「审批」的选项被过滤掉。
    await expect(options.filter({ hasText: '新增服务器' })).toHaveCount(0);

    // Enter 选中高亮项：默认高亮首项「创建审批」，ArrowDown 移到「审批通过」。
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(actionSelect).toContainText('审批通过');

    // 换一个走查点名的关键词「删除」：6 个删除动作（服务器/数据库/供应商/
    // 会话 + 六期打磨 sprint 4 补齐的「删除告警规则」「删除通知渠道」）。
    await actionSelect.click();
    await page.keyboard.type('删除');
    await expect(options).toHaveCount(6);
    for (const label of [
      '删除服务器',
      '删除数据库',
      '删除供应商',
      '删除会话',
      '删除告警规则',
      '删除通知渠道',
    ]) {
      await expect(options.filter({ hasText: label })).toBeVisible();
    }

    // 清空过滤词后可重新搜索（不依赖全量列表渲染：下拉是虚拟滚动，
    // 清空后滚动位置不定，首项可能不在渲染窗口，改为再次键入短词验证）。
    // label 含「登录」的恰好 4 项：登录成功/登录失败/退出登录/登录限流。
    await page.keyboard.press('Control+a');
    await page.keyboard.press('Delete');
    await page.keyboard.type('登录');
    await expect(options).toHaveCount(4);
    for (const label of ['登录成功', '登录失败', '退出登录', '登录限流']) {
      await expect(options.filter({ hasText: label })).toBeVisible();
    }
    await page.keyboard.press('Escape');
  });
});
