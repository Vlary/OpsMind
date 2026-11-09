import { useSyncExternalStore } from 'react';

import type { ThemeMode } from './theme';

/**
 * CONTRACT.md 8.3.12：主题偏好三态。
 *
 * `system` 表示跟随 `prefers-color-scheme`；`resolved` 才是实际生效的
 * light/dark，CSS 与 antd 都只认后者。
 */
export type ThemePreference = 'light' | 'dark' | 'system';

/** localStorage 键（契约 8.3.12 写死；不得改名，否则老用户偏好丢失）。 */
export const THEME_STORAGE_KEY = 'opsmind.theme';

/** 默认偏好 = 浅色（契约 8.3 首条：浅色为默认主题）。 */
export const DEFAULT_THEME_PREFERENCE: ThemePreference = 'light';

const DARK_QUERY = '(prefers-color-scheme: dark)';

/** `light | dark | system` 的合法值集合，供读取时校验。 */
const VALID_PREFERENCES: readonly ThemePreference[] = ['light', 'dark', 'system'];

/**
 * 把偏好解析为实际生效的模式。
 *
 * 三态 × 系统明暗全矩阵（F04 断言）：
 * - `light` → light（无论系统）
 * - `dark` → dark（无论系统）
 * - `system` → 系统暗则 dark，否则 light
 */
export function resolveTheme(pref: ThemePreference, systemDark: boolean): ThemeMode {
  if (pref === 'light') {
    return 'light';
  }
  if (pref === 'dark') {
    return 'dark';
  }
  return systemDark ? 'dark' : 'light';
}

/** 最小 Storage 形状：只用到 getItem / setItem，便于单测注入。 */
export interface PreferenceStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

/**
 * 读取已存偏好。
 *
 * 缺失、非法值、`localStorage` 抛异常（隐私模式 / 存储被禁）一律回落
 * `light`——契约 8.3.12 的「兜底按 light」，绝不让异常冒到首绘。
 */
export function readStoredPreference(
  storage: PreferenceStorage | null | undefined,
): ThemePreference {
  if (!storage) {
    return DEFAULT_THEME_PREFERENCE;
  }
  try {
    const raw = storage.getItem(THEME_STORAGE_KEY);
    if (raw && (VALID_PREFERENCES as readonly string[]).includes(raw)) {
      return raw as ThemePreference;
    }
  } catch {
    // 隐私模式下 localStorage 读写可能直接抛 SecurityError。
  }
  return DEFAULT_THEME_PREFERENCE;
}

/**
 * 写入偏好。写失败（配额/隐私模式）静默忽略：主题已在本会话生效，
 * 不该因为存不下而弹错或中断交互。
 */
export function writeStoredPreference(
  storage: PreferenceStorage | null | undefined,
  pref: ThemePreference,
): void {
  if (!storage) {
    return;
  }
  try {
    storage.setItem(THEME_STORAGE_KEY, pref);
  } catch {
    // 忽略：持久化失败不影响当前会话的主题。
  }
}

/** 浏览器当前是否偏好深色；无 matchMedia 的环境（SSR/单测）按浅色。 */
export function systemPrefersDark(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return false;
  }
  return window.matchMedia(DARK_QUERY).matches;
}

/**
 * 订阅系统明暗变化。返回取消订阅函数。
 *
 * 旧 Safari（< 14）只有 `addListener`，这里一并兼容。
 */
export function subscribeSystemTheme(onChange: () => void): () => void {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return () => undefined;
  }
  const mql = window.matchMedia(DARK_QUERY);
  if (typeof mql.addEventListener === 'function') {
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  }
  const legacy = mql as unknown as {
    addListener?: (cb: () => void) => void;
    removeListener?: (cb: () => void) => void;
  };
  legacy.addListener?.(onChange);
  return () => {
    legacy.removeListener?.(onChange);
  };
}

/**
 * 把解析结果写回 DOM（`data-theme` + `data-theme-preference`）。
 *
 * CSS 只认 `:root[data-theme='dark']`；`data-theme-preference` 仅用于
 * hook 还原三态选择与调试，不参与样式选择器。
 */
export function applyThemeToDom(pref: ThemePreference, resolved: ThemeMode): void {
  if (typeof document === 'undefined') {
    return;
  }
  document.documentElement.dataset['theme'] = resolved;
  document.documentElement.dataset['themePreference'] = pref;
}

/** 读当前生效模式：**以 `<html data-theme>` 为唯一事实源**。 */
function readDomTheme(): ThemeMode {
  if (typeof document === 'undefined') {
    return 'light';
  }
  return document.documentElement.dataset['theme'] === 'dark' ? 'dark' : 'light';
}

function readDomPreference(): ThemePreference {
  if (typeof document === 'undefined') {
    return DEFAULT_THEME_PREFERENCE;
  }
  const raw = document.documentElement.dataset['themePreference'];
  if (raw && (VALID_PREFERENCES as readonly string[]).includes(raw)) {
    return raw as ThemePreference;
  }
  return DEFAULT_THEME_PREFERENCE;
}

// ---------------------------------------------------------------------------
// 外部订阅：主题变更先写 DOM 再通知 React，保证 CSS 与组件树同帧更新。
//
// 不用新增 Zustand store（契约 8.3.12：8.5 只允许两个 store，且主题必须在
// React 挂载前生效——store 模块求值无法早于 index.html 内联脚本）。
// ---------------------------------------------------------------------------

const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) {
    listener();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** 供测试与外部代码在 DOM 被直接改写后手动广播。 */
export function notifyThemeChange(): void {
  emit();
}

function getStorage(): PreferenceStorage | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage;
  } catch {
    return null;
  }
}

/**
 * 当前偏好 → 解析 → 落 DOM 的完整应用流程。
 *
 * `setMode` 与 system 变化都走这里，确保 DOM / localStorage / 订阅者
 * 三者一次性对齐。
 */
function applyPreference(pref: ThemePreference): void {
  const resolved = resolveTheme(pref, systemPrefersDark());
  applyThemeToDom(pref, resolved);
  emit();
}

/** 全局只订阅一次系统明暗变化（模块级），避免每次 hook 挂载都加监听。 */
let systemSubscribed = false;
function ensureSystemSubscription(): void {
  if (systemSubscribed) {
    return;
  }
  systemSubscribed = true;
  subscribeSystemTheme(() => {
    // 只有 system 偏好才跟随；light/dark 下系统变化不影响 DOM。
    if (readDomPreference() === 'system') {
      applyPreference('system');
    }
  });
}

/**
 * 外部 store 快照：用 `data-theme|data-theme-preference` 拼串，
 * `useSyncExternalStore` 靠它判断是否变化（字符串比较天然稳定）。
 */
function snapshot(): string {
  if (typeof document === 'undefined') {
    return 'light|light';
  }
  return `${document.documentElement.dataset['theme'] ?? 'light'}|${
    document.documentElement.dataset['themePreference'] ?? 'light'
  }`;
}

/**
 * 当前偏好 → 应用（不写 localStorage）。供测试与外部同步入口使用。
 * 正式交互请用 `useTheme().setMode`（会一并持久化）。
 */
export function setThemePreference(pref: ThemePreference): void {
  applyPreference(pref);
}

export interface ThemeController {
  /**
   * 实际生效模式（light / dark），供 antd `themeConfig(mode)` 与
   * `terminalTheme(mode)` 使用。
   */
  mode: ThemeMode;
  /** 与 `mode` 同值（契约 8.3.12 的 `resolved` 命名）。 */
  resolved: ThemeMode;
  /** 用户偏好三态，仅供切换控件回显；不影响 CSS。 */
  preference: ThemePreference;
  /** 设置偏好：写 localStorage、写 DOM、通知订阅者。 */
  setMode: (pref: ThemePreference) => void;
}

/**
 * 主题 hook：`{ mode, resolved, preference, setMode }`。
 *
 * `mode` 与 `resolved` 都是**已解析**的 light/dark（antd 与 xterm 直接用
 * 它）；三态偏好放在 `preference`，只有切换控件需要它。这样 `mode` 与
 * `themeConfig(mode)` / `terminalTheme(mode)` 的入参类型天然一致，不会
 * 出现把 `'system'` 误传给只认 light|dark 的函数。
 *
 * 用 `useSyncExternalStore` 订阅 DOM：主题在 index.html 内联脚本里首绘前
 * 就已写好，React 只是跟随，不制造第二份真相。
 */
export function useTheme(): ThemeController {
  ensureSystemSubscription();

  const raw = useSyncExternalStore(subscribe, snapshot, snapshot);
  const [themeRaw, prefRaw] = raw.split('|');
  const resolved: ThemeMode = themeRaw === 'dark' ? 'dark' : 'light';
  const preference: ThemePreference =
    prefRaw && (VALID_PREFERENCES as readonly string[]).includes(prefRaw)
      ? (prefRaw as ThemePreference)
      : DEFAULT_THEME_PREFERENCE;

  return {
    mode: resolved,
    resolved,
    preference,
    setMode: (next: ThemePreference) => {
      writeStoredPreference(getStorage(), next);
      applyPreference(next);
    },
  };
}

/**
 * 供 `index.html` 内联脚本与测试共用的「首次绘制前」解析逻辑：
 * 读偏好 → 解析 → 返回应写入 DOM 的两个 dataset 值。
 *
 * 内联脚本必须自包含（不能 import 模块），此处保持同一套语义便于断言。
 */
export function initialThemeState(
  storage: PreferenceStorage | null | undefined,
  systemDark: boolean,
): { preference: ThemePreference; resolved: ThemeMode } {
  const preference = readStoredPreference(storage);
  return { preference, resolved: resolveTheme(preference, systemDark) };
}

/** 当前 DOM 上的生效模式（非 React 环境读取用）。 */
export function currentThemeMode(): ThemeMode {
  return readDomTheme();
}
