import { Empty } from 'antd';
import type { ReactNode } from 'react';

interface EmptyStateProps {
  description?: ReactNode;
  children?: ReactNode;
}

export default function EmptyState({ description = '暂无数据', children }: EmptyStateProps) {
  return <Empty description={description}>{children}</Empty>;
}
