import { Skeleton } from 'antd';

import ErrorBanner from '../ErrorBanner';
import { describeFailure, failureFromError } from './apiError';

export function AdminLoading({ rows = 5 }: { rows?: number }) {
  return (
    <div style={{ padding: '8px 0' }}>
      <Skeleton active title={{ width: 180 }} paragraph={{ rows }} />
    </div>
  );
}

export function QueryErrorState({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const failure = failureFromError(error);
  return (
    <ErrorBanner
      code={failure.code}
      message={describeFailure(failure)}
      requestId={failure.requestId}
      onRetry={onRetry}
    />
  );
}
