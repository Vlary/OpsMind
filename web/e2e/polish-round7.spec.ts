import { expect, test } from '@playwright/test';

import { AUTH_STATE_FILE, DEFAULT_FINAL, SCENARIO_SLOW, sendMessage } from './helpers';

// 走查驱动第 2 轮打磨的端到端验证（沿用 polish-roundN 编号顺延）：
//  - A2（high）：反向代理挂住 SSE 连接（后端被 SIGKILL 等拓扑）时，前端
//    永久卡「正在生成…」→ 空闲看门狗 30s 兜底中止并提示网络异常；
//    后端恢复落库 error 消息后 live 消退，「已停止」标记接管。
//  - A2 附带：悬挂轮（user 已落库、无任何 assistant 回复）在 user 消息上
//    提供「重新生成」，刷新后也有恢复入口。
//  - D1/D3：输入框超长粘贴被截断时必须有可见计数（showCount）。
//  - C1：服务器/数据库端口输 0 不再被 blur 静默 clamp 成 1 后直接建档，
//    由 Form 校验显式报「端口范围为 1-65535」并拦截提交。

test.describe('走查二：流式空闲看门狗', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('代理挂住 SSE：30s 无帧后中止并提示网络异常，不再永久卡「正在生成」', async ({ page }) => {
    // 页面级模拟反向代理半开连接：/api/chat 透传真实后端（user 消息正常
    // 落库、SSE 头与首帧 token 正常到达），但把响应体替换为「转发第一帧后
    // 永远挂起」的流——浏览器视角与走查 A2（SIGKILL 后端经 vite preview
    // 代理）一致：fetch reader 既不结束也不抛错。abort 时同步向挂起流注入
    // AbortError，模拟真实连接被中止后 read() reject 的行为。
    await page.addInitScript(() => {
      const origFetch = window.fetch.bind(window);
      window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
        const url =
          typeof input === 'string' ? input : input instanceof Request ? input.url : String(input);
        if (!url.includes('/api/chat')) {
          return origFetch(input, init);
        }
        return origFetch(input, init).then((real) => {
          const reader = real.body ? real.body.getReader() : null;
          const hung = new ReadableStream<Uint8Array>({
            start(controller) {
              if (!reader) {
                controller.close();
                return;
              }
              void reader.read().then(({ done, value }) => {
                if (!done && value) {
                  controller.enqueue(value);
                }
                // 之后不再读、不 close：连接挂死。
              });
              init?.signal?.addEventListener('abort', () => {
                try {
                  controller.error(new DOMException('Aborted', 'AbortError'));
                } catch {
                  // 流已结束，忽略。
                }
                void reader.cancel().catch(() => undefined);
              });
            },
          });
          return new Response(hung, real);
        });
      };
    });

    await page.goto('/chat');
    await sendMessage(page, SCENARIO_SLOW);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 先确认进入流式：慢速场景首帧 token 已透传，「正在生成…」出现。
    await expect(page.getByText('正在生成…')).toBeVisible({ timeout: 20_000 });

    // 修复前：此处永久停留（走查实测 22s+ 仍无断线提示）。
    // 修复后：距首帧 30s 触发看门狗 → NETWORK_ERROR 横幅（可重试语义）。
    await expect(page.getByText(/30 秒未收到新数据/)).toBeVisible({ timeout: 45_000 });

    // 中止链路完整走通（catch→finally）：phase 回 idle，「正在生成…」消失。
    await expect(page.getByText('正在生成…')).toHaveCount(0, { timeout: 45_000 });

    // 横幅可关闭：忽略后 live 回合保留「已停止」标记（内容可能截断，
    // 不以完整回答的外观留存），不再是无法摆脱的永久卡死状态。
    await page.getByRole('button', { name: '忽略' }).click();
    await expect(page.getByText('已停止', { exact: true }).first()).toBeVisible({
      timeout: 10_000,
    });
  });

  test('悬挂轮：user 消息无 assistant 回复时出现重新生成，点击后新一轮跑通', async ({ page }) => {
    // 正常跑完一轮（user + assistant 均落库）。
    await page.goto('/chat');
    await sendMessage(page, '走查二悬挂轮恢复入口验证');
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    await expect(page.getByText(DEFAULT_FINAL)).toBeVisible({ timeout: 30_000 });

    // 改写消息列表响应：滤掉 assistant 消息，制造「user 落库但本轮永远
    // 没有 assistant 回复」的悬挂视图（等价于后端崩溃经代理后刷新所见）。
    const messagesPattern = '**/api/conversations/*/messages*';
    await page.route(messagesPattern, async (route) => {
      const response = await route.fetch();
      const payload = (await response.json()) as { items?: Array<{ role: string }> };
      const filtered = {
        ...payload,
        items: (payload.items ?? []).filter((item) => item.role === 'user'),
      };
      await route.fulfill({ response, json: filtered });
    });

    // 刷新后：user 消息悬挂 → 其气泡上出现「重新生成」入口。
    await page.reload();
    const regenerate = page
      .locator('.chat-message--user')
      .getByRole('button', { name: '重新生成' });
    await expect(regenerate).toBeVisible({ timeout: 20_000 });

    // 点击后以该轮 user 原文重新发起一轮；恢复真实消息列表，断言新一轮
    // 跑通且悬挂入口消失（末条不再是悬挂 user）。
    await page.unroute(messagesPattern);
    await regenerate.click();
    await expect(page.getByText(DEFAULT_FINAL).first()).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole('button', { name: '重新生成' })).toHaveCount(0);
  });
});

test.describe('走查二：输入长度可见性（showCount）', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('消息输入框：粘贴 20000 字符被截断时显示 16000 计数', async ({ page }) => {
    await page.goto('/chat');
    const box = page.getByLabel('消息输入框');
    await expect(box).toBeVisible();

    await box.fill('长'.repeat(20000));
    // 修复前：静默截断到 16000，页面上无任何计数/提示（走查 D1）。
    await expect(page.getByText(/16000\s*\/\s*16000/).first()).toBeVisible();
    expect((await box.inputValue()).length).toBe(16000);
  });

  test('会话重命名弹窗：标题输入框显示字符计数', async ({ page }) => {
    await page.goto('/chat');
    const item = page.locator('.chat-conv-item').first();
    await expect(item).toBeVisible({ timeout: 20_000 });
    await item.hover();
    await item
      .getByRole('button')
      .filter({ has: page.locator('.anticon-more') })
      .click();
    await page.getByRole('menuitem', { name: '重命名' }).click();

    const dialog = page.getByRole('dialog', { name: '重命名会话' });
    const input = dialog.getByRole('textbox');
    // 修复前：300 字符静默变 200（走查 D3），无计数提示。
    await input.fill('题'.repeat(300));
    await expect(dialog.getByText(/200\s*\/\s*200/)).toBeVisible();
    expect((await input.inputValue()).length).toBe(200);
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
  });
});

test.describe('走查二：端口越界显式校验', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('服务器端口输 0：不再被 clamp 成 1，报「端口范围为 1-65535」且不建档', async ({ page }) => {
    await page.goto('/servers');
    await page.getByRole('button', { name: '新增服务器' }).click();

    const dialog = page.getByRole('dialog', { name: '新增服务器' });
    await dialog.getByLabel('名称').fill('wz-port-zero');
    await dialog.getByLabel('主机地址').fill('192.0.2.1');
    const port = dialog.getByLabel('端口');
    await port.fill('0');
    await port.blur();

    // 修复前：antd InputNumber 的 min=1 在 blur 时静默 clamp 成 1（走查 C1
    // 实测 blur 后值=1，且 API 落库 :1）；修复后保持 0 并由 Form 校验报错。
    await expect(port).toHaveValue('0');
    await expect(dialog.getByText('端口范围为 1-65535')).toBeVisible({ timeout: 10_000 });

    // 提交被拦：弹窗仍在（未发出创建请求）。
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText('端口范围为 1-65535')).toBeVisible();

    // API 侧确认没有建档 0/1 端口的记录。
    const response = await page.request.get('/api/servers');
    expect(response.ok()).toBeTruthy();
    const servers = ((await response.json()) as { items: Array<{ name: string; port: number }> })
      .items;
    const created = servers.filter((server) => server.name === 'wz-port-zero');
    expect(created).toHaveLength(0);
  });

  test('数据库端口输 0：同型显式报错，不静默 clamp', async ({ page }) => {
    await page.goto('/databases');
    await page.getByRole('button', { name: '新增数据库' }).click();

    const dialog = page.getByRole('dialog', { name: '新增数据库' });
    await dialog.getByLabel('名称').fill('wz-db-port-zero');
    await dialog.getByLabel('主机地址').fill('192.0.2.2');
    await dialog.getByLabel('数据库名').fill('app');
    await dialog.getByLabel('用户名').fill('readonly');
    const port = dialog.getByLabel('端口');
    await port.fill('0');
    await port.blur();

    await expect(port).toHaveValue('0');
    await expect(dialog.getByText('端口范围为 1-65535')).toBeVisible({ timeout: 10_000 });

    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText('端口范围为 1-65535')).toBeVisible();
  });
});
