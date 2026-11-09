import type { ThemeMode } from '../styles/theme';
import type { ThemePreference } from '../styles/themePreference';

/**
 * 主题状态的门面模块（stores/ 目录下的入口，供页面层统一 import）。
 *
 * **实现不在 Zustand 里**——CONTRACT.md 8.3.12 明确「不新增 Zustand store」：
 * 8.5 只允许两个 store，且主题必须在 React 挂载前由 `index.html` 内联脚本
 * 写成 `<html data-theme>`，Zustand 的模块求值时机晚于首绘，天生做不到。
 * 因此真正的实现位于 `styles/themePreference.ts`（纯函数 + 基于
 * `useSyncExternalStore` 的 `useTheme`），本文件只做一层re-export，
 * 让「状态入口在 stores/」与「实现留在 styles/」两个要求同时成立。
 */
export {
  DEFAULT_THEME_PREFERENCE,
  THEME_STORAGE_KEY,
  applyThemeToDom,
  currentThemeMode,
  initialThemeState,
  readStoredPreference,
  resolveTheme,
  setThemePreference,
  subscribeSystemTheme,
  systemPrefersDark,
  useTheme,
  writeStoredPreference,
} from '../styles/themePreference';

export type {
  PreferenceStorage,
  ThemeController,
  ThemePreference,
} from '../styles/themePreference';

/** `{ mode: 三态偏好, resolved: 生效模式, setMode }` 的形状便捷别名。 */
export interface ThemeState {
  mode: ThemePreference;
  resolved: ThemeMode;
  setMode: (pref: ThemePreference) => void;
}
