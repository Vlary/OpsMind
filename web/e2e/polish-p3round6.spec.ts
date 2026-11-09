import { expect, test, type Page } from '@playwright/test';

import { AUTH_STATE_FILE } from './helpers';

// 三期第 6 轮冲刺激验收走查（p3 round 6）：真实链路逐页操作，重点覆盖
// 三期功能面——服务器表单 device_kind/品牌/access_proto 联动与 Telnet
// 明文确认、网络设备连接测试错误反馈、mongodb 表单联动与 JSON 查询抽屉
// （校验错误展示）、终端页网络设备可选性、RDP 下拉隐藏网络设备、审计
// 三期事件标签、对话上下文含设备标识。全程收集 console 错误。

const consoleErrors: string[] = [];

test.beforeEach(({ page }) => {
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      consoleErrors.push(`[console.error] ${page.url()} :: ${msg.text()}`);
    }
  });
  page.on('pageerror', (err) => {
    consoleErrors.push(`[pageerror] ${page.url()} :: ${err.message}`);
  });
});

/** 当前可见的下拉选项（antd：非 hidden 的 dropdown 内的 option）。 */
function dropdownOption(page: Page, text: string) {
  return page
    .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option')
    .filter({ hasText: text });
}

function radio(scope: Page | { locator: ReturnType<Page['locator']> }, text: string) {
  return scope.locator('.ant-radio-button-wrapper').filter({ hasText: text }).first();
}

const SUFFIX = Date.now().toString().slice(-6);
const TELNET_NAME = `e2e-p3r6-telnet-${SUFFIX}`;
const NETSSH_NAME = `e2e-p3r6-netssh-${SUFFIX}`;
const MONGO_NAME = `e2e-p3r6-mongo-${SUFFIX}`;

test.describe.serial('三期第 6 轮走查', () => {
  test.use({ storageState: AUTH_STATE_FILE });

  test('1. 服务器表单 device_kind/品牌/协议联动与 Telnet 明文确认', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/servers');
    await expect(page.getByRole('heading', { name: '服务器' })).toBeVisible();

    await page.getByRole('button', { name: /新增服务器/ }).click();
    // 注意：切到网络设备后 modal 标题变为「新增网络设备」，不能用标题过滤。
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();

    // 普通服务器形态：无品牌/接入协议字段。
    await expect(dialog.getByText('设备品牌')).toHaveCount(0);
    await expect(dialog.getByText('接入协议')).toHaveCount(0);

    // 切网络设备：品牌/接入协议出现，端口重置 22。
    await radio(dialog, '网络设备').click();
    await expect(dialog.getByText('设备品牌')).toBeVisible();
    await expect(dialog.getByText('接入协议')).toBeVisible();
    await expect(dialog.locator('#port')).toHaveValue('22');
    // 认证方式固定密码且禁用。
    await expect(
      dialog.locator('.ant-radio-button-wrapper').filter({ hasText: '密码认证' }),
    ).toBeDisabled();

    // 品牌选华为；协议切 Telnet：端口联动 23、明文警示出现、按钮文案变化。
    await dialog.locator('#netdev_brand').click();
    await dropdownOption(page, '华为').click();
    await radio(dialog, 'Telnet').click();
    await expect(dialog.locator('#port')).toHaveValue('23');
    await expect(dialog.getByText('Telnet 为明文传输，登录与特权密码可被截获')).toBeVisible();
    await expect(dialog.getByRole('button', { name: /确认明文风险并保存/ })).toBeVisible();

    await dialog.getByLabel('名称').fill(TELNET_NAME);
    await dialog.getByLabel('主机地址').fill('127.0.0.1');
    await dialog.getByLabel('登录用户').fill('admin');
    await dialog.getByLabel('密码', { exact: true }).fill('e2e-telnet-pass');

    // 保存触发不可跳过的二次确认。
    await dialog.getByRole('button', { name: /确认明文风险并保存/ }).click();
    const confirm = page.locator('.ant-modal').filter({ hasText: '确认使用 Telnet 明文传输？' });
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: /确认明文风险并保存/ }).click();
    await expect(page.getByText('服务器已创建')).toBeVisible({ timeout: 20_000 });
    // 二次确认 Modal.confirm 与表单 Modal 的离场动画并行，排除确认弹窗。
    await expect(page.locator('.ant-modal:not(.ant-modal-confirm)')).toBeHidden({
      timeout: 20_000,
    });

    // 列表出现网络设备行（搜索过滤避开分页）。
    await page
      .getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签 / 品牌')
      .fill(TELNET_NAME);
    const row = page.locator('.ant-table-row').filter({ hasText: TELNET_NAME }).first();
    await expect(row).toBeVisible();
    await expect(row).toContainText('网络设备');
    await expect(row).toContainText('华为 · TELNET');
  });

  test('2. 网络设备连接测试错误反馈（中文分类、剥英文前缀）', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/servers');
    await page
      .getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签 / 品牌')
      .fill(TELNET_NAME);
    const row = page.locator('.ant-table-row').filter({ hasText: TELNET_NAME }).first();
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: /测\s*试/ }).click();

    const modal = page.locator('.ant-modal').filter({ hasText: `测试连接 · ${TELNET_NAME}` });
    await expect(modal).toBeVisible({ timeout: 30_000 });
    await expect(modal).toContainText('失败');
    const body = await modal.innerText();
    console.log(`[走查] telnet 设备连接测试弹窗全文：\n${body}`);
    // 英文通道前缀不得直达用户。
    await expect(modal).not.toContainText('network device connection test failed');
    // p3 sprint 6：内部包名哨兵链（netdev: telnet dial / telnetx: dial）
    // 不得透传到弹窗，reason 只保留 dial tcp 级最终原因。
    await expect(modal).not.toContainText('netdev: telnet dial');
    await expect(modal).not.toContainText('telnetx: dial');
    await expect(modal).toContainText(/原因：dial tcp 127\.0\.0\.1:23/);
    // 中文分类短语（连接被拒绝 / 超时 / 不可达之一）。
    await expect(modal).toContainText(/无法连接|连接被拒绝|超时|不可达/);
    await modal.getByRole('button', { name: '关 闭' }).click();
  });

  test('3. SSH 网络设备创建 + 终端页可选性 + Telnet 常驻说明 + RDP 隐藏', async ({ page }) => {
    test.setTimeout(150_000);
    await page.goto('/servers');
    await page.getByRole('button', { name: /新增服务器/ }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    await radio(dialog, '网络设备').click();
    await dialog.getByLabel('名称').fill(NETSSH_NAME);
    await dialog.getByLabel('主机地址').fill('127.0.0.1');
    await dialog.locator('#port').fill('22222');
    await dialog.getByLabel('登录用户').fill('admin');
    await dialog.getByLabel('密码', { exact: true }).fill('e2e-net-pass');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(page.getByText('服务器已创建')).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('.ant-modal:not(.ant-modal-confirm)')).toBeHidden({
      timeout: 20_000,
    });
    // 列表确认创建落库（排除创建静默失败）。
    await page
      .getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签 / 品牌')
      .fill(NETSSH_NAME);
    await expect(
      page.locator('.ant-table-row').filter({ hasText: NETSSH_NAME }).first(),
    ).toBeVisible();

    // 终端页：SSH 网络设备可选（品牌/协议标识），Telnet 设备 disabled + 常驻说明。
    // API 探测：定位下拉缺失是后端返回还是前端渲染。
    const serversApi = await page.request.get('/api/servers');
    const serversJson = (await serversApi.json()) as { items?: { name: string }[] };
    console.log(
      `[走查] GET /api/servers 返回 ${serversJson.items?.length ?? -1} 台：${serversJson.items?.map((s) => s.name).join(', ')}`,
    );

    await page.goto('/term');
    await expect(page.getByRole('heading', { name: '终端' })).toBeVisible();
    await expect(page.getByText(/Telnet 接入的网络设备暂不支持终端/)).toBeVisible();
    // 每个 test 是独立浏览器 context（无 React Query 缓存），TermPage 的
    // serversQuery 需从零 fetch；并行负载下 fetch 未完成时下拉恒为空，
    // 20s 选项断言撞上慢 fetch 即假失败（全量跑两度实测；单文件串行恒过）。
    // 显式等待数据就绪：Select 默认选中第一台 SSH 可用设备（seed 的
    // e2e-fake-host），其选中态出现即 serversQuery 已渲染。
    await expect(page.getByText('e2e-fake-host（127.0.0.1:1）')).toBeVisible({
      timeout: 15_000,
    });
    await page.getByLabel('选择服务器').click();
    // 并行负载下 click 可能落在下拉动画/查询未就绪上：先确认 dropdown
    // 真正展开（否则后续 :not(.ant-select-dropdown-hidden) 过滤器永远匹配
    // 不到，20s 超时表现为「选项不存在」——全量跑实测抖动，功能本身
    // 正常（单文件串行 9 passed）。
    await expect(page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)')).toBeVisible(
      { timeout: 10_000 },
    );
    const sshOption = dropdownOption(page, `${NETSSH_NAME}（127.0.0.1:22222 · Cisco · SSH）`);
    await expect(sshOption).toBeVisible();
    await expect(sshOption).not.toHaveClass(/ant-select-item-option-disabled/);
    const telnetOption = dropdownOption(page, `${TELNET_NAME}（127.0.0.1:23 · 华为 · TELNET）`);
    await expect(telnetOption).toBeVisible();
    await expect(telnetOption).toHaveClass(/ant-select-item-option-disabled/);
    await sshOption.click();
    // 选中 SSH 网络设备后出现能力说明（与常驻 Telnet 说明文案近似，
    // 用选中态独有的尾句锚定）。
    await expect(page.getByText(/Telnet 接入的设备不支持终端。/)).toBeVisible();
    await page.keyboard.press('Escape');

    // RDP 页下拉不含网络设备。
    await page.goto('/rdp');
    // 六期打磨 sprint 1：页头跟随导航统一为「桌面」（此前「RDP 桌面」）。
    await expect(page.getByRole('heading', { name: '桌面', exact: true })).toBeVisible();
    const rdpCombo = page.getByRole('combobox').first();
    await rdpCombo.click();
    const allOptions = await page
      .locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option')
      .allInnerTexts();
    console.log(`[走查] RDP 服务器下拉全部选项：${JSON.stringify(allOptions)}`);
    for (const option of allOptions) {
      expect(
        option.includes(NETSSH_NAME) || option.includes(TELNET_NAME),
        `RDP 下拉不应出现网络设备，实际出现：${option}`,
      ).toBe(false);
    }
    await page.keyboard.press('Escape');
  });

  test('4. mongodb 表单联动与本地成对校验', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/databases');
    await expect(page.getByRole('heading', { name: '数据库' })).toBeVisible();
    await page.getByRole('button', { name: /新增数据库/ }).click();
    const dialog = page.locator('.ant-modal').filter({ hasText: '新增数据库' });
    await expect(dialog).toBeVisible();

    await dialog.locator('#engine').click();
    await dropdownOption(page, 'MongoDB').click();
    // 联动：默认端口 27017 + Mongo 专属字段 + 只读白名单 Alert。
    await expect(dialog.locator('#port')).toHaveValue('27017');
    await expect(dialog.getByText('认证库（AuthSource）')).toBeVisible();
    await expect(dialog.getByText('MongoDB：查询走 JSON 命令文档白名单，只读')).toBeVisible();

    await dialog.getByLabel('名称').fill(MONGO_NAME);
    await dialog.getByLabel('主机地址').fill('127.0.0.1');
    await dialog.getByLabel('数据库名').fill('app');
    // 只填用户名不填密码 → 本地成对校验拦截。
    await dialog.getByLabel('用户名（无认证可留空）').fill('readonly');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(
      dialog.getByText('填写用户名时需同时填写密码；无认证部署则两者都留空'),
    ).toBeVisible({ timeout: 10_000 });
    // 清空用户名（无认证部署）后保存成功。
    await dialog.getByLabel('用户名（无认证可留空）').fill('');
    await dialog.getByRole('button', { name: /保\s*存/ }).click();
    await expect(dialog).toBeHidden({ timeout: 20_000 });

    await page.getByPlaceholder(/搜索/).fill(MONGO_NAME);
    await expect(
      page.locator('.ant-table-row').filter({ hasText: MONGO_NAME }).first(),
    ).toBeVisible();
  });

  test('5. JSON 查询抽屉：本地校验错误展示 + 服务端拒绝 + 连接失败口径', async ({ page }) => {
    test.setTimeout(150_000);
    await page.goto('/databases');
    await page.getByPlaceholder(/搜索/).fill(MONGO_NAME);
    const row = page.locator('.ant-table-row').filter({ hasText: MONGO_NAME }).first();
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: /查\s*询/ }).click();

    const drawer = page.locator('.ant-drawer').filter({ hasText: `JSON 查询 · ${MONGO_NAME}` });
    await expect(drawer).toBeVisible();
    // 初始命令文档 + 白名单说明 + 连接目标。
    await expect(drawer.locator('.opsmind-sql-editor')).toHaveValue(/"find"/);
    await expect(drawer.getByText('只读 JSON 命令控制台（MongoDB）')).toBeVisible();
    await expect(
      drawer.getByText(/连接：127\.0\.0\.1:27017\/app（AuthSource admin）/),
    ).toBeVisible();

    const editor = drawer.locator('.opsmind-sql-editor');
    const run = drawer.getByRole('button', { name: /执行查询/ });

    // (a) 非法 JSON → 本地校验错误。
    await editor.fill('{"find"');
    await run.click();
    await expect(drawer.getByText('命令文档校验未通过')).toBeVisible();
    await expect(drawer.getByText(/不是合法的 JSON 命令文档/)).toBeVisible();

    // (b) 数组字面量 → 对象形状错误。
    await editor.fill('[1,2]');
    await run.click();
    await expect(drawer.getByText(/命令文档必须是 JSON 对象/)).toBeVisible();

    // (c) 双命令键。
    await editor.fill('{"find":"users","count":"users"}');
    await run.click();
    await expect(drawer.getByText(/只允许一个顶层命令键/)).toBeVisible();

    // (d) 非白名单命令键。
    await editor.fill('{"insert":"users","documents":[]}');
    await run.click();
    await expect(drawer.getByText(/顶层命令键必须是/)).toBeVisible();

    // (e) 合法形状但参数超白名单（limit 5000）→ 服务端守卫拒绝 → 只读警示。
    await editor.fill('{"find":"users","filter":{},"limit":5000}');
    await run.click();
    await expect(drawer.getByText(/仅支持只读查询|参数超/).first()).toBeVisible({
      timeout: 30_000,
    });
    const drawerText5 = await drawer.innerText();
    console.log(`[走查] mongo 超白名单被拒弹层全文：\n${drawerText5}`);

    // (f) 合法且参数达标 → 连接不可达 → 去 SQL 化的失败口径。
    await editor.fill('{"find":"users","filter":{},"limit":10}');
    await run.click();
    await expect(drawer.getByText(/查询执行失败|查询失败|无法连接/).first()).toBeVisible({
      timeout: 45_000,
    });
    // p3 sprint 6：mongo ping 失败不得透传 "dbproxy: mongo ping: " 包名
    // 哨兵与驱动 topology dump；reason 截断到 server selection 级原因。
    await expect(drawer).not.toContainText('dbproxy: mongo ping');
    await expect(drawer).not.toContainText('current topology');
    await expect(drawer).toContainText('server selection error: server selection timeout');
    const drawerText6 = await drawer.innerText();
    console.log(`[走查] mongo 连接失败弹层全文：\n${drawerText6}`);
    await expect(drawer).not.toContainText('SQL 执行失败');
    await drawer.getByRole('button', { name: '关闭', exact: true }).click();
  });

  test('6. 审计页三期事件：查询被拒绝可筛选展示', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/audit');
    await expect(page.getByRole('heading', { name: '审计' })).toBeVisible();

    // 动作筛选下拉按 label 搜索 + Enter 选中（对齐 polish-sprint10 写法）。
    const actionSelect = page.locator('.opsmind-admin-toolbar .ant-select').first();
    await actionSelect.click();
    await page.keyboard.type('数据库查询被拒绝');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect(actionSelect).toContainText('数据库查询被拒绝');
    // 列表出现 test 5 种下的被拒记录。
    await expect(
      page.locator('.ant-table-row').filter({ hasText: '数据库查询被拒绝' }).first(),
    ).toBeVisible({ timeout: 20_000 });
    const firstRow = await page
      .locator('.ant-table-row')
      .filter({ hasText: '数据库查询被拒绝' })
      .first()
      .innerText();
    console.log(`[走查] 审计 database.query_rejected 行：\n${firstRow}`);
  });

  test('7. 对话上下文下拉含设备标识（品牌/协议）', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/chat');
    // antd multiple Select 的 placeholder 文本节点不可点，点其容器。
    const serverContext = page.locator('.ant-select', { hasText: '服务器上下文' }).first();
    await expect(serverContext).toBeVisible();
    await serverContext.click();
    const telnetOpt = dropdownOption(page, `${TELNET_NAME}（127.0.0.1:23 · 华为 · TELNET）`);
    await expect(telnetOpt).toBeVisible();
    const sshOpt = dropdownOption(page, `${NETSSH_NAME}（127.0.0.1:22222 · Cisco · SSH）`);
    await expect(sshOpt).toBeVisible();
    await page.keyboard.press('Escape');
  });

  test('8. 编辑 Telnet 网络设备：回填正确、无主机密钥提示', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/servers');
    await page
      .getByPlaceholder('搜索名称 / 地址 / 端口 / 登录用户 / 标签 / 品牌')
      .fill(TELNET_NAME);
    const row = page.locator('.ant-table-row').filter({ hasText: TELNET_NAME }).first();
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: /编\s*辑/ }).click();

    const dialog = page.locator('.ant-modal').filter({ hasText: `编辑网络设备 · ${TELNET_NAME}` });
    await expect(dialog).toBeVisible();
    // 回填：品牌华为（antd v6 选中值类名不定，断言品牌 Select 容器文本）、
    // 协议 Telnet、端口 23。
    const brandSelect = dialog
      .locator('.ant-form-item')
      .filter({ hasText: '设备品牌' })
      .locator('.ant-select');
    await expect(brandSelect).toContainText('华为');
    await expect(radio(dialog, 'Telnet')).toHaveClass(/ant-radio-button-wrapper-checked/);
    await expect(dialog.locator('#port')).toHaveValue('23');
    // Telnet 通道无主机密钥语义：不渲染清指纹提示。
    await expect(dialog.getByText('修改地址或端口会清除已信任的主机密钥')).toHaveCount(0);
    // 切回服务器形态再取消（形态切换不炸）。
    await radio(dialog, '服务器').click();
    await expect(dialog.getByText('操作系统')).toBeVisible();
    await dialog.getByRole('button', { name: /取\s*消/ }).click();
    await expect(dialog).toBeHidden();
  });

  test('汇总 console 错误', () => {
    console.log(
      `[走查] 全程收集到 ${consoleErrors.length} 条 console 错误：\n${consoleErrors.join('\n') || '（无）'}`,
    );
  });
});
