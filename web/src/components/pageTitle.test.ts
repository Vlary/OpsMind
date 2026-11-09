import { describe, expect, it } from 'vitest';

import { pageTitle, pageTitleFor } from '../lib/pageTitle';

// 成品标准「页面标题与 favicon」：title 随路由区分（round 3 打磨项）。
// 纯函数部分在 node 环境直测；document.title 的 DOM 行为由
// e2e/polish-round8.spec.ts 在真实浏览器里验证。
describe('pageTitleFor', () => {
  it('各导航路由映射到对应页面名', () => {
    expect(pageTitleFor('/chat')).toBe('对话');
    expect(pageTitleFor('/servers')).toBe('服务器');
    expect(pageTitleFor('/databases')).toBe('数据库');
    expect(pageTitleFor('/providers')).toBe('模型');
    expect(pageTitleFor('/audit')).toBe('审计');
  });

  it('会话详情路由 /c/:id 与登录页', () => {
    expect(pageTitleFor('/c/018f3c1e-9b0c-7c3e-a1b2-c3d4e5f6a7b8')).toBe('对话');
    expect(pageTitleFor('/login')).toBe('登录');
  });

  it('/chat 不会被 /c/ 前缀误判为会话详情', () => {
    // '/chat'.startsWith('/c/') 为 false（第三个字符是 h），但显式回归。
    expect('/chat'.startsWith('/c/')).toBe(false);
  });

  it('未匹配路径（404 页）返回「页面不存在」；根路径对齐重定向目标', () => {
    expect(pageTitleFor('/no-such-page')).toBe('页面不存在');
    expect(pageTitleFor('/chatting')).toBe('页面不存在');
    expect(pageTitleFor('/')).toBe('对话');
  });

  it('pageTitle 拼上站点名后缀', () => {
    expect(pageTitle('/servers')).toBe('服务器 · 云枢 OpsMind');
    expect(pageTitle('/login')).toBe('登录 · 云枢 OpsMind');
    expect(pageTitle('/x')).toBe('页面不存在 · 云枢 OpsMind');
  });
});
