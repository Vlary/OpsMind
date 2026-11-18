// 00012：邀请/重置 token 落地页（/invite?token=...&kind=invite|reset）。
//
// 此前前端没有接受邀请页，邀请链接即使送达也没有可用界面（评审 high）。
// 公开路由：不需要登录；Origin 由全局中间件校验，token 30 分钟单次。
// 兑换成功后跳登录页，由本人用刚设置的密码登录。

import { LockOutlined } from '@ant-design/icons';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, ConfigProvider, Form, Input, Typography } from 'antd';
import { useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import BrandMark from '../../components/BrandMark';
import {
  ApiRequestError,
  describeInviteFailure,
  failureFromError,
  makeFailure,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import { themeConfig } from '../../styles/theme';
import { useTheme } from '../../styles/themePreference';
import '../../styles/admin.css';

interface InviteFormValues {
  new_password: string;
  confirm: string;
  display_name?: string;
}

export default function InviteAcceptPage() {
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form] = Form.useForm<InviteFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const { resolved: theme } = useTheme();
  const token = (searchParams.get('token') ?? '').trim();
  const kind = searchParams.get('kind') === 'reset' ? 'reset' : 'invite';

  const acceptMutation = useMutation({
    mutationFn: async (values: InviteFormValues) => {
      const { data, error, response } = await api.POST('/api/auth/invite/accept', {
        body: {
          token,
          new_password: values.new_password,
          ...(values.display_name?.trim() ? { display_name: values.display_name.trim() } : {}),
        },
        params: { header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      queryClient.removeQueries({ queryKey: qk.me() });
      navigate('/login', { replace: true, state: { passwordSet: true } });
    },
    onError: (error) => setFailure(failureFromError(error)),
  });

  if (!token) {
    return (
      <ConfigProvider theme={themeConfig(theme)}>
        <div className="app-center">
          <Card className="opsmind-login-card">
            <Alert
              type="error"
              showIcon
              message="链接无效或已过期"
              description="邀请/重置链接缺少 token，请让管理员重新发送。"
            />
            <div style={{ marginTop: 16 }}>
              <Link to="/login">返回登录</Link>
            </div>
          </Card>
        </div>
      </ConfigProvider>
    );
  }

  return (
    <ConfigProvider theme={themeConfig(theme)}>
      <div className="opsmind-login-page">
        <Card className="opsmind-login-card" title={null}>
          <div className="opsmind-login-brand">
            <div className="opsmind-login-logo">
              <BrandMark size={24} decorative />
            </div>
            <div>
              <Typography.Title level={4} style={{ margin: 0 }}>
                云枢 OpsMind
              </Typography.Title>
              <Typography.Text type="secondary">
                {kind === 'reset' ? '重置密码' : '接受邀请'}
              </Typography.Text>
            </div>
          </div>
          <Typography.Paragraph type="secondary" style={{ marginTop: 16 }}>
            {kind === 'reset'
              ? '设置新密码后，原有会话会立即失效。'
              : '设置密码后即可登录；邀请链接 30 分钟内有效且仅可使用一次。'}
          </Typography.Paragraph>
          {failure ? (
            <Alert
              type="error"
              showIcon
              style={{ marginBottom: 16 }}
              message={describeInviteFailure(failure)}
              description={failure.requestId ? `request_id: ${failure.requestId}` : null}
            />
          ) : null}
          {failure?.code === 'SELF_REDEEM_FORBIDDEN' ? (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 16 }}
              message="下一步"
              description="请把链接转发给受邀者，由其在自己的设备/网络打开并设置密码；管理员本人无法在同一出口 IP 兑换。若你正在本机验证流程，可换用其他出口网络后重试。"
            />
          ) : null}
          <Form<InviteFormValues>
            form={form}
            layout="vertical"
            requiredMark={false}
            onFinish={(values) => {
              setFailure(null);
              if (values.new_password !== values.confirm) {
                setFailure(makeFailure('VALIDATION_FAILED', '两次输入的密码不一致'));
                return;
              }
              acceptMutation.mutate(values);
            }}
          >
            <Form.Item
              name="new_password"
              label="新密码"
              rules={[
                { required: true, message: '请输入新密码' },
                { min: 1, max: 72, message: '密码长度需在 1..72 字节之间' },
              ]}
            >
              <Input.Password prefix={<LockOutlined />} autoComplete="new-password" />
            </Form.Item>
            <Form.Item
              name="confirm"
              label="确认新密码"
              dependencies={['new_password']}
              rules={[
                { required: true, message: '请再次输入新密码' },
                ({ getFieldValue }) => ({
                  validator(_, value: string) {
                    if (!value || getFieldValue('new_password') === value) {
                      return Promise.resolve();
                    }
                    return Promise.reject(new Error('两次输入的密码不一致'));
                  },
                }),
              ]}
            >
              <Input.Password prefix={<LockOutlined />} autoComplete="new-password" />
            </Form.Item>
            {kind === 'invite' ? (
              <Form.Item name="display_name" label="显示名（可选）">
                <Input autoComplete="off" maxLength={100} />
              </Form.Item>
            ) : null}
            <Button type="primary" htmlType="submit" block loading={acceptMutation.isPending}>
              {/* 兑换成功只会跳登录页，不会自动登录——按钮文案不得承诺
                  「并登录」（RBAC 走查 sprint 2）。 */}
              {kind === 'reset' ? '设置新密码' : '设置密码'}
            </Button>
          </Form>
          <div style={{ marginTop: 12 }}>
            <Link to="/login">返回登录</Link>
          </div>
        </Card>
      </div>
    </ConfigProvider>
  );
}
