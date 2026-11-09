import { useMutation } from '@tanstack/react-query';
import { Alert, Form, Input, Modal } from 'antd';

import { api } from '../../api/client';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  describeChangePasswordFailure,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';

interface ChangePasswordValues {
  oldPassword: string;
  newPassword: string;
  confirmPassword: string;
}

interface ChangePasswordModalProps {
  open: boolean;
  onClose: () => void;
  /** 密码修改成功（全部会话已被服务端吊销）：调用方负责清缓存并回到登录页。 */
  onSuccess: () => void;
}

// 后端 bcrypt 明文上限按字节计（internal/auth maxPasswordBytes=72），
// 中文等多字节字符需按 UTF-8 字节数校验，避免提交必然被拒的值。
function passwordByteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

// 顶栏头像下拉「修改密码」弹窗：校验旧密码 → 设置新密码。
// 成功后服务端吊销全部会话，本组件只上报 onSuccess，导航与清缓存由
// 调用方（AppShell）处理。
export default function ChangePasswordModal({
  open,
  onClose,
  onSuccess,
}: ChangePasswordModalProps) {
  const [form] = Form.useForm<ChangePasswordValues>();

  const changeMutation = useMutation({
    mutationFn: async (values: ChangePasswordValues) => {
      const { error, response } = await api.POST('/api/auth/password', {
        body: { old_password: values.oldPassword, new_password: values.newPassword },
        params: { header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      // 成功固定 204 无 body；能走到这里即成功。
      return true;
    },
    onSuccess: () => {
      form.resetFields();
      onSuccess();
    },
  });

  const failure = changeMutation.error ? failureFromError(changeMutation.error) : null;

  return (
    <Modal
      open={open}
      title="修改密码"
      okText="确认修改"
      cancelText="取消"
      confirmLoading={changeMutation.isPending}
      destroyOnHidden
      onCancel={() => {
        changeMutation.reset();
        onClose();
      }}
      onOk={() => {
        changeMutation.reset();
        form.submit();
      }}
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="修改成功后当前所有登录会话都会失效，需要用新密码重新登录。"
      />
      {/* AUTH_INVALID 在改密场景必然是旧密码校验失败（表单无用户名字段），
          describeChangePasswordFailure 把文案改写为「旧密码不正确」；错误码与
          request_id 仍由 ApiErrorAlert 的 description 小字保留，便于排查。 */}
      {failure ? (
        <ApiErrorAlert failure={failure} describe={describeChangePasswordFailure} />
      ) : null}
      <Form<ChangePasswordValues>
        form={form}
        layout="vertical"
        requiredMark={false}
        onFinish={(values) => changeMutation.mutate(values)}
      >
        <Form.Item
          name="oldPassword"
          label="旧密码"
          rules={[
            { required: true, message: '请输入旧密码' },
            { max: 256, message: '密码不能超过 256 个字符' },
          ]}
        >
          <Input.Password placeholder="请输入当前密码" autoComplete="current-password" autoFocus />
        </Form.Item>
        <Form.Item
          name="newPassword"
          label="新密码"
          rules={[
            { required: true, message: '请输入新密码' },
            {
              validator: (_rule, value: string | undefined) => {
                if (value === undefined || value === '') {
                  return Promise.resolve();
                }
                if (passwordByteLength(value) > 72) {
                  return Promise.reject(new Error('新密码不能超过 72 字节（中文占 3 字节）'));
                }
                return Promise.resolve();
              },
            },
          ]}
        >
          <Input.Password placeholder="请输入新密码" autoComplete="new-password" />
        </Form.Item>
        <Form.Item
          name="confirmPassword"
          label="确认新密码"
          dependencies={['newPassword']}
          rules={[
            { required: true, message: '请再次输入新密码' },
            ({ getFieldValue }) => ({
              validator: (_rule, value: string | undefined) => {
                if (value === undefined || value === '' || value === getFieldValue('newPassword')) {
                  return Promise.resolve();
                }
                return Promise.reject(new Error('两次输入的新密码不一致'));
              },
            }),
          ]}
        >
          <Input.Password placeholder="请再次输入新密码" autoComplete="new-password" />
        </Form.Item>
      </Form>
    </Modal>
  );
}
