import { describe, expect, it } from 'vitest';

import { isChunkLoadError, shouldAutoReload } from './chunkError';

function memoryStorage(initial: Record<string, string> = {}): Pick<
  Storage,
  'getItem' | 'setItem'
> & {
  data: Map<string, string>;
} {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => {
      data.set(k, v);
    },
  };
}

describe('isChunkLoadError', () => {
  it('识别各浏览器的懒加载失败文案', () => {
    expect(
      isChunkLoadError(
        new Error('Failed to fetch dynamically imported module: https://x/ChatPage.js'),
      ),
    ).toBe(true);
    expect(isChunkLoadError(new Error('Importing a module script failed.'))).toBe(true);
    expect(isChunkLoadError(new Error('Loading chunk 42 failed.'))).toBe(true);
    expect(isChunkLoadError(new Error('Loading CSS chunk 7 failed.'))).toBe(true);
  });

  it('非 chunk 错误不触发自愈（普通渲染错误保持 fallback）', () => {
    expect(isChunkLoadError(new Error('Cannot read properties of undefined'))).toBe(false);
    expect(isChunkLoadError(new Error('unterminated regular expression'))).toBe(false);
  });
});

describe('shouldAutoReload', () => {
  it('chunk 错误首次触发自愈并写入一次性标记', () => {
    const storage = memoryStorage();
    expect(
      shouldAutoReload(new Error('Failed to fetch dynamically imported module'), storage),
    ).toBe(true);
    expect(storage.data.size).toBe(1);
  });

  it('同一会话只自愈一次：已有标记时不再刷新（防刷新风暴）', () => {
    const storage = memoryStorage({ 'aiops.chunk-error-reloaded': '1' });
    expect(
      shouldAutoReload(new Error('Failed to fetch dynamically imported module'), storage),
    ).toBe(false);
  });

  it('非 chunk 错误永不自动刷新且不写标记', () => {
    const storage = memoryStorage();
    expect(shouldAutoReload(new Error('boom'), storage)).toBe(false);
    expect(storage.data.size).toBe(0);
  });

  it('storage 抛异常（隐私模式）时静默放弃自动刷新', () => {
    const broken = {
      getItem: (): string => {
        throw new DOMException('denied');
      },
      setItem: (): void => {
        throw new DOMException('denied');
      },
    };
    expect(shouldAutoReload(new Error('Failed to fetch dynamically imported module'), broken)).toBe(
      false,
    );
  });
});
