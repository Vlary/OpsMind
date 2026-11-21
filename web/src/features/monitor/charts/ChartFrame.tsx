// 图表外壳：标题/副标题/右上角切换器 + 加载态、错误态、空态。
//
// 每个图表都必须四态齐全（本轮图表组件约束 3）。错误内容由页面传入
// （通常是 QueryErrorState，保留 API 错误码与 request_id），本组件只负责
// 占位高度与对齐；空态与加载态使用与页面一致的 antd 组件。

import { Skeleton } from 'antd';
import type { ReactNode } from 'react';

export interface ChartFrameProps {
  title?: ReactNode;
  subtitle?: ReactNode;
  /** 右上角的时间范围/维度切换。 */
  actions?: ReactNode;
  loading?: boolean;
  /** 错误态内容（字符串或 ErrorBanner/QueryErrorState 元素）。 */
  error?: ReactNode;
  /** 空态（数据为空；与 loading/error 互斥，优先级 loading > error > empty）。 */
  empty?: boolean;
  emptyMessage?: ReactNode;
  /** 四态占位高度，需与图表绘制高度一致。 */
  height: number;
  children: ReactNode;
}

export default function ChartFrame({
  title,
  subtitle,
  actions,
  loading = false,
  error,
  empty = false,
  emptyMessage = '暂无数据',
  height,
  children,
}: ChartFrameProps) {
  const hasHeader = Boolean(title || subtitle || actions);
  const hasError = error !== undefined && error !== null && error !== false;
  return (
    <section
      style={{ display: 'flex', flexDirection: 'column', gap: 8, width: '100%' }}
      aria-busy={loading || undefined}
    >
      {hasHeader ? (
        <header
          style={{
            display: 'flex',
            alignItems: 'flex-start',
            justifyContent: 'space-between',
            gap: 12,
            flexWrap: 'wrap',
          }}
        >
          <div style={{ minWidth: 0 }}>
            {title ? <div style={{ fontWeight: 600 }}>{title}</div> : null}
            {subtitle ? (
              <div style={{ fontSize: 12, color: 'var(--color-text-tertiary)' }}>{subtitle}</div>
            ) : null}
          </div>
          {actions ? <div style={{ flexShrink: 0 }}>{actions}</div> : null}
        </header>
      ) : null}

      {loading ? (
        <div style={{ minHeight: height }} data-testid="chart-loading">
          <Skeleton active title={{ width: 160 }} paragraph={{ rows: 4 }} />
        </div>
      ) : hasError ? (
        <div
          style={{ minHeight: height, display: 'flex', alignItems: 'center' }}
          data-testid="chart-error"
        >
          {typeof error === 'string' ? (
            <div role="alert" style={{ color: 'var(--color-danger)', fontSize: 13 }}>
              {error}
            </div>
          ) : (
            error
          )}
        </div>
      ) : empty ? (
        <div
          role="status"
          data-testid="chart-empty"
          style={{
            minHeight: height,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            color: 'var(--color-text-tertiary)',
            fontSize: 12,
            textAlign: 'center',
            padding: '0 12px',
          }}
        >
          {emptyMessage}
        </div>
      ) : (
        children
      )}
    </section>
  );
}
