import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, ConfigProvider, Form, Input, Spin, Typography } from 'antd';
import { Navigate, useLocation, useNavigate, useSearchParams } from 'react-router-dom';

import { themeConfig } from '../../styles/theme';
import { useTheme } from '../../styles/themePreference';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import BrandMark from '../../components/BrandMark';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  describeLoginFailure,
  failureFromError,
  isSyntheticInternalFailure,
  makeFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { defaultLandingPath } from '../../lib/rbac';
import { sessionExpiredNotice } from './loginNotice';
import { useMe } from './useMe';
import '../../styles/admin.css';

interface LoginFormValues {
  username: string;
  password: string;
}

// 只允许站内路径回跳，阻止 //evil.example 之类的开放重定向。
// 未带 next 时返回 null，由登录响应里的 permissions 决定落地页。
function safeNext(raw: string | null): string | null {
  if (!raw) {
    return null;
  }
  if (!raw.startsWith('/') || raw.startsWith('//')) {
    return null;
  }
  if (raw === '/login' || raw.startsWith('/login?') || raw.startsWith('/login/')) {
    return null;
  }
  return raw;
}

export default function LoginPage() {
  const [searchParams] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form] = Form.useForm<LoginFormValues>();
  const next = safeNext(searchParams.get('next'));
  // 会话过期或角色/权限变更导致服务端吊销会话时，认证门会带 reason 跳回；
  // 明确告知用户原因，而不是静默回到登录页。被停用账号仍统一 AUTH_INVALID
  //（契约 12.4 第 7 条，禁止账号枚举），不能在此区分。
  const sessionNotice = sessionExpiredNotice(searchParams.get('reason'));
  // RBAC 走查 sprint 2：邀请/重置落地页设置密码成功后跳转到登录页并带上
  // state.passwordSet（InviteAcceptPage）。此前登录页从不读这个 state，
  // 用户被静默送到 /login，不知道密码已生效、也不知道为何没有自动登录。
  const passwordSet = (location.state as { passwordSet?: boolean } | null)?.passwordSet === true;
  // 契约 8.3 第 7 章：登录页也走全站主题（公开页，无 AppShell 包裹）。
  const { resolved: theme } = useTheme();

  // 已登录访问 /login 时直接回首页（CONTRACT.md 8.1）。
  // 00012：缓存完整 MeResponse（user + permissions），供角色感知落地。
  const meQuery = useMe();

  const loginMutation = useMutation({
    mutationFn: async (values: LoginFormValues) => {
      // 00012：登录必须同时提交 username 与 password。
      const { data, error, response } = await api.POST('/api/auth/login', {
        body: { username: values.username, password: values.password },
        params: { header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty login response'));
      }
      return data;
    },
    onSuccess: (data) => {
      // 00012：缓存完整响应（含 permissions），避免落地页再拉一次 me。
      queryClient.setQueryData(qk.me(), data);
      navigate(next ?? defaultLandingPath(data.permissions), { replace: true });
    },
  });

  if (meQuery.data) {
    return <Navigate to={next ?? defaultLandingPath(meQuery.data.permissions)} replace />;
  }

  if (meQuery.isPending) {
    return (
      <ConfigProvider theme={themeConfig(theme)}>
        <div className="opsmind-login-page">
          <Spin size="large" tip="正在确认登录状态…">
            <div style={{ width: 160, height: 80 }} />
          </Spin>
        </div>
      </ConfigProvider>
    );
  }

  const meFailure = meQuery.isError ? failureFromError(meQuery.error) : null;
  const loginFailure = loginMutation.error ? failureFromError(loginMutation.error) : null;
  // 冲刺 7 打磨：登出/改密后 AppShell 会 queryClient.clear() 清空缓存，
  // 在途 me 查询被中止/重发，其失败经 failureFromError 兜底为不带 requestId
  // 的合成 INTERNAL，登录页曾把它渲染成「服务器内部错误」+ 重试横幅——把
  // 预期内的会话结束误报成服务器故障。合成 INTERNAL（后端真实 5xx 必带
  // request_id）视为会话已结束的正常态：不显示 me 横幅，登录表单照常可用，
  // 登录请求本身失败仍由 loginFailure 正常提示。
  const showMeWarning =
    meFailure !== null &&
    meFailure.code !== 'AUTH_REQUIRED' &&
    !isSyntheticInternalFailure(meFailure);

  return (
    <ConfigProvider theme={themeConfig(theme)}>
      <div className="opsmind-login-page">
        <Card className="opsmind-login-card">
          <div className="opsmind-login-brand">
            <div className="opsmind-login-logo">
              {/* 40px 底座内 24px mark；颜色继承 .opsmind-login-logo 的
                  color: var(--color-primary)，随主题切换。字标紧随其后，
                  不把 mark 单独当导航图标使用。 */}
              <BrandMark size={24} decorative />
            </div>
            <div>
              <Typography.Title level={4} style={{ margin: 0 }}>
                云枢 OpsMind
              </Typography.Title>
              <Typography.Text type="secondary">运维智能体控制台</Typography.Text>
            </div>
          </div>
          <Typography.Paragraph type="secondary" style={{ marginTop: 16 }}>
            使用你的用户名与密码登录。角色与权限由服务端下发，界面只按权限展示可用操作。
          </Typography.Paragraph>
          {sessionNotice ? (
            <Alert type="info" showIcon style={{ marginBottom: 12 }} message={sessionNotice} />
          ) : null}
          {passwordSet ? (
            <Alert
              type="success"
              showIcon
              style={{ marginBottom: 12 }}
              message="密码已设置，请用新密码登录"
              description="为安全起见不会自动登录；请使用刚设置的密码登录。"
            />
          ) : null}
          <Typography.Paragraph type="secondary" style={{ marginTop: -8, fontSize: 12 }}>
            首次启动会创建 admin；未设置 ADMIN_PASSWORD 时随机密码打印在服务日志中（docker compose
            logs app | grep &apos;admin
            password&apos;）。其他账号由管理员创建并通过邀请邮件设置密码。
          </Typography.Paragraph>

          {showMeWarning && meFailure ? (
            <ApiErrorAlert failure={meFailure} onRetry={() => void meQuery.refetch()} />
          ) : null}
          {/* 00012：用户名 + 密码均需输入；服务端对未知用户/停用/错密码统一 AUTH_INVALID。 */}
          {loginFailure ? (
            <ApiErrorAlert failure={loginFailure} describe={describeLoginFailure} />
          ) : null}

          <Form<LoginFormValues>
            form={form}
            layout="vertical"
            initialValues={{ username: '', password: '' }}
            onFinish={(values) => {
              loginMutation.reset();
              loginMutation.mutate(values);
            }}
            requiredMark={false}
          >
            <Form.Item
              name="username"
              label="用户名"
              rules={[
                { required: true, message: '请输入用户名' },
                { max: 64, message: '用户名不能超过 64 个字符' },
              ]}
            >
              <Input
                prefix={<UserOutlined />}
                placeholder="请输入用户名"
                autoComplete="username"
                autoFocus
              />
            </Form.Item>
            <Form.Item
              name="password"
              label="密码"
              rules={[
                { required: true, message: '请输入密码' },
                { max: 256, message: '密码不能超过 256 个字符' },
              ]}
            >
              <Input.Password
                prefix={<LockOutlined />}
                placeholder="请输入密码"
                autoComplete="current-password"
              />
            </Form.Item>
            <Form.Item style={{ marginBottom: 0 }}>
              <Button
                type="primary"
                htmlType="submit"
                block
                size="large"
                loading={loginMutation.isPending}
              >
                登录
              </Button>
            </Form.Item>
          </Form>
        </Card>
      </div>
    </ConfigProvider>
  );
}
