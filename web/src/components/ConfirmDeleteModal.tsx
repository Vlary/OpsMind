import { Modal } from 'antd';

interface ConfirmDeleteModalProps {
  open: boolean;
  title: string;
  content?: string;
  loading?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

export default function ConfirmDeleteModal({
  open,
  title,
  content = '此操作不可撤销。',
  loading = false,
  onConfirm,
  onCancel,
}: ConfirmDeleteModalProps) {
  return (
    <Modal
      open={open}
      title={title}
      okText="删除"
      cancelText="取消"
      okButtonProps={{ danger: true }}
      confirmLoading={loading}
      onOk={onConfirm}
      onCancel={onCancel}
    >
      {content}
    </Modal>
  );
}
