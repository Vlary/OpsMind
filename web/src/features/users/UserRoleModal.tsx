import { Alert, Form, Modal, Select } from 'antd';
import { useEffect, useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  failureFromError,
  makeFailure,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { message } from '../../components/antdMessage';
import { ROLE_LABELS, ROLE_OPTIONS, type Role } from '../../lib/rbacPermissions';
import { canSubmitRoleChange, roleDescription, type User } from './usersView';

type UserUpdate = components['schemas']['UserUpdate'];

interface UserRoleModalProps {
  open: boolean;
  user: User | null;
  users: readonly User[];
  currentUserId: string;
  onClose: () => void;
  onSaved: (user: User) => void;
}

/** 修改角色（PATCH /api/users/:id）：服务端会在同事务吊销目标全部会话。 */
export default function UserRoleModal({
  open,
  user,
  users,
  currentUserId,
  onClose,
  onSaved,
}: UserRoleModalProps) {
  const [form] = Form.useForm<{ role: Role }>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const selectedRole = Form.useWatch('role', form) ?? user?.role ?? 'auditor';
  const guard = user
    ? canSubmitRoleChange(user, selectedRole, currentUserId, users)
    : { allowed: false, reason: '未选择用户' };

  // 第 9 轮走查 high：Form.useForm 的 store 在弹窗关闭后不会自动清空，
  // antd 的 initialValues 又只在首次挂载生效；先打开 A 再打开 B 时，
  // 弹窗标题已是 B，下拉却仍是 A 的角色，直接保存会把 B 改成 A 的角色。
  // 每次打开或目标用户变化都显式回填目标角色（不能只依赖 initialValues）。
  useEffect(() => {
    if (!open) {
      return;
    }
    form.resetFields();
    form.setFieldsValue({ role: user?.role ?? 'auditor' });
  }, [open, user?.id, user?.role, form]);

  const close = () => {
    if (submitting) {
      return;
    }
    setFailure(null);
    onClose();
  };

  const submit = async () => {
    if (!user || !guard.allowed) {
      return;
    }
    setSubmitting(true);
    setFailure(null);
    try {
      const body: UserUpdate = { role: selectedRole };
      const { data, error, response } = await api.PATCH('/api/users/{id}', {
        params: { path: { id: user.id }, header: writeHeaders() },
        body,
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty user response'));
      }
      message.success(
        `已将 ${user.username} 的角色改为「${ROLE_LABELS[selectedRole]}」，其会话已吊销`,
      );
      onSaved(data);
      onClose();
    } catch (error) {
      setFailure(failureFromError(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      open={open}
      title={user ? `修改角色 · ${user.username}` : '修改角色'}
      okText="保存"
      cancelText="取消"
      okButtonProps={{ disabled: !guard.allowed }}
      confirmLoading={submitting}
      onOk={() => void submit()}
      onCancel={close}
      destroyOnHidden
      centered
    >
      {failure ? <ApiErrorAlert failure={failure} /> : null}
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        title="修改角色会立即生效"
        description="目标账号的全部登录会话会被吊销，进行中的终端/RDP 会断开，待处理审批会被取消。"
      />
      {guard.reason && guard.reason !== '角色未变化' ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          title={guard.reason}
          description="服务端会以 409 拒绝该操作；请先确认还有其他启用中的管理员，或改由他人操作。"
        />
      ) : null}
      <Form
        key={user?.id ?? 'no-user'}
        form={form}
        layout="vertical"
        requiredMark={false}
        initialValues={{ role: user?.role }}
      >
        <Form.Item
          name="role"
          label="角色"
          // 与创建用户弹窗同源：角色职责说明必须同屏可见，否则管理员只看到
          // 四个名字，不知道「审批者不能发起对话」「审计员只读」（评审 medium）。
          extra={roleDescription(selectedRole) || undefined}
          rules={[{ required: true, message: '请选择角色' }]}
        >
          <Select
            options={ROLE_OPTIONS.map(({ value, label }) => ({ value, label }))}
            onChange={() => setFailure(null)}
          />
        </Form.Item>
      </Form>
    </Modal>
  );
}
