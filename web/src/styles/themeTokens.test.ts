import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { TERMINAL_PALETTES } from './palette';
import { EFFECT_TOKENS, SEMANTIC_TOKENS, themeConfig } from './theme';

// CONTRACT.md 8.3.14 F05：用 node:fs 读 tokens.css 原文（**不要**用 ?raw：
// 实测 vitest 下 ?raw 返回空串），解析两套变量，逐键断言与 theme.ts /
// palette.ts 一致，并断言两套键集合完全相同（防漏定义）。

const here = dirname(fileURLToPath(import.meta.url));
const tokensCss = readFileSync(resolve(here, 'tokens.css'), 'utf8');

type Decl = { name: string; value: string };

/**
 * 从 `tokens.css` 里取某个选择器块内的自定义属性声明。
 *
 * 只做本文件需要的极简解析：找到 `selector {` 起至匹配的 `}`，逐行取
 * `--name: value;`。注释先剔除，避免注释里的示例被当成声明。
 */
function declarationsIn(selector: string): Decl[] {
  const withoutComments = tokensCss.replace(/\/\*[\s\S]*?\*\//g, '');
  const start = withoutComments.indexOf(`${selector} {`);
  expect(start, `tokens.css 缺少选择器 ${selector}`).toBeGreaterThanOrEqual(0);
  const open = withoutComments.indexOf('{', start);
  const close = withoutComments.indexOf('\n}', open);
  const body = withoutComments.slice(open + 1, close);
  const decls: Decl[] = [];
  for (const line of body.split('\n')) {
    const match = /^\s*(--[a-z0-9-]+)\s*:\s*(.+?);\s*$/.exec(line);
    if (match) {
      decls.push({ name: match[1] as string, value: match[2] as string });
    }
  }
  return decls;
}

/** 只取语义/图表/代码类令牌（`--color-*`；旧调色板别名已按 8.3.13 删除）。 */
function semanticMap(selector: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const { name, value } of declarationsIn(selector)) {
    if (name.startsWith('--color-')) {
      map.set(name.slice(2), value);
    }
  }
  return map;
}

const lightCss = semanticMap(':root');
const darkCss = semanticMap(":root[data-theme='dark']");

/** 契约 8.3.7 允许两主题同值的令牌。 */
const SAME_VALUE_ALLOWED = new Set(['color-overlay-media-bg', 'color-overlay-media-text']);

describe('tokens.css 两套主题的键集合', () => {
  it('浅色与深色定义的 --color-* 键完全相同（防漏定义）', () => {
    const lightKeys = [...lightCss.keys()].sort();
    const darkKeys = [...darkCss.keys()].sort();
    expect(darkKeys).toEqual(lightKeys);
  });

  it('除 8.3.7 的媒体浮层外，两主题取值不得相同', () => {
    const identical: string[] = [];
    for (const [key, value] of lightCss) {
      if (SAME_VALUE_ALLOWED.has(key)) {
        continue;
      }
      if (darkCss.get(key) === value) {
        identical.push(key);
      }
    }
    expect(identical).toEqual([]);
  });

  it('两套主题都声明 color-scheme 且取值正确（8.3.11 第 6 条）', () => {
    expect(tokensCss).toMatch(/:root\s*\{[^}]*color-scheme:\s*light;/s);
    expect(tokensCss).toMatch(/:root\[data-theme='dark'\]\s*\{[^}]*color-scheme:\s*dark;/s);
  });

  it('深色只由 :root[data-theme=dark] 触发（不得用 prefers-color-scheme 直接切）', () => {
    // 主题偏好由 data-theme 表达；CSS 里出现媒体查询会让「跟随系统」以外的
    // 偏好（显式 light）被系统暗色覆盖。
    expect(tokensCss).not.toMatch(/@media\s*\(prefers-color-scheme/);
  });
});

describe('tokens.css ↔ theme.ts SEMANTIC_TOKENS', () => {
  it.each(['light', 'dark'] as const)('%s 主题逐键一致', (mode) => {
    const css = mode === 'light' ? lightCss : darkCss;
    const ts = SEMANTIC_TOKENS[mode] as Record<string, string>;
    const mismatches: string[] = [];
    for (const [key, value] of Object.entries(ts)) {
      const fromCss = css.get(key);
      if (fromCss === undefined) {
        mismatches.push(`${key}: tokens.css 缺失`);
      } else if (fromCss.toLowerCase() !== value.toLowerCase()) {
        mismatches.push(`${key}: css=${fromCss} ts=${value}`);
      }
    }
    expect(mismatches).toEqual([]);
  });

  it('SEMANTIC_TOKENS 没有 tokens.css 未定义的键', () => {
    for (const mode of ['light', 'dark'] as const) {
      const css = mode === 'light' ? lightCss : darkCss;
      const missing = Object.keys(SEMANTIC_TOKENS[mode]).filter((key) => !css.has(key));
      expect(missing, `${mode} 主题`).toEqual([]);
    }
  });

  it('light / dark 的键集合相同', () => {
    expect(Object.keys(SEMANTIC_TOKENS.dark).sort()).toEqual(
      Object.keys(SEMANTIC_TOKENS.light).sort(),
    );
  });
});

describe('tokens.css ↔ theme.ts EFFECT_TOKENS', () => {
  it.each(['light', 'dark'] as const)('%s 主题逐键一致', (mode) => {
    const css = mode === 'light' ? lightCss : darkCss;
    const ts = EFFECT_TOKENS[mode] as Record<string, string>;
    const mismatches: string[] = [];
    for (const [key, value] of Object.entries(ts)) {
      const fromCss = css.get(key);
      if (fromCss === undefined) {
        mismatches.push(`${key}: tokens.css 缺失`);
      } else if (fromCss.toLowerCase() !== value.toLowerCase()) {
        mismatches.push(`${key}: css=${fromCss} ts=${value}`);
      }
    }
    expect(mismatches).toEqual([]);
  });
});

describe('theme.ts themeConfig 与语义令牌同源', () => {
  it.each(['light', 'dark'] as const)('%s：核心 token 逐键取自 SEMANTIC_TOKENS', (mode) => {
    const cfg = themeConfig(mode);
    const c = SEMANTIC_TOKENS[mode];
    expect(cfg.token?.['colorPrimary']).toBe(c['color-primary']);
    expect(cfg.token?.['colorPrimaryHover']).toBe(c['color-primary-hover']);
    expect(cfg.token?.['colorPrimaryActive']).toBe(c['color-primary-active']);
    expect(cfg.token?.['colorInfo']).toBe(c['color-info']);
    expect(cfg.token?.['colorSuccess']).toBe(c['color-success']);
    expect(cfg.token?.['colorWarning']).toBe(c['color-warning']);
    expect(cfg.token?.['colorError']).toBe(c['color-danger']);
    expect(cfg.token?.['colorLink']).toBe(c['color-link']);
    expect(cfg.token?.['colorBgBase']).toBe(c['color-bg-base']);
    expect(cfg.token?.['colorBgContainer']).toBe(c['color-bg-container']);
    expect(cfg.token?.['colorBgElevated']).toBe(c['color-bg-elevated']);
    expect(cfg.token?.['colorText']).toBe(c['color-text']);
    expect(cfg.token?.['colorTextSecondary']).toBe(c['color-text-secondary']);
    expect(cfg.token?.['colorTextTertiary']).toBe(c['color-text-tertiary']);
    expect(cfg.token?.['colorTextPlaceholder']).toBe(c['color-text-placeholder']);
    expect(cfg.token?.['colorTextDisabled']).toBe(c['color-text-disabled']);
    expect(cfg.token?.['colorTextLightSolid']).toBe(c['color-text-inverse']);
    expect(cfg.token?.['colorBorder']).toBe(c['color-border']);
    expect(cfg.token?.['colorBorderSecondary']).toBe(c['color-border-subtle']);
  });

  it('两个主题都用 defaultAlgorithm（禁止 darkAlgorithm，8.3.8）', async () => {
    const antd = await import('antd');
    for (const mode of ['light', 'dark'] as const) {
      expect(themeConfig(mode).algorithm).toBe(antd.theme.defaultAlgorithm);
      expect(themeConfig(mode).algorithm).not.toBe(antd.theme.darkAlgorithm);
    }
  });

  it('深色 Tooltip 底必须提亮，否则文字不可读（8.3.15）', () => {
    expect(themeConfig('dark').token?.['colorBgSpotlight']).toBe('#e6e9f2');
    expect(themeConfig('light').token?.['colorBgSpotlight']).toBe('#313244');
  });

  it('Menu darkItem* 必须逐键钉死（默认会合成 1.05:1）', () => {
    const menu = themeConfig('dark').components?.['Menu'];
    expect(menu?.['darkItemColor']).toBe('#bac2de');
    expect(menu?.['darkItemHoverColor']).toBe('#cdd6f4');
    expect(menu?.['darkItemBg']).toBe('#181825');
    expect(menu?.['darkItemSelectedBg']).toBe('#383b4d');
    expect(menu?.['darkItemSelectedColor']).toBe('#89b4fa');
    expect(menu?.['darkPopupBg']).toBe('#313244');
    expect(menu?.['darkSubMenuItemBg']).toBe('#181825');
    expect(menu?.['darkGroupTitleColor']).toBe('#a6adc8');
    expect(menu?.['darkItemDisabledColor']).toBe('#8b90a6');
  });

  it('Layout triggerBg/triggerColor 必须钉死（默认 1.01:1）', () => {
    for (const mode of ['light', 'dark'] as const) {
      const layout = themeConfig(mode).components?.['Layout'];
      expect(layout?.['triggerBg']).toBeTruthy();
      expect(layout?.['triggerColor']).toBeTruthy();
    }
  });

  it.each(['light', 'dark'] as const)(
    '%s：输入类控件外框钉为 --color-border-strong（8.3.11 可操作边界 ≥ 3:1）',
    (mode) => {
      const c = SEMANTIC_TOKENS[mode];
      const components = themeConfig(mode).components;
      // 全局 colorBorder 仍是装饰级弱线（表格/卡片分隔线），不得被抬高。
      expect(themeConfig(mode).token?.['colorBorder']).toBe(c['color-border']);
      expect(c['color-border-strong']).not.toBe(c['color-border']);
      for (const name of ['Input', 'InputNumber', 'Select', 'DatePicker'] as const) {
        expect(components?.[name]?.['colorBorder'], `${mode} ${name}`).toBe(
          c['color-border-strong'],
        );
      }
    },
  );

  it.each(['light', 'dark'] as const)(
    '%s：Slider 未播放轨道用 --color-border-strong，播放段与滑块用 primary',
    (mode) => {
      const c = SEMANTIC_TOKENS[mode];
      const slider = themeConfig(mode).components?.['Slider'];
      expect(slider?.['railBg']).toBe(c['color-border-strong']);
      expect(slider?.['railHoverBg']).toBe(c['color-border-strong']);
      expect(slider?.['trackBg']).toBe(c['color-primary']);
      expect(slider?.['trackHoverBg']).toBe(c['color-primary-hover']);
      expect(slider?.['handleColor']).toBe(c['color-primary']);
      expect(slider?.['handleActiveColor']).toBe(c['color-primary']);
    },
  );

  it.each(['light', 'dark'] as const)(
    '%s：表格空态提示不得用禁用色（8.3.11：禁用色不得承载提示文案）',
    (mode) => {
      const c = SEMANTIC_TOKENS[mode];
      const table = themeConfig(mode).components?.['Table'];
      // antd 的 `table/style/empty.js` 直接用 colorTextDisabled 渲染空态。
      expect(table?.['colorTextDisabled']).toBe(c['color-text-tertiary']);
      expect(table?.['colorTextDisabled']).not.toBe(c['color-text-disabled']);
    },
  );

  it('Tag 预设色不在 ThemeConfig 里钉（antd 会用 seed 色板覆盖，见 tokens.css）', () => {
    // antd 的 defaultAlgorithm 在运行时重算 green1..10 等并覆盖显式 token，
    // 因此在 theme.ts 声明这些键无效；实际色阶由 tokens.css 的 .css-var-*
    // 桥接覆盖 antd 生成的 --ant-* 变量。这里断言「没有无效声明」，
    // 防有人再次把它加回来并误以为生效。
    for (const mode of ['light', 'dark'] as const) {
      const token = themeConfig(mode).token ?? {};
      for (const color of ['green', 'gold', 'red', 'blue', 'cyan', 'geekblue']) {
        for (const step of ['1', '3', '6', '7']) {
          const key = `${color}${step}`;
          expect(token[key as keyof typeof token], `${mode} 不应声明 ${key}`).toBeUndefined();
        }
      }
    }
  });

  it('tokens.css 用 .css-var-* 桥接覆盖 6 组预设色阶', () => {
    const css = readFileSync(resolve(here, 'tokens.css'), 'utf8');
    expect(css).toMatch(/\[class\*='css-var-'\]/);
    for (const color of ['green', 'gold', 'red', 'blue', 'cyan', 'geekblue']) {
      for (const step of ['1', '3', '6', '7']) {
        expect(css).toContain(`--ant-${color}-${step}: var(--color-`);
      }
    }
  });
});

describe('palette.ts 终端色板', () => {
  const ANSI_KEYS = [
    'black',
    'brightBlack',
    'red',
    'brightRed',
    'green',
    'brightGreen',
    'yellow',
    'brightYellow',
    'blue',
    'brightBlue',
    'magenta',
    'brightMagenta',
    'cyan',
    'brightCyan',
    'white',
    'brightWhite',
  ] as const;

  it.each(['light', 'dark'] as const)('%s：16 个 ANSI 色全部显式给出', (mode) => {
    const theme = TERMINAL_PALETTES[mode];
    for (const key of ANSI_KEYS) {
      expect(theme[key], `${mode} 缺 ${key}`).toMatch(/^#[0-9a-f]{6}$/);
    }
    expect(theme.background).toMatch(/^#[0-9a-f]{6}$/);
    expect(theme.foreground).toMatch(/^#[0-9a-f]{6}$/);
    expect(theme.cursor).toMatch(/^#[0-9a-f]{6}$/);
    expect(theme.selectionBackground).toMatch(/^#[0-9a-f]{6}$/);
  });

  it('浅色终端底与 --color-bg-code 同值', () => {
    expect(TERMINAL_PALETTES.light.background).toBe(SEMANTIC_TOKENS.light['color-bg-code']);
  });

  it('深色终端底与 --color-bg-code 同值', () => {
    expect(TERMINAL_PALETTES.dark.background).toBe(SEMANTIC_TOKENS.dark['color-bg-code']);
  });

  it('终端不是只换背景：两主题的 ANSI 色互不相同', () => {
    const light = TERMINAL_PALETTES.light;
    const dark = TERMINAL_PALETTES.dark;
    const same = ANSI_KEYS.filter((key) => light[key] === dark[key]);
    expect(same).toEqual([]);
  });
});
