import { useMutation } from '@tanstack/react-query';
import { Alert, Form, Input, Modal, Select } from 'antd';
import { useEffect, useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  applyFieldErrors,
  failureFromError,
  makeFailure,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { message } from '../../components/antdMessage';
import { ROLE_DESCRIPTIONS, ROLE_OPTIONS, type Role } from '../../lib/rbacPermissions';

type User = components['schemas']['User'];

interface UserFormValues {
  username: string;
  email: string;
  role: Role;
  display_name?: string;
}

interface UserCreateModalProps {
  open: boolean;
  onClose: () => void;
  onCreated: (user: User) => void;
}

const USER_FIELDS = ['username', 'email', 'role', 'display_name'];

/**
 * 创建用户（CONTRACT.md 12.6 POST /api/users）。
 *
 * 请求不含密码：服务端生成一次性 invite token 并经 SMTP/IM 通道投递到
 * 目标 email（响应不返回 token）。投递通道缺失时 503 DELIVERY_UNAVAILABLE，
 * 直接展示中文说明。
 */
export default function UserCreateModal({ open, onClose, onCreated }: UserCreateModalProps) {
  const [form] = Form.useForm<UserFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const selectedRole = Form.useWatch('role', form) ?? 'auditor';

  // 第 9 轮走查 high：取消后重开必须是一张空白表单，不能残留上一条草稿
  //（用户名/邮箱/角色）。Form.useForm 的 store 在 destroyOnHidden 重挂载后
  // 仍保留旧值，initialValues 不会覆盖它，因此在每次 open 时显式 reset。
  useEffect(() => {
    if (!open) {
      return;
    }
    form.resetFields();
  }, [open, form]);

  const createMutation = useMutation({
    mutationFn: async (values: UserFormValues) => {
      const body = {
        username: values.username.trim(),
        email: values.email.trim(),
        role: values.role,
        ...(values.display_name?.trim() ? { display_name: values.display_name.trim() } : {}),
      };
      const { data, error, response } = await api.POST('/api/users', {
        body,
        params: { header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty user response'));
      }
      return data;
    },
    onSuccess: (user) => {
      // 第 9 轮走查 medium：同名未激活账号会用本次请求的邮箱更新账号并
      // 投递到该地址，toast 必须显示实际收件地址而不是笼统的「目标邮箱」。
      message.success(`用户已创建（同名未激活账号则已重发邀请），邀请链接已发送到 ${user.email}`);
      form.resetFields();
      setFailure(null);
      onCreated(user);
      onClose();
    },
    onError: (error) => {
      const next = failureFromError(error);
      setFailure(next);
      applyFieldErrors(next.details, USER_FIELDS, (items) =>
        form.setFields(items as Parameters<typeof form.setFields>[0]),
      );
    },
  });

  const close = () => {
    if (createMutation.isPending) {
      return;
    }
    setFailure(null);
    onClose();
  };

  return (
    <Modal
      open={open}
      title="创建用户"
      okText="创建并发送邀请"
      cancelText="取消"
      confirmLoading={createMutation.isPending}
      onOk={() => form.submit()}
      onCancel={close}
      destroyOnHidden
      centered
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="不设置初始密码"
        description="系统会向目标邮箱发送一次性邀请链接，由本人设置密码；链接 30 分钟内有效且仅可使用一次。若用户名与未激活账号重名，将重发邀请并使用本次填写的邮箱。"
      />
      {failure ? <ApiErrorAlert failure={failure} /> : null}
      <Form<UserFormValues>
        form={form}
        layout="vertical"
        requiredMark={false}
        initialValues={{ username: '', email: '', role: 'auditor', display_name: '' }}
        onFinish={(values) => {
          setFailure(null);
          createMutation.mutate(values);
        }}
      >
        <Form.Item
          name="username"
          label="用户名"
          rules={[
            { required: true, message: '请输入用户名' },
            {
              pattern: /^[a-z0-9][a-z0-9._-]{0,63}$/,
              message:
                '小写字母或数字开头，仅可包含小写字母、数字、点、下划线、连字符（最多 64 位）',
            },
          ]}
        >
          <Input autoComplete="off" placeholder="例如 zhangsan" />
        </Form.Item>
        <Form.Item
          name="email"
          label="邮箱"
          rules={[
            { required: true, message: '请输入邮箱' },
            { type: 'email', message: '邮箱格式不正确' },
          ]}
        >
          <Input autoComplete="off" placeholder="邀请与密码重置链接将发送到该邮箱" />
        </Form.Item>
        <Form.Item
          name="role"
          label="角色"
          rules={[{ required: true, message: '请选择角色' }]}
          extra={ROLE_DESCRIPTIONS[selectedRole]}
        >
          <Select
            options={ROLE_OPTIONS.map(({ value, label }) => ({
              value,
              label,
              title: ROLE_DESCRIPTIONS[value],
            }))}
          />
        </Form.Item>
        <Form.Item name="display_name" label="显示名（可选）">
          <Input autoComplete="off" maxLength={100} />
        </Form.Item>
      </Form>
    </Modal>
  );
}
