import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { test as base, expect, type Page, type Route } from '@playwright/test';

// e2e 统一 test/expect 出口。
//
// RBAC（00012）后审批职责分离由服务端强制：requested_by == decided_by 返回
// 403 APPROVAL_SELF_DECISION。页面会话（storageState）是 admin，同时也是
// 会话/批量任务的发起人，因此点击「批准」必须由独立的 reviewer 会话发起。
//
// 这里在**测试侧**对 decide 请求注入 reviewer Cookie：后端仍按真实
// reviewer 用户鉴权/复检/审计（生产鉴权没有任何放宽），只是把「第二个人
// 批准」这一动作自动化。其它请求（列表、聊天、审计…）仍用 admin 会话。
const here = path.dirname(fileURLToPath(import.meta.url));
const APPROVER_STATE_FILE = path.join(here, '.auth', 'reviewer.json');
const E2E_ORIGIN = 'http://localhost:4173';

let cachedApproverCookie: string | null = null;
let cachedApproverUserId: string | null = null;

async function approverUserId(): Promise<string> {
  if (cachedApproverUserId) {
    return cachedApproverUserId;
  }
  const response = await fetch(`${E2E_ORIGIN}/api/auth/me`, {
    headers: { Cookie: approverCookie(), Accept: 'application/json' },
  });
  if (!response.ok) {
    throw new Error(`reviewer /api/auth/me -> ${response.status}`);
  }
  const body = (await response.json()) as { user?: { id?: string } };
  const id = body.user?.id;
  if (!id) {
    throw new Error('reviewer /api/auth/me missing user.id');
  }
  cachedApproverUserId = id;
  return id;
}

function approverCookie(): string {
  if (cachedApproverCookie) {
    return cachedApproverCookie;
  }
  const raw = JSON.parse(readFileSync(APPROVER_STATE_FILE, 'utf8')) as {
    cookies: Array<{ name: string; value: string }>;
  };
  const cookie = raw.cookies.find((item) => item.name === 'opsmind_session');
  if (!cookie) {
    throw new Error(`reviewer session cookie missing in ${APPROVER_STATE_FILE}`);
  }
  cachedApproverCookie = `opsmind_session=${cookie.value}`;
  return cachedApproverCookie;
}

/**
 * createApprovedGrant 创建并批准一条 JIT 终端/RDP grant（00012）。
 *
 * 会话创建要求「他人批准的单次 grant」；页面会话是 admin，审批必须由
 * reviewer 发起（服务端职责分离）。这里用 reviewer Cookie 直接调用
 * decide，随后页面点「打开终端」时后端会消费该用户在该服务器上最新一条
 * 已批准 grant（请求省略 grant_id）。
 */
export async function createApprovedGrant(
  page: Page,
  serverId: string,
  kind: 'term' | 'rdp',
): Promise<string> {
  const grantPath = kind === 'rdp' ? '/api/rdp/grants' : '/api/term/grants';
  const created = await page.request.post(grantPath, {
    headers: { Origin: E2E_ORIGIN },
    data: { server_id: serverId },
  });
  expect(created.status(), await created.text()).toBe(202);
  const body = (await created.json()) as { approval: { id: string } };
  const decided = await page.request.post(`/api/approvals/${body.approval.id}/decide`, {
    headers: { Origin: E2E_ORIGIN, Cookie: approverCookie() },
    data: { decision: 'approve' },
  });
  expect(decided.status(), await decided.text()).toBe(200);
  return body.approval.id;
}

// installApproverIdentity 给指定 page 注册「审批者视角」路由；默认 page
// fixture 已自动安装，测试自行 context.newPage() 创建的窗口需显式调用
// （如双窗口用例）。
//
// 1) decide 请求：Chromium 不允许 route.continue 覆盖浏览器托管的 Cookie
//    头（实测仍以 admin 会话发出、返回 403 SELF_DECISION）。这里把 decide
//    请求转给 Node 侧 fetch 重放：只带 reviewer Cookie，响应原样回填给
//    页面。后端看到的就是「另一位 approver 用户」发起的真实决策。
// 2) /api/auth/me：审批卡按「approvals:decide + requested_by != 当前用户」
//    收敛按钮（docs：RBAC 走查 sprint 1）。页面会话是 admin（同时也是
//    发起人），若原样返回会让所有既有用例看到「不能自审」而不是按钮。
//    这里只把 user.id 换成 reviewer 的 id（username/role/permissions 仍为
//    admin），保持页面能力不变，仅让 UI 的职责分离判定等价于
//    「admin 在替另一位审批者操作」——与 1) 的 decide 身份一致。生产代码
//    没有任何测试开关，服务端仍按真实 reviewer 鉴权。
export async function installApproverIdentity(page: Page): Promise<void> {
  // /api/auth/me 是页面生命周期内随时可能被取消的后台请求（路由跳转、
  // 组件卸载、测试结束关闭页面）。请求被取消后 Playwright 会 dispose
  // 对应的 APIResponse，`response.body()` 抛 "Response has been disposed"；
  // 这是测试夹具的清理竞态，不是产品行为，不能让整条用例红。
  const abortQuietly = async (route: Route) => {
    try {
      await route.abort();
    } catch {
      // 路由已被浏览器取消/处理，无需再操作。
    }
  };
  await page.route('**/api/auth/me', async (route) => {
    let response;
    try {
      response = await route.fetch();
    } catch {
      await abortQuietly(route);
      return;
    }
    // 第 4 轮走查 low：不能用 route.fulfill({ response, json }) 回填已经
    // json() 读过的 APIResponse——Playwright 读取 body 后会把该响应标记为
    // disposed，fulfill 再次读 body 抛 "Response has been disposed"。这里
    // 先一次性读出 body，再以显式 status/headers/body 回填。
    const headers = { ...response.headers() };
    // 原始 Content-Length/Encoding 属于未改写 body；由 fulfill 按新 body 重算。
    delete headers['content-length'];
    delete headers['content-encoding'];
    let raw: Buffer;
    try {
      raw = await response.body();
    } catch {
      // 页面在响应读取前已取消该请求（导航/卸载/关闭）：把它当作已取消。
      await abortQuietly(route);
      return;
    }
    if (!response.ok()) {
      try {
        await route.fulfill({ status: response.status(), headers, body: raw });
      } catch {
        await abortQuietly(route);
      }
      return;
    }
    let body: { user?: { id?: string } };
    try {
      body = JSON.parse(raw.toString('utf8')) as { user?: { id?: string } };
    } catch {
      // 非 JSON 的异常响应原样回填，保持与后端一致的错误语义。
      try {
        await route.fulfill({ status: response.status(), headers, body: raw });
      } catch {
        await abortQuietly(route);
      }
      return;
    }
    if (body.user) {
      body.user.id = await approverUserId();
    }
    try {
      await route.fulfill({
        status: response.status(),
        headers,
        body: JSON.stringify(body),
      });
    } catch {
      await abortQuietly(route);
    }
  });
  await page.route('**/api/approvals/*/decide', async (route) => {
    const request = route.request();
    const origin = new URL(request.url()).origin;
    const headers: Record<string, string> = {
      Origin: origin,
      Cookie: approverCookie(),
      Accept: 'application/json',
    };
    const contentType = request.headers()['content-type'];
    if (contentType) {
      headers['Content-Type'] = contentType;
    }
    const body = request.postData();
    const response = await fetch(request.url(), {
      method: request.method(),
      headers,
      body: body ?? undefined,
    });
    const responseBody = Buffer.from(await response.arrayBuffer());
    const outHeaders: Record<string, string> = {};
    const responseType = response.headers.get('content-type');
    if (responseType) {
      outHeaders['content-type'] = responseType;
    }
    await route.fulfill({
      status: response.status,
      headers: outHeaders,
      body: responseBody,
    });
  });
}

export const test = base.extend<{ approverIdentity: void }>({
  approverIdentity: [
    async ({ context, page }, use) => {
      // 默认 page + 已存在的其它窗口。
      for (const existing of context.pages()) {
        await installApproverIdentity(existing);
      }
      // 测试中途打开的 popup（如诊断深链）同样需要身份注入。
      context.on('page', (opened) => {
        void installApproverIdentity(opened);
      });
      if (!context.pages().includes(page)) {
        await installApproverIdentity(page);
      }
      await use();
    },
    { auto: true },
  ],
});

export { expect };
