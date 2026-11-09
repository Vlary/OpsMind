import { expect, test, type Page } from '@playwright/test';
import { NAV_ITEMS } from '../src/components/navItems';
import {
  AUTH_STATE_FILE,
  REASONING_BODY_PREFIX,
  REASONING_FINAL,
  SCENARIO_REASONING,
  sendMessage,
} from './helpers';

// 六期打磨 sprint 1：深度思考流式全链路 E2E。fake-llm 的
// __scenario_reasoning__ 先逐段（350ms 慢节奏）回 delta.reasoning_content
// 再逐 token 回正文——此前全部 10 个场景都不输出 reasoning_content，深度
// 思考只有组件单测与回放 fake 数据覆盖，流式全链路（LLM→SSE→折叠/展开/
// 复制）无回归保护。

test.use({ storageState: AUTH_STATE_FILE });

async function readClipboard(page: Page): Promise<string> {
  return page.evaluate(() => navigator.clipboard.readText());
}

test.describe('六期打磨：深度思考流式全链路', () => {
  test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

  test('reasoning_content 流：思考中自动展开 → 正文后回折叠 → 复制全文 → 折叠不丢内容', async ({
    page,
  }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_REASONING);
    await expect(page).toHaveURL(/\/c\//, { timeout: 20_000 });

    // 1. 首个 reasoning 段到达：自动展开 + 「思考中…」摘要（段间慢节奏
    //    留出可捕捉的展开窗口；MessageList 的 reasoningStreaming =
    //    streaming && reasoning !== '' && text === ''）。
    const toggle = page.locator('.chat-reasoning-toggle');
    await expect(toggle).toBeVisible({ timeout: 15_000 });
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(toggle).toContainText('思考中');

    // 2. 正文首 token 到达后自动回折叠：摘要切「深度思考」（流式带耗时、
    //    持久化无计时，两种文案都以「深度思考」开头），正文上屏。
    await expect(page.getByText(REASONING_BODY_PREFIX).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(toggle).toContainText('深度思考');

    // 等流结束（发送按钮恢复）：live 撤除后操作持久化渲染的 ReasoningBlock，
    // 避免展开/折叠断言撞上 live→持久化切换重挂载。
    await expect(page.getByRole('button', { name: '发送' })).toBeVisible({ timeout: 20_000 });

    // 3. 手动展开：统一 CopyButton 的复制全文入口出现，复制后剪贴板 =
    //    思考全文（reasoningCopyTarget 恒为完整文本，不因折叠截断）。
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const copy = page.locator('.chat-reasoning-copy');
    await expect(copy).toBeVisible();
    await copy.click();
    await expect(page.getByText('已复制').first()).toBeVisible();
    expect(await readClipboard(page)).toBe(REASONING_FINAL);

    // 4. 再折叠：折叠不丢内容——display:none 收起但 DOM 与字符串原样
    //    保留（契约 8.6 第 13 条）。
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    const body = page.locator('.chat-reasoning-block-body');
    await expect(body).toBeHidden();
    await expect(body).toContainText(REASONING_FINAL);
  });
});

// ---------------------------------------------------------------------------
// 六期打磨 sprint 1（走查发现）+ 七期契约 8.3.10：全量导航页的「导航 label /
// 页头标题 / 标签页 title」三方必须逐字相同。实机采样曾为：
//   /rdp   | nav="桌面" | header="RDP 桌面" | title="桌面 · 云枢 OpsMind"
//   /batch | nav="批量" | header="批量操作" | title="批量 · 云枢 OpsMind"
// 统一口径为**页头跟随导航**（nav = 页头标题 = 标签页 title）。
//
// 用例由 NAV_ITEMS 驱动（契约 8.3.10：不得手写页面名副本，否则会漂移）——
// 导航项新增/改名后这里自动覆盖，无需同步维护第二份清单。
// ---------------------------------------------------------------------------
test.describe('导航 / 页头 / 标签页三方一致', () => {
  test('逐页断言 nav label、PageHeader 标题、document.title 三者相同', async ({ page }) => {
    for (const item of NAV_ITEMS) {
      await page.goto(item.key);
      await expect(page.locator('.app-header')).toBeVisible({ timeout: 20_000 });
      // 页头（PageHeader → h3）与标签页 title。
      await expect(page.getByRole('heading', { name: item.label, exact: true })).toBeVisible({
        timeout: 20_000,
      });
      await expect(page).toHaveTitle(`${item.label} · 云枢 OpsMind`);
      // 导航项（菜单当前选中项文案 = 同一名称）。
      const selectedNav = page.locator('.ant-menu-item-selected').first();
      await expect(selectedNav).toContainText(item.label);
      // 三方逐字相同：没有「RDP 桌面」「批量操作」等变体。
      const heading =
        (await page.getByRole('heading', { name: item.label, exact: true }).textContent()) ?? '';
      expect(heading.trim(), item.key).toBe(item.label);
      expect((await selectedNav.textContent())?.trim(), item.key).toBe(item.label);
    }
  });
});
