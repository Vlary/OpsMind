import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

import { expect, test, type Page } from '@playwright/test';

import {
  AUTH_STATE_FILE,
  DEFAULT_FINAL,
  FAKE_LLM_URL,
  RISKY_FINAL,
  SCENARIO_ERROR_ALT,
  SCENARIO_RISKY,
  SCENARIO_SLOW,
  conversationIdOf,
  sendMessage,
} from './helpers';

// 前端覆盖扩充：审批审计断言、停止/失败轮恢复的补充路径、上下文选择的
// 仅数据库与清除分支、供应商「拉取模型」与新增成功路径、SQL 抽屉快捷键
// 与多语句/EXPLAIN、审计资源类型与 UUID 筛选及翻页重置、250 条长会话分页。
// 与既有 spec 互补，全部用例幂等（每轮 e2e 数据库整体重建）。

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const ORIGIN_HEADERS = { Origin: 'http://localhost:4173' };

/** 直连 e2e postgres 批量插入消息（长会话分页用例）。 */
async function insertMessages(conversationId: string, count: number) {
  const sql = [
    `INSERT INTO messages (conversation_id, turn_id, role, content, status, created_at)`,
    `SELECT '${conversationId}'::uuid, gen_random_uuid(),`,
    `  CASE WHEN g % 2 = 0 THEN 'user' ELSE 'assistant' END,`,
    `  '长会话分页补充 #' || g, 'complete',`,
    `  now() - interval '1 second' * (${count} + 10 - g)`,
    `FROM generate_series(1, ${count}) AS g;`,
  ].join(' ');
  await execFileAsync(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      'db',
      'psql',
      '-U',
      'aiops',
      '-d',
      'aiops_e2e',
      '-v',
      'ON_ERROR_STOP=1',
      '-c',
      sql,
    ],
    { cwd: repoRoot },
  );
}

/** 审计 API：按会话过滤取动作集合。 */
async function auditActionsOf(page: Page, conversationId: string): Promise<string[]> {
  const response = await page.request.get('/api/audit', {
    params: { conversation_id: conversationId, limit: 100 },
  });
  expect(response.ok()).toBeTruthy();
  const payload = (await response.json()) as { items: Array<{ action: string }> };
  return payload.items.map((item) => item.action);
}

/** 建一个指向 e2e postgres 的数据库资产（唯一名，天然幂等）。 */
async function createPgDatabase(page: Page, name: string): Promise<void> {
  const created = await page.request.post('/api/databases', {
    headers: ORIGIN_HEADERS,
    data: {
      name,
      engine: 'postgres',
      host: '127.0.0.1',
      port: 65432,
      database_name: 'aiops_e2e',
      username: 'aiops',
      password: 'aiops',
    },
  });
  expect(created.status()).toBe(201);
}

/** 数据库列表分页 10 条/页：按名称搜索聚焦目标行。 */
async function focusDatabaseRow(page: Page, name: string) {
  await page.getByPlaceholder('搜索名称 / 主机 / 数据库名 / 用户名').fill(name);
  const row = page.locator('tr', { hasText: name }).first();
  await expect(row).toBeVisible({ timeout: 20_000 });
  return row;
}

/** 打开 SQL 抽屉并返回抽屉 locator。 */
async function openSqlConsole(page: Page, name: string) {
  await page.goto('/databases');
  const row = await focusDatabaseRow(page, name);
  await row.getByRole('button', { name: /查询/ }).click();
  const drawer = page.getByRole('dialog', { name: `SQL 查询 · ${name}` });
  await expect(drawer).toBeVisible();
  return drawer;
}

test.describe('审批链路补充：审计断言', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('审批批准：审计记录 approval.approved；SSH 失败不写 command.executed', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: '批准' }).click();
    await expect(approvalCard.getByText('审批已批准', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    // 模型收尾（第 3 次假 LLM 调用）。
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });

    // 审计：审批创建与批准均有记录。
    const actions = await auditActionsOf(page, cid);
    expect(actions).toContain('approval.created');
    expect(actions).toContain('approval.approved');
    // 批准后 ssh_exec 对假服务器连接失败：runner 在执行失败路径提前返回，
    // 不会写 command.executed（只有真实执行完成才落 executed 审计）。
    expect(actions).not.toContain('command.executed');
    expect(actions).not.toContain('command.denied');
  });

  test('审批拒绝：审计记录 approval.denied 与 command.denied', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_RISKY);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    const approvalCard = page.locator('.chat-approval', { hasText: '命令审批' }).first();
    await expect(approvalCard).toBeVisible({ timeout: 30_000 });
    await approvalCard.getByRole('button', { name: '拒绝' }).click();
    await expect(approvalCard.getByText('审批已拒绝', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText(RISKY_FINAL)).toBeVisible({ timeout: 30_000 });

    const actions = await auditActionsOf(page, cid);
    expect(actions).toContain('approval.created');
    expect(actions).toContain('approval.denied');
    expect(actions).toContain('command.denied');
    expect(actions).not.toContain('approval.approved');
    expect(actions).not.toContain('command.executed');
  });
});

test.describe('停止与失败轮恢复补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('停止慢速流后立即发送新消息：新一轮不受影响正常完成', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_SLOW);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    // 慢速流进行中：等正文上屏再停止。
    await expect(page.locator('.chat-message--assistant .opsmind-markdown').first()).toBeVisible({
      timeout: 30_000,
    });
    await page.locator('.chat-composer').getByRole('button', { name: '停止' }).click();
    await expect(page.getByText('已停止').first()).toBeVisible({ timeout: 20_000 });

    // 停止后按钮回到「发送」：立即发起新一轮（新内容，非重新生成）。
    const followUpMarker = '停止后立即追问的定位标记';
    const box = page.getByLabel('消息输入框');
    await box.fill(followUpMarker);
    await page.getByRole('button', { name: '发送' }).click();

    // 新一轮走默认场景完整回答（发送按钮回到停止再回到发送 = 流结束）。
    await expect(page.getByText(DEFAULT_FINAL).first()).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole('button', { name: '发送' })).toBeVisible({ timeout: 20_000 });
    // 新一轮 user 气泡与上一轮已停止消息并存。
    await expect(
      page.locator('.chat-message--user', { hasText: followUpMarker }).first(),
    ).toBeVisible();
    await expect(page.getByText('已停止').first()).toBeVisible();
  });

  test('上游失败轮：不刷新直接「忽略」错误横幅，落库后出现重新生成入口', async ({ page }) => {
    await page.goto('/chat');
    await sendMessage(page, SCENARIO_ERROR_ALT);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });
    const cid = conversationIdOf(page);

    // 假 LLM 前 3 次调用 500：本轮流式失败，出现错误横幅与「忽略」按钮。
    await expect(page.getByText('生成失败').first()).toBeVisible({ timeout: 30_000 });
    const ignoreButton = page.locator('.chat-stream-error').getByRole('button', { name: '忽略' });
    await expect(ignoreButton).toBeVisible();
    await ignoreButton.click();

    // 「忽略」后错误横幅（含忽略按钮）消失，页面不刷新。
    await expect(
      page.locator('.chat-stream-error').getByRole('button', { name: '忽略' }),
    ).toHaveCount(0);

    // 后端落库 error 消息后（invalidate refetch），历史失败消息出现重新生成。
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`/api/conversations/${cid}/messages`, {
            params: { limit: 10 },
          });
          const payload = (await response.json()) as {
            items: Array<{ role: string; status: string }>;
          };
          const last = payload.items[payload.items.length - 1];
          return last?.role === 'assistant' && last.status === 'error' ? 'error' : '';
        },
        { timeout: 30_000, intervals: [2_000] },
      )
      .toBe('error');
    await expect(page.getByRole('button', { name: '重新生成' }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});

test.describe('服务器/数据库上下文选择补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('仅选择数据库：请求前缀只含 databases 行，不含 servers 行', async ({ page }) => {
    const created = await page.request.post('/api/databases', {
      headers: ORIGIN_HEADERS,
      data: {
        name: 'e2e-ctx-only-db',
        engine: 'postgres',
        host: '127.0.0.1',
        port: 5432,
        database_name: 'aiops',
        username: 'aiops',
        password: 'aiops',
      },
    });
    expect(created.status()).toBe(201);

    await page.goto('/chat');
    // 顶栏第 2 个多选是数据库（第 1 个是服务器，保持不选）。
    const databaseSelect = page.locator('.chat-topbar .ant-select-multiple').nth(1);
    await databaseSelect.click();
    await expect(
      page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)').last(),
    ).toBeVisible();
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    const dbTag = await databaseSelect.locator('.ant-select-selection-item').first().textContent();
    expect(dbTag).toBeTruthy();

    const marker = '仅数据库上下文验证标记';
    await sendMessage(page, `查数据 ${marker}`);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    let captured = '';
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`${FAKE_LLM_URL}/e2e/last-user`);
          const payload = (await response.json()) as { history?: string[] };
          captured = payload.history?.find((item) => item.includes(marker)) ?? '';
          return captured !== '';
        },
        { timeout: 30_000, intervals: [1_000] },
      )
      .toBe(true);
    expect(captured.startsWith('<!--opsmind-context')).toBe(true);
    expect(captured).toContain(`databases: ${dbTag?.trim()} (id=`);
    expect(captured).not.toContain('servers:');
    // 正文跟在前缀后。
    expect(captured.endsWith(`\n查数据 ${marker}`)).toBe(true);
    // 页面不出现上下文注释。
    await expect(page.getByText('opsmind-context')).toHaveCount(0);
  });

  test('清除全部上下文后发送：请求正文不带任何前缀', async ({ page }) => {
    await page.goto('/chat');
    // 先选一台服务器（Enter 选中高亮首项）。
    const serverSelect = page.locator('.chat-topbar .ant-select-multiple').first();
    await serverSelect.click();
    await expect(
      page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)').last(),
    ).toBeVisible();
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(serverSelect).toContainText('e2e-fake-host');

    // allowClear：hover 出现清除图标，点击后选择清空。
    await serverSelect.hover();
    await serverSelect.locator('.ant-select-clear').click();
    await expect(serverSelect.locator('.ant-select-selection-item')).toHaveCount(0);
    await expect(serverSelect).toContainText('服务器上下文');

    const marker = '清除上下文验证标记';
    await sendMessage(page, `裸消息 ${marker}`);
    await expect(page).toHaveURL(/\/c\/[0-9a-f-]+/, { timeout: 20_000 });

    let captured = '';
    await expect
      .poll(
        async () => {
          const response = await page.request.get(`${FAKE_LLM_URL}/e2e/last-user`);
          const payload = (await response.json()) as { history?: string[] };
          captured = payload.history?.find((item) => item.includes(marker)) ?? '';
          return captured !== '';
        },
        { timeout: 30_000, intervals: [1_000] },
      )
      .toBe(true);
    // 无任何选择时 buildContextContent 原样透传正文。
    expect(captured).toBe(`裸消息 ${marker}`);
    expect(captured).not.toContain('<!--');
  });
});

test.describe('模型供应商管理补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('「拉取模型」：成功提示拉到的模型数量', async ({ page }) => {
    await page.goto('/providers');
    const row = page.locator('tr', { hasText: 'e2e-fake-llm' }).first();
    await expect(row).toBeVisible({ timeout: 20_000 });
    await row.getByRole('button', { name: '拉取模型' }).click();

    // ProvidersPage 刷新成功提示（fake-llm /v1/models 返回 1 个模型）。
    await expect(
      page.locator('.ant-message').getByText(/已刷新「e2e-fake-llm」的模型列表：1 个/),
    ).toBeVisible({ timeout: 20_000 });
  });

  test('新增供应商成功路径：预设默认填充可覆盖，保存后表格出现新行', async ({ page }) => {
    const uniqueName = 'e2e-provider-create-ok';
    await page.goto('/providers');
    await page.getByRole('button', { name: '新增供应商' }).click();
    const dialog = page.getByRole('dialog', { name: '新增供应商' });
    await expect(dialog).toBeVisible();

    // 预设默认填充名称/Base URL/默认模型：只覆盖名称、Base URL 并补 API Key。
    await dialog.getByRole('textbox', { name: '名称' }).fill(uniqueName);
    await dialog.getByRole('textbox', { name: 'Base URL' }).fill(`${FAKE_LLM_URL}/v1`);
    await dialog.getByRole('textbox', { name: 'API Key' }).fill('e2e-create-key');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeHidden({ timeout: 20_000 });

    try {
      const row = page.locator('tr', { hasText: uniqueName }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      // 覆盖后的 Base URL 出现在表格行（默认模型随预设填充，不硬编码其名）。
      await expect(row).toContainText(`${FAKE_LLM_URL}/v1`.replace('http://', ''));
    } finally {
      const list = await page.request.get('/api/providers');
      const payload = (await list.json()) as { items: Array<{ id: string; name: string }> };
      const created = payload.items.find((item) => item.name === uniqueName);
      if (created) {
        const removed = await page.request.delete(`/api/providers/${created.id}`, {
          headers: ORIGIN_HEADERS,
        });
        expect(removed.status()).toBeLessThan(300);
      }
    }
  });
});

test.describe('数据库 SQL 查询抽屉补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('Ctrl+Enter 执行查询；「清空」恢复初始提示', async ({ page }) => {
    await createPgDatabase(page, 'e2e-sql-shortcut');
    const drawer = await openSqlConsole(page, 'e2e-sql-shortcut');

    const editor = drawer.locator('.opsmind-sql-editor');
    await editor.fill('SELECT count(*) AS n FROM conversations;');
    await editor.press('Control+Enter');
    await expect(drawer.getByText('返回 1 行')).toBeVisible({ timeout: 20_000 });
    await expect(drawer.locator('th', { hasText: 'n' })).toBeVisible();

    // 清空：编辑器与结果区一起复位。
    await drawer.getByRole('button', { name: '清空' }).click();
    await expect(editor).toHaveValue('');
    await expect(drawer.getByText('返回 1 行')).toHaveCount(0);
    await expect(drawer.getByText('执行查询后在此显示结果。')).toBeVisible();
  });

  test('多语句被只读守卫拦截：SQL_NOT_READONLY 提示且无结果', async ({ page }) => {
    await createPgDatabase(page, 'e2e-sql-multi');
    const drawer = await openSqlConsole(page, 'e2e-sql-multi');

    await drawer.locator('.opsmind-sql-editor').fill('SELECT 1; SELECT 2;');
    await drawer.getByRole('button', { name: '执行查询' }).click();

    await expect(drawer.getByText(/仅支持只读查询，写操作不会执行/).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(drawer.getByText(/返回 \d+ 行/)).toHaveCount(0);
  });

  test('EXPLAIN 只读语句可执行并返回执行计划', async ({ page }) => {
    await createPgDatabase(page, 'e2e-sql-explain');
    const drawer = await openSqlConsole(page, 'e2e-sql-explain');

    await drawer.locator('.opsmind-sql-editor').fill('EXPLAIN SELECT count(*) FROM conversations;');
    await drawer.getByRole('button', { name: '执行查询' }).click();

    // postgres 返回 QUERY PLAN 列与至少 1 行计划。
    await expect(drawer.getByText(/返回 \d+ 行/)).toBeVisible({ timeout: 20_000 });
    await expect(drawer.locator('th', { hasText: 'QUERY PLAN' })).toBeVisible();
    await expect(drawer.getByText('Seq Scan', { exact: false }).first()).toBeVisible();
  });
});

test.describe('审计筛选补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('按目标类型筛选「会话」：仅显示会话记录，服务器记录消失', async ({ page }) => {
    // 自建一个会话保证筛选结果非空（conversation.create 审计）。
    const created = await page.request.post('/api/conversations', {
      headers: ORIGIN_HEADERS,
      data: { title: 'e2e审计类型筛选会话' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };

    try {
      await page.goto('/audit');
      const table = page.locator('.ant-table');
      await expect(table).toBeVisible();
      await expect(table.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

      // toolbar 第 2 个 Select 是目标类型：键盘输入过滤 + Enter 选中。
      const typeSelect = page.locator('.opsmind-admin-toolbar .ant-select').nth(1);
      await typeSelect.click();
      await page.keyboard.type('会话');
      await page.keyboard.press('Enter');
      await page.keyboard.press('Escape');
      await expect(typeSelect).toContainText('会话');

      // 目标列的精确文本「会话」出现（动作列是「新建会话」等，不会误命中）。
      const rows = table.locator('.ant-table-row');
      await expect(rows.first()).toBeVisible({ timeout: 20_000 });
      const count = await rows.count();
      expect(count).toBeGreaterThanOrEqual(1);
      for (let index = 0; index < count; index += 1) {
        await expect(rows.nth(index).getByText('会话', { exact: true }).first()).toBeVisible();
      }
      // 服务器类目标（精确文本「服务器」）不再出现。
      await expect(table.getByText('服务器', { exact: true })).toHaveCount(0);

      // API 侧对照：会话类型记录里能找到自建会话的创建动作
      //（conversation.create 的会话列为空，按 resource_id 精确过滤）。
      const response = await page.request.get('/api/audit', {
        params: { resource_type: 'conversation', resource_id: id, limit: 50 },
      });
      const payload = (await response.json()) as { items: Array<{ action: string }> };
      expect(payload.items.some((item) => item.action === 'conversation.create')).toBe(true);
    } finally {
      await page.request.delete(`/api/conversations/${id}`, { headers: ORIGIN_HEADERS });
    }
  });

  test('会话 UUID 筛选：非法值给出警告；合法值筛出该会话记录', async ({ page }) => {
    const created = await page.request.post('/api/conversations', {
      headers: ORIGIN_HEADERS,
      data: { title: 'e2e审计UUID筛选会话' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };

    try {
      // 发一条消息让会话产生带 conversation_id 的审计
      //（conversation.create 本身的会话列为空，按会话筛不出来）。
      await page.goto(`/c/${id}`);
      await sendMessage(page, '审计UUID筛选巡检');
      await expect(page.getByText(DEFAULT_FINAL).first()).toBeVisible({ timeout: 30_000 });
      // 等「发送」按钮回来（流真正 done）再离开：DEFAULT_FINAL 上屏后
      // done 帧可能还没到，立即导航会断流落成 chat.cancelled。
      await expect(page.getByRole('button', { name: '发送' })).toBeVisible({ timeout: 30_000 });
      // chat.finished 审计相对 done 帧可能略滞后：落库后再进审计页断言。
      await expect
        .poll(
          async () => {
            const response = await page.request.get('/api/audit', {
              params: { conversation_id: id, limit: 10 },
            });
            const payload = (await response.json()) as { items: Array<{ action: string }> };
            return payload.items.some((item) => item.action === 'chat.finished') ? 'ok' : '';
          },
          { timeout: 20_000, intervals: [1_000] },
        )
        .toBe('ok');

      await page.goto('/audit');
      await expect(page.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

      // 非法 UUID：提交被前端轻校验拦下并提示。
      // toolbar 第 2 个「筛选」按钮属于会话 ID 搜索框（第 1 个是目标 ID，
      // 三个 UUID 筛选框均接入 uuidFilter 轻校验，见 e2e/audit.spec.ts）。
      const search = page.getByPlaceholder('会话 ID（conversation UUID）');
      await search.fill('not-a-uuid');
      await page
        .locator('.opsmind-admin-toolbar')
        .getByRole('button', { name: /筛\s*选/ })
        .nth(1)
        .click();
      // 冲刺 5：精确断言补空格后的文案「会话 ID 必须是合法的 UUID」。
      await expect(page.locator('.ant-message').getByText('会话 ID 必须是合法的 UUID')).toBeVisible(
        { timeout: 10_000 },
      );

      // 合法 UUID：筛出该会话的 chat.started / chat.finished 记录
      //（按 id 倒序，首行是最后完成的 chat.finished）。
      await search.fill(id);
      await page
        .locator('.opsmind-admin-toolbar')
        .getByRole('button', { name: /筛\s*选/ })
        .nth(1)
        .click();
      const rows = page.locator('.ant-table-row');
      await expect(rows.first()).toBeVisible({ timeout: 20_000 });
      expect(await rows.count()).toBeGreaterThanOrEqual(1);
      await expect(rows.first()).toContainText('回答完成');
      await expect(rows.first()).toContainText('chat.finished');
    } finally {
      await page.request.delete(`/api/conversations/${id}`, { headers: ORIGIN_HEADERS });
    }
  });

  test('翻页后修改筛选：分页重置回第 1 页', async ({ page }) => {
    // 自种 25 对审计（数据库创建 + 删除），保证记录数超一页且与执行顺序无关。
    const seedIds: string[] = [];
    for (let i = 0; i < 25; i += 1) {
      const response = await page.request.post('/api/databases', {
        headers: ORIGIN_HEADERS,
        data: {
          name: `e2e-audit-reset-seed-${i}`,
          engine: 'sqlite',
          sqlite_path: '/nonexistent-reset-seed/seed.db',
        },
      });
      expect(response.status()).toBe(201);
      seedIds.push(((await response.json()) as { id: string }).id);
    }
    for (const id of seedIds) {
      const removed = await page.request.delete(`/api/databases/${id}`, {
        headers: ORIGIN_HEADERS,
      });
      expect(removed.status()).toBeLessThan(300);
    }

    try {
      await page.goto('/audit');
      const table = page.locator('.ant-table');
      await expect(table.locator('.ant-table-row').first()).toBeVisible({ timeout: 20_000 });

      // 每页缩到 20 条（toolbar 第 3 个 Select：ArrowUp 50→20 + Enter）。
      const pageSizeSelect = page.locator('.opsmind-admin-toolbar .ant-select').nth(2);
      await pageSizeSelect.click();
      await page.keyboard.press('ArrowUp');
      await page.keyboard.press('Enter');
      await expect(pageSizeSelect).toContainText('20 条 / 页');
      await expect(page.getByText('还有更多记录')).toBeVisible({ timeout: 20_000 });

      // 翻到第 2 页。
      await page.getByRole('button', { name: '下一页' }).click();
      await expect(page.getByText('第 2 页')).toBeVisible({ timeout: 20_000 });

      // 修改动作筛选（登录成功）：分页必须重置回第 1 页。
      const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
      await actionSelect.click();
      await page.keyboard.type('登录成功');
      await page.keyboard.press('Enter');
      await page.keyboard.press('Escape');
      await expect(actionSelect).toContainText('登录成功');
      await expect(page.getByText('第 1 页')).toBeVisible({ timeout: 20_000 });
      // 筛选生效：登录成功记录可见。
      await expect(table.locator('.ant-table-row').first()).toBeVisible();
    } finally {
      for (const id of seedIds) {
        await page.request
          .delete(`/api/databases/${id}`, { headers: ORIGIN_HEADERS })
          .catch(() => undefined);
      }
    }
  });
});

test.describe('长会话「加载更早消息」补充', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('250 条消息：两次「加载更早消息」取完历史，按钮消失', async ({ page }) => {
    const created = await page.request.post('/api/conversations', {
      headers: ORIGIN_HEADERS,
      data: { title: '长会话两轮分页验证' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    await insertMessages(id, 250);

    await page.goto(`/c/${id}`);
    // 首屏最新 100 条（#151..#250）。
    await expect(page.getByRole('button', { name: '加载更早消息' })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('长会话分页补充 #250', { exact: true })).toBeVisible();

    // 第一次加载（#51..#150）：还有一页，按钮仍在。
    await page.getByRole('button', { name: '加载更早消息' }).click();
    await expect(page.getByRole('button', { name: '加载更早消息' })).toBeVisible({
      timeout: 20_000,
    });

    // 第二次加载：#1..#50，全部取完按钮消失。
    await page.getByRole('button', { name: '加载更早消息' }).click();
    await expect(page.getByRole('button', { name: '加载更早消息' })).toHaveCount(0, {
      timeout: 20_000,
    });

    // 滚到顶部后最早消息可见。
    await page.locator('.chat-messages').evaluate((element) => {
      element.scrollTop = 0;
    });
    await expect(page.getByText('长会话分页补充 #1', { exact: true })).toBeVisible({
      timeout: 10_000,
    });
  });

  test('加载完历史后切走再回来：更早消息仍在缓存，无需再次加载', async ({ page }) => {
    const created = await page.request.post('/api/conversations', {
      headers: ORIGIN_HEADERS,
      data: { title: '长会话切回缓存验证' },
    });
    expect(created.status()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    await insertMessages(id, 150);

    await page.goto(`/c/${id}`);
    await expect(page.getByRole('button', { name: '加载更早消息' })).toBeVisible({
      timeout: 20_000,
    });
    // 加载一次取完（100 + 50）。
    await page.getByRole('button', { name: '加载更早消息' }).click();
    await expect(page.getByRole('button', { name: '加载更早消息' })).toHaveCount(0, {
      timeout: 20_000,
    });

    // 经侧栏 SPA 导航切到新会话页再返回：react-query 缓存保留已加载页，
    // 按钮、消息都在（page.goto 是整页加载会重置缓存，不能用于本用例）。
    const conversationItem = page
      .locator('.chat-conv-item', { hasText: '长会话切回缓存验证' })
      .first();
    await page.getByTitle('新建会话').click();
    await expect(page.getByText('开始新的运维对话')).toBeVisible();
    await conversationItem.click();
    await expect(page).toHaveURL(new RegExp(`/c/${id}`));
    await expect(page.getByText('长会话分页补充 #150', { exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByRole('button', { name: '加载更早消息' })).toHaveCount(0);
  });
});
