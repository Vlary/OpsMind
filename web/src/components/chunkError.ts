const CHUNK_RELOAD_KEY = 'aiops.chunk-error-reloaded';

/** 判定是否为懒加载 chunk 失败类错误（Vite/浏览器多家文案）。 */
export function isChunkLoadError(error: Error): boolean {
  return /dynamically imported module|Failed to fetch|importing a module script|Loading chunk \d+ failed|Loading CSS chunk/i.test(
    error.message,
  );
}

/**
 * 自愈决策：chunk 错误且本会话尚未自愈过时返回 true，并写入一次性标记。
 * storage 参数化便于单测注入；storage 抛异常（隐私模式等）时不自动刷新。
 */
export function shouldAutoReload(
  error: Error,
  storage: Pick<Storage, 'getItem' | 'setItem'>,
): boolean {
  if (!isChunkLoadError(error)) {
    return false;
  }
  try {
    if (storage.getItem(CHUNK_RELOAD_KEY) !== null) {
      return false;
    }
    storage.setItem(CHUNK_RELOAD_KEY, String(Date.now()));
    return true;
  } catch {
    return false;
  }
}
