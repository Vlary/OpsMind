import { Alert, Button } from 'antd';

interface ErrorBannerProps {
  code: string;
  /** 面向用户的中文提示（describeError / live.error.message 的结果）。 */
  message: string;
  requestId?: string;
  onRetry?: () => void;
}

// 统一错误横幅：第一行是中文文案，错误码与 request_id 降级为同行的
// description 小字，不再把大写英文错误码（AUTH_INVALID 等）当标题展示。
export default function ErrorBanner({ code, message, requestId, onRetry }: ErrorBannerProps) {
  const headline = message.trim() !== '' ? message : code;
  const meta = [code, requestId ? `request_id: ${requestId}` : null]
    .filter((part): part is string => part !== null)
    .join(' · ');
  return (
    <Alert
      type="error"
      showIcon
      message={headline}
      description={<div style={{ opacity: 0.7, fontSize: 12, wordBreak: 'break-all' }}>{meta}</div>}
      action={
        onRetry ? (
          <Button size="small" onClick={() => onRetry()}>
            重试
          </Button>
        ) : null
      }
    />
  );
}
