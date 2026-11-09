import { Modal } from 'antd';
import type { ReactNode } from 'react';

interface ConfirmActionModalProps {
  open: boolean;
  title: string;
  description: ReactNode;
  okText: string;
  loading: boolean;
  danger: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

// 危险/敏感操作的统一二次确认框（与 ConfirmDeleteModal 区分：可自定义按钮文案）。
export default function ConfirmActionModal({
  open,
  title,
  description,
  okText,
  loading,
  danger,
  onConfirm,
  onCancel,
}: ConfirmActionModalProps) {
  return (
    <Modal
      open={open}
      title={title}
      okText={okText}
      cancelText="取消"
      okButtonProps={{ danger }}
      confirmLoading={loading}
      onOk={onConfirm}
      onCancel={onCancel}
      destroyOnHidden
      centered
    >
      {description}
    </Modal>
  );
}
