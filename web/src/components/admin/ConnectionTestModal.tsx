import { Alert, Button, Descriptions, Modal, Tag } from 'antd';

import { describeFailure, type ApiFailure } from './apiError';
import { formatDuration } from './format';

export interface ConnectionTestSuccess {
  latencyMs: number;
  lines: { label: string; value: string }[];
  note: string | null;
  /** Telnet 网络设备：明文风险告警（契约 2.6 必须显示）；其余通道 null。 */
  warning?: string | null;
}

interface ConnectionTestModalProps {
  open: boolean;
  title: string;
  success: ConnectionTestSuccess | null;
  error: ApiFailure | null;
  onClose: () => void;
}

// 服务器 / 数据库 / 供应商连接测试的统一结果弹窗：延迟 + 系统信息。
export default function ConnectionTestModal({
  open,
  title,
  success,
  error,
  onClose,
}: ConnectionTestModalProps) {
  return (
    <Modal
      open={open}
      title={title}
      onCancel={onClose}
      destroyOnHidden
      footer={
        <Button type="primary" onClick={onClose}>
          关闭
        </Button>
      }
    >
      {error ? (
        <Alert
          type="error"
          showIcon
          message={describeFailure(error)}
          description={
            <div className="opsmind-admin-meta">
              {[error.code, error.requestId ? `request_id: ${error.requestId}` : null]
                .filter((part): part is string => part !== null)
                .join(' · ')}
            </div>
          }
        />
      ) : null}
      {success ? (
        <div>
          <Alert
            type="success"
            showIcon
            message="连接成功"
            description={<Tag color="green">延迟 {formatDuration(success.latencyMs)}</Tag>}
          />
          {success.lines.length > 0 ? (
            <Descriptions
              size="small"
              column={1}
              bordered
              style={{ marginTop: 16 }}
              items={success.lines.map((line) => ({
                key: line.label,
                label: line.label,
                children: <span className="opsmind-admin-mono">{line.value}</span>,
              }))}
            />
          ) : null}
          {success.warning ? (
            <Alert type="warning" showIcon message={success.warning} style={{ marginTop: 16 }} />
          ) : null}
          {success.note ? (
            <Alert type="info" showIcon message={success.note} style={{ marginTop: 16 }} />
          ) : null}
        </div>
      ) : null}
    </Modal>
  );
}
