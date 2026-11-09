import type { ITheme } from '@xterm/xterm';

import type { ThemeMode } from './theme';

/*
 * CONTRACT.md 8.3.6：xterm 终端色板。
 *
 * 这是**唯一允许十六进制字面量的 TS 文件**（8.3.1 第 1 条）——xterm 的
 * `theme` 不接受 `var(--x)`（实测传入后 foreground 落回默认 #ffffff、
 * background 不生效），因此两主题各一套字面量在这里集中定义。
 *
 * 16 个 ANSI 色必须全部显式给出，不得依赖 xterm 默认调色板：默认调色板在
 * 深色底 #11111b 上有 6 色 < 4.5:1（black 1.48 / red 3.19 / blue 3.16 /
 * magenta 2.85 / brightBlack 2.57 / brightRed 4.48）。本表两主题在各自
 * 底色上全部 ≥ 4.5:1（浅色最小 5.53、深色最小 6.64），在选区上全部
 * ≥ 4.5:1（浅色最小 4.73、深色最小 5.00）。
 */

const LIGHT: ITheme = {
  background: '#f6f8fa',
  foreground: '#1f2430',
  cursor: '#1a56c4',
  selectionBackground: '#dbe7ff',
  black: '#1f2430',
  brightBlack: '#5b6373',
  red: '#c0272d',
  brightRed: '#a51f24',
  green: '#146c2e',
  brightGreen: '#0f5a26',
  yellow: '#8a5a00',
  brightYellow: '#744c00',
  blue: '#1a56c4',
  brightBlue: '#14479f',
  magenta: '#6d28d9',
  brightMagenta: '#5b21b6',
  cyan: '#0b6e7f',
  brightCyan: '#095e6c',
  white: '#5b6373',
  brightWhite: '#1f2430',
};

const DARK: ITheme = {
  background: '#11111b',
  foreground: '#cdd6f4',
  cursor: '#f5e0dc',
  selectionBackground: '#262b3a',
  black: '#9399b2',
  brightBlack: '#a6adc8',
  red: '#f38ba8',
  brightRed: '#f5a3bd',
  green: '#a6e3a1',
  brightGreen: '#b9eab5',
  yellow: '#f9e2af',
  brightYellow: '#fbecc4',
  blue: '#89b4fa',
  brightBlue: '#a3c5fb',
  magenta: '#cba6f7',
  brightMagenta: '#d8bdfa',
  cyan: '#94e2d5',
  brightCyan: '#aeece7',
  white: '#bac2de',
  brightWhite: '#cdd6f4',
};

/** 导出供 themeTokens 测试逐键比对；运行时只用 `terminalTheme(mode)`。 */
export const TERMINAL_PALETTES = { light: LIGHT, dark: DARK } as const;

/**
 * 当前模式的 xterm 主题。切换主题时只需
 * `terminal.options.theme = terminalTheme(mode)` 热更新——不要销毁重建
 * 终端（会丢 PTY 尺寸与输入，见 8.3.15）。
 */
export function terminalTheme(mode: ThemeMode): ITheme {
  return mode === 'dark' ? DARK : LIGHT;
}
