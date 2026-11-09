import { Component, type ErrorInfo, type ReactNode } from 'react';

import { isChunkLoadError, shouldAutoReload } from './chunkError';

// RouteErrorBoundary 兜底路由树渲染期错误，核心场景是 React.lazy 的 chunk
// 加载失败（发版后旧 hash 404 / 弱网）：lazy 会缓存 rejected promise，
// 不加边界时错误沿渲染树抛出导致整树卸载白屏，且每次导航重抛同一错误。
//
// chunk 类错误额外做一次性自动整页 reload（业界 Vite 常规做法）：发版后
// 旧 hash chunk 404，刷新即取新入口 HTML 与新 hash 的资源，是清除 lazy
// rejected 缓存的唯一可靠方式；sessionStorage 标记保证只自愈一次，刷新
// 后仍失败则停留 fallback 由用户手动处理，避免刷新风暴。
// 判定与决策逻辑在 chunkError.ts（可单测）。
//
// fallback 刻意只用原生元素：此时 antd/图标 chunk 本身可能加载失败。
interface State {
  error: Error | null;
}

export default class RouteErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // 保留客户端侧排查线索；不上报远端（无遥测）。
    console.error('RouteErrorBoundary caught:', error, info.componentStack);
    if (shouldAutoReload(error, window.sessionStorage)) {
      window.location.reload();
    }
  }

  render() {
    if (!this.state.error) {
      return this.props.children;
    }
    const isChunkError = isChunkLoadError(this.state.error);
    return (
      <div
        style={{
          minHeight: '100vh',
          display: 'grid',
          placeItems: 'center',
          padding: 24,
          fontFamily: 'system-ui, sans-serif',
        }}
      >
        <div style={{ maxWidth: 480, textAlign: 'center' }}>
          <h2 style={{ fontSize: 18, margin: '0 0 8px' }}>页面加载失败</h2>
          <p style={{ color: 'var(--color-text-secondary)', fontSize: 14, margin: '0 0 16px' }}>
            {isChunkError ? '应用已更新或网络不稳定，页面资源未能加载。' : '页面出现意外错误。'}
          </p>
          <button
            type="button"
            onClick={() => window.location.reload()}
            style={{
              padding: '6px 18px',
              fontSize: 14,
              cursor: 'pointer',
              borderRadius: 6,
              border: '1px solid var(--color-border-strong)',
              background: 'var(--color-bg-container)',
            }}
          >
            重新加载
          </button>
        </div>
      </div>
    );
  }
}
