import { Alert, Button } from 'antd';

import { describeFailure, type ApiFailure } from './apiError';

interface ApiErrorAlertProps {
  failure: ApiFailure;
  /** 文案渲染函数，默认 describeFailure；需场景化改写错误码文案的调用方传入。 */
  describe?: (failure: ApiFailure) => string;
  closable?: boolean;
  onClose?: () => void;
  onRetry?: () => void;
}

// 资源管理页统一的错误条：第一行中文说明，错误码与 request_id 降级为
// description 同行小字，不再把大写英文错误码（AUTH_INVALID 等）当标题。
export default function ApiErrorAlert({
  failure,
  describe = describeFailure,
  closable = false,
  onClose,
  onRetry,
}: ApiErrorAlertProps) {
  // 00012：通用 403 已在 api 层归一化为「无权访问」，不再展示 FORBIDDEN
  // 错误码；专用 403 码（ORIGIN_FORBIDDEN 等）仍保留在 description。
  const meta = [
    failure.code !== 'FORBIDDEN' ? failure.code : null,
    failure.requestId ? `request_id: ${failure.requestId}` : null,
  ]
    .filter((part): part is string => part !== null)
    .join(' · ');

  return (
    <Alert
      type="error"
      showIcon
      message={describe(failure)}
      {...(meta ? { description: <div className="opsmind-admin-meta">{meta}</div> } : {})}
      closable={closable}
      {...(onClose ? { onClose } : {})}
      action={
        onRetry ? (
          <Button size="small" onClick={onRetry}>
            重试
          </Button>
        ) : null
      }
      style={{ marginBottom: 16 }}
    />
  );
}
