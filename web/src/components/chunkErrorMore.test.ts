import { describe, expect, it } from 'vitest';

import { isChunkLoadError, shouldAutoReload } from './chunkError';

// 懒加载 chunk 自愈（chunkError.ts）的正则分支矩阵与一次性标记状态机。
// 与 routeErrorBoundary.test.ts 的基础行为互补：逐文案分支 + storage 序列。

function memoryStorage(): Pick<Storage, 'getItem' | 'setItem'> {
  const map = new Map<string, string>();
  return {
    getItem: (key) => map.get(key) ?? null,
    setItem: (key, value) => {
      map.set(key, value);
    },
  };
}

describe('isChunkLoadError 文案矩阵', () => {
  it('Vite：Importing a module script failed（Firefox）', () => {
    expect(isChunkLoadError(new Error('Importing a module script failed.'))).toBe(true);
  });

  it('Firefox 新版：dynamically imported module', () => {
    expect(isChunkLoadError(new Error('error loading dynamically imported module'))).toBe(true);
  });

  it('Failed to fetch（Firefox 动态 import）', () => {
    expect(isChunkLoadError(new Error('Loading chunk 3 failed.\nerror: Failed to fetch'))).toBe(
      true,
    );
  });

  it('Loading chunk N failed：N 为任意数字', () => {
    expect(isChunkLoadError(new Error('Loading chunk 0 failed.'))).toBe(true);
    expect(isChunkLoadError(new Error('Loading chunk 123 failed.'))).toBe(true);
  });

  it('Loading CSS chunk（样式分包失败）', () => {
    expect(isChunkLoadError(new Error('Loading CSS chunk 12 failed.'))).toBe(true);
  });

  it('importing a module script（Safari）', () => {
    expect(isChunkLoadError(new Error('importing a module script failed.'))).toBe(true);
  });

  it('普通渲染错误不命中', () => {
    expect(isChunkLoadError(new Error('Cannot read properties of undefined'))).toBe(false);
    expect(isChunkLoadError(new Error('Rendered fewer hooks than expected'))).toBe(false);
    expect(isChunkLoadError(new Error('网络异常，请检查连接后重试'))).toBe(false);
    expect(isChunkLoadError(new Error(''))).toBe(false);
  });

  it('chunk 相关但措辞不同（如 ChunkLoadError 类名）不靠类名判定', () => {
    // 实现按 message 正则判定，不含类名信息。
    const error = new Error('something else');
    error.name = 'ChunkLoadError';
    expect(isChunkLoadError(error)).toBe(false);
  });
});

describe('shouldAutoReload 一次性自愈状态机', () => {
  it('chunk 错误 + 空 storage：返回 true 并写入时间戳标记', () => {
    const storage = memoryStorage();
    expect(shouldAutoReload(new Error('Loading chunk 5 failed.'), storage)).toBe(true);
    expect(storage.getItem('aiops.chunk-error-reloaded')).not.toBeNull();
  });

  it('已自愈过（标记存在）：同一错误不再触发', () => {
    const storage = memoryStorage();
    expect(shouldAutoReload(new Error('Loading chunk 5 failed.'), storage)).toBe(true);
    expect(shouldAutoReload(new Error('Loading chunk 7 failed.'), storage)).toBe(false);
    expect(shouldAutoReload(new Error('Failed to fetch'), storage)).toBe(false);
  });

  it('非 chunk 错误：永不自愈且不写标记', () => {
    const storage = memoryStorage();
    expect(shouldAutoReload(new Error('普通错误'), storage)).toBe(false);
    expect(storage.getItem('aiops.chunk-error-reloaded')).toBeNull();
  });

  it('storage 抛异常（隐私模式）：静默放弃自愈', () => {
    const throwingStorage: Pick<Storage, 'getItem' | 'setItem'> = {
      getItem: () => {
        throw new Error('SecurityError');
      },
      setItem: () => {
        throw new Error('SecurityError');
      },
    };
    expect(shouldAutoReload(new Error('Loading chunk 5 failed.'), throwingStorage)).toBe(false);
  });

  it('getItem 抛异常但错误非 chunk：先走 chunk 判定，返回 false', () => {
    const throwingStorage: Pick<Storage, 'getItem' | 'setItem'> = {
      getItem: () => {
        throw new Error('boom');
      },
      setItem: () => undefined,
    };
    expect(shouldAutoReload(new Error('随便什么错误'), throwingStorage)).toBe(false);
  });

  it('连续三次调用（首次自愈 + 两次拒绝）：自愈恰好一次', () => {
    const storage = memoryStorage();
    const error = new Error('Loading chunk 9 failed.');
    const results = [
      shouldAutoReload(error, storage),
      shouldAutoReload(error, storage),
      shouldAutoReload(error, storage),
    ];
    expect(results).toEqual([true, false, false]);
  });
});
