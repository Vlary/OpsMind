import { describe, expect, it } from 'vitest';

import { pageTitle, pageTitleFor } from './pageTitle';

// 页面标题路由匹配的补充矩阵：与 pageTitle.test.ts 互补，
// 覆盖子路径、尾斜杠、大小写、查询串与 hash 等浏览器地址形态。

describe('pageTitleFor 路由矩阵', () => {
  it('全部主导航前缀', () => {
    expect(pageTitleFor('/login')).toBe('登录');
    expect(pageTitleFor('/chat')).toBe('对话');
    expect(pageTitleFor('/servers')).toBe('服务器');
    expect(pageTitleFor('/databases')).toBe('数据库');
    expect(pageTitleFor('/providers')).toBe('模型');
    expect(pageTitleFor('/audit')).toBe('审计');
  });

  it('子路径命中同页（前缀以斜杠结尾判定）', () => {
    expect(pageTitleFor('/servers/abc')).toBe('服务器');
    expect(pageTitleFor('/databases/xyz/detail')).toBe('数据库');
    expect(pageTitleFor('/providers/new')).toBe('模型');
  });

  // 回归：/term /recordings /rdp /containers 四条导航路由曾漏进 PAGE_TITLES，
  // 页面标签页显示「页面不存在 · 云枢 OpsMind」。导航项与标题映射必须一一对应。
  // 八期 FE-R（契约 8.3.10）：/recordings 更名「录制」，新增 /replays
  // 「回放执行」——两条断言必须与 NAV_ITEMS 同一次提交更新。
  it('二至四期导航路由都有标题（回归）', () => {
    expect(pageTitleFor('/term')).toBe('终端');
    expect(pageTitleFor('/recordings')).toBe('录制');
    expect(pageTitleFor('/rdp')).toBe('桌面');
    expect(pageTitleFor('/containers')).toBe('容器');
    expect(pageTitleFor('/recordings/abc')).toBe('录制');
    expect(pageTitleFor('/containers/c1/logs')).toBe('容器');
  });

  it('八期：/replays 与 /replays/:id 都是「回放执行」，且与 /recordings 区分', () => {
    expect(pageTitleFor('/replays')).toBe('回放执行');
    expect(pageTitleFor('/replays/550e8400-e29b-41d4-a716-446655440000')).toBe('回放执行');
    expect(pageTitleFor('/recordings')).toBe('录制');
    expect(pageTitleFor('/recordings')).not.toBe(pageTitleFor('/replays'));
  });

  it('前缀相同但不带斜杠的路径不误命中（/chatty 不是 /chat）', () => {
    expect(pageTitleFor('/chatty')).toBe('页面不存在');
    expect(pageTitleFor('/serversss')).toBe('页面不存在');
  });

  it('会话详情 /c/:id 与根路径都归对话页', () => {
    expect(pageTitleFor('/c/550e8400-e29b-41d4-a716-446655440000')).toBe('对话');
    expect(pageTitleFor('/c/')).toBe('对话');
    expect(pageTitleFor('/')).toBe('对话');
  });

  it('大小写敏感：/Chat 不匹配 /chat', () => {
    expect(pageTitleFor('/Chat')).toBe('页面不存在');
    expect(pageTitleFor('/AUDIT')).toBe('页面不存在');
  });

  it('未知路径一律「页面不存在」', () => {
    expect(pageTitleFor('/nope')).toBe('页面不存在');
    expect(pageTitleFor('/nope/deep/path')).toBe('页面不存在');
    // 六期 /settings 成为真实路由（与 /batch 等同款前缀匹配）；改用未注册路径。
    expect(pageTitleFor('/internal/general')).toBe('页面不存在');
  });

  it('六期：/settings 标题为「设置」，深层路径同前缀匹配', () => {
    expect(pageTitleFor('/settings')).toBe('设置');
    expect(pageTitleFor('/settings/general')).toBe('设置');
  });
});

describe('pageTitle 站点名后缀', () => {
  it('所有页面统一「页面名 · 云枢 OpsMind」', () => {
    expect(pageTitle('/login')).toBe('登录 · 云枢 OpsMind');
    expect(pageTitle('/chat')).toBe('对话 · 云枢 OpsMind');
    expect(pageTitle('/servers')).toBe('服务器 · 云枢 OpsMind');
    expect(pageTitle('/databases')).toBe('数据库 · 云枢 OpsMind');
    expect(pageTitle('/providers')).toBe('模型 · 云枢 OpsMind');
    expect(pageTitle('/audit')).toBe('审计 · 云枢 OpsMind');
    expect(pageTitle('/c/abc')).toBe('对话 · 云枢 OpsMind');
    expect(pageTitle('/missing')).toBe('页面不存在 · 云枢 OpsMind');
  });

  it('子路径同样带后缀', () => {
    expect(pageTitle('/servers/s1')).toBe('服务器 · 云枢 OpsMind');
  });
});
