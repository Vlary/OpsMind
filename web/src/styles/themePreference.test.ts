import { describe, expect, it } from 'vitest';

import {
  DEFAULT_THEME_PREFERENCE,
  THEME_STORAGE_KEY,
  initialThemeState,
  readStoredPreference,
  resolveTheme,
  writeStoredPreference,
  type PreferenceStorage,
  type ThemePreference,
} from './themePreference';

/** 内存 Storage，便于断言读写与异常路径。 */
function memoryStorage(seed?: Record<string, string>): PreferenceStorage & {
  readonly data: Record<string, string>;
} {
  const data: Record<string, string> = { ...seed };
  return {
    data,
    getItem: (key: string) => (key in data ? (data[key] as string) : null),
    setItem: (key: string, value: string) => {
      data[key] = value;
    },
  };
}

const throwingStorage: PreferenceStorage = {
  getItem: () => {
    throw new Error('SecurityError: localStorage is disabled');
  },
  setItem: () => {
    throw new Error('SecurityError: localStorage is disabled');
  },
};

describe('resolveTheme（F04：三态 × 系统明暗全矩阵）', () => {
  const cases: Array<[ThemePreference, boolean, 'light' | 'dark']> = [
    ['light', true, 'light'],
    ['light', false, 'light'],
    ['dark', true, 'dark'],
    ['dark', false, 'dark'],
    ['system', true, 'dark'],
    ['system', false, 'light'],
  ];

  for (const [pref, systemDark, expected] of cases) {
    it(`${pref} + 系统${systemDark ? '暗' : '亮'} → ${expected}`, () => {
      expect(resolveTheme(pref, systemDark)).toBe(expected);
    });
  }
});

describe('readStoredPreference', () => {
  it('缺失键回落默认浅色', () => {
    expect(readStoredPreference(memoryStorage())).toBe('light');
    expect(DEFAULT_THEME_PREFERENCE).toBe('light');
  });

  it('storage 为 null / undefined 回落浅色', () => {
    expect(readStoredPreference(null)).toBe('light');
    expect(readStoredPreference(undefined)).toBe('light');
  });

  it('localStorage 抛异常（隐私模式）回落浅色', () => {
    expect(readStoredPreference(throwingStorage)).toBe('light');
  });

  it.each(['', 'Light', 'DARK', 'auto', 'system ', '0', '{}'])('非法值 %j 一律回落浅色', (raw) => {
    expect(readStoredPreference(memoryStorage({ [THEME_STORAGE_KEY]: raw }))).toBe('light');
  });

  it.each<ThemePreference>(['light', 'dark', 'system'])('合法值 %s 原样读出', (pref) => {
    expect(readStoredPreference(memoryStorage({ [THEME_STORAGE_KEY]: pref }))).toBe(pref);
  });

  it('契约写死的键名不得漂移', () => {
    expect(THEME_STORAGE_KEY).toBe('opsmind.theme');
  });
});

describe('writeStoredPreference', () => {
  it('写入后读回一致（三态逐一）', () => {
    for (const pref of ['light', 'dark', 'system'] as const) {
      const storage = memoryStorage();
      writeStoredPreference(storage, pref);
      expect(storage.data[THEME_STORAGE_KEY]).toBe(pref);
      expect(readStoredPreference(storage)).toBe(pref);
    }
  });

  it('写入抛异常时静默（不中断交互）', () => {
    expect(() => writeStoredPreference(throwingStorage, 'dark')).not.toThrow();
  });

  it('storage 为 null 时静默', () => {
    expect(() => writeStoredPreference(null, 'dark')).not.toThrow();
  });
});

describe('initialThemeState（首绘前解析，与 index.html 内联脚本同语义）', () => {
  it('默认浅色 + 系统亮 → light/light', () => {
    expect(initialThemeState(memoryStorage(), false)).toEqual({
      preference: 'light',
      resolved: 'light',
    });
  });

  it('system + 系统暗 → system/dark', () => {
    expect(initialThemeState(memoryStorage({ [THEME_STORAGE_KEY]: 'system' }), true)).toEqual({
      preference: 'system',
      resolved: 'dark',
    });
  });

  it('显式 dark + 系统亮 → dark/dark（偏好优先于系统）', () => {
    expect(initialThemeState(memoryStorage({ [THEME_STORAGE_KEY]: 'dark' }), false)).toEqual({
      preference: 'dark',
      resolved: 'dark',
    });
  });

  it('存储不可用 + 系统暗 → 仍按 light（隐私模式兜底）', () => {
    expect(initialThemeState(throwingStorage, true)).toEqual({
      preference: 'light',
      resolved: 'light',
    });
  });
});
