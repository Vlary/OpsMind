import { Tag } from 'antd';

const statusMap: Record<string, { color: string; label: string }> = {
  pending: { color: 'gold', label: '待处理' },
  approved: { color: 'green', label: '已批准' },
  denied: { color: 'red', label: '已拒绝' },
  expired: { color: 'default', label: '已过期' },
  started: { color: 'blue', label: '执行中' },
  awaiting_approval: { color: 'gold', label: '待审批' },
  succeeded: { color: 'green', label: '成功' },
  failed: { color: 'red', label: '失败' },
  awaiting_user: { color: 'blue', label: '待用户回答' },
  complete: { color: 'green', label: '完成' },
  error: { color: 'red', label: '错误' },
};

export interface StatusTagProps {
  status: string;
  /** 覆盖默认标签文案；不传时按 status 映射。颜色仍由 status 决定。 */
  label?: string | undefined;
}

export default function StatusTag({ status, label }: StatusTagProps) {
  const hit = statusMap[status] ?? { color: 'default', label: status };
  return <Tag color={hit.color}>{label ?? hit.label}</Tag>;
}
