import { describe, expect, it } from 'vitest';

import { NAV_ITEMS } from '../components/navItems';
import { pageTitle, pageTitleFor } from './pageTitle';

// CONTRACT.md 8.3.10：页面标题三层一致（写死不变量）。
//
// 导航 label（NAV_ITEMS）、PageHeader 的 title、document.title（pageTitle()）
// 三者逐字相同，口径是「页头跟随导航」。历史缺陷：/rdp 页头曾为「RDP 桌面」
// 而导航是「桌面」；/batch 页头曾为「批量操作」而导航是「批量」。
//
// 用例由 NAV_ITEMS 驱动（契约明令不得手写 15 条字面量——手写会退化成又一份
// 会漂移的副本）：
//  1) 每个导航项 → pageTitleFor(key) 必须等于该项 label；
//  2) 反向：PAGE_TITLES 里除 /login（公开页）与 /（重定向到 /chat）外，
//     每一项都必须能在 NAV_ITEMS 找到同名 key。
describe('页面标题：导航与页面标题三层一致（契约 8.3.10）', () => {
  it('每个导航项的 label 与 pageTitleFor(key) 逐字相同', () => {
    for (const item of NAV_ITEMS) {
      expect(pageTitleFor(item.key), item.key).toBe(item.label);
    }
  });

  it('每个导航项的子路径仍归属同一页面', () => {
    for (const item of NAV_ITEMS) {
      expect(pageTitleFor(`${item.key}/detail-1`), item.key).toBe(item.label);
    }
  });

  it('pageTitle 后缀固定为「 · 云枢 OpsMind」', () => {
    for (const item of NAV_ITEMS) {
      expect(pageTitle(item.key)).toBe(`${item.label} · 云枢 OpsMind`);
    }
  });

  it('反向覆盖：公开页之外不存在导航里没有的页面名', () => {
    const labels = new Set(NAV_ITEMS.map((item) => item.label));
    // 契约列出的两个例外：/login 公开页无导航项；/ 重定向到 /chat（同为「对话」）。
    const exceptions = new Set(['登录']);
    const probes = [
      '/login',
      '/',
      '/chat',
      '/c/550e8400-e29b-41d4-a716-446655440000',
      '/servers',
      '/databases',
      '/providers',
      '/audit',
      '/term',
      '/recordings',
      '/rdp',
      '/containers',
      '/monitor',
      '/alerts',
      '/inspections',
      '/batch',
      '/settings',
      '/definitely-not-a-route',
    ];
    for (const path of probes) {
      const name = pageTitleFor(path);
      if (exceptions.has(name) || name === '页面不存在') {
        continue;
      }
      expect(labels, `${path} 的页面名「${name}」必须在 NAV_ITEMS 中找到`).toContain(name);
    }
  });

  it('旧的页头变体名不再表示任何路由（「RDP 桌面」「批量操作」）', () => {
    expect(pageTitleFor('/rdp')).toBe('桌面');
    expect(pageTitleFor('/batch')).toBe('批量');
    expect(pageTitleFor('/rdp')).not.toBe('RDP 桌面');
    expect(pageTitleFor('/batch')).not.toBe('批量操作');
  });
});
