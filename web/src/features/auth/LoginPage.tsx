import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, ConfigProvider, Form, Input, Spin, Typography } from 'antd';
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom';

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
import '../../styles/admin.css';

interface LoginFormValues {
  username: string;
  password: string;
}

// 只允许站内路径回跳，阻止 //evil.example 之类的开放重定向。
function safeNext(raw: string | null): string {
  if (!raw) {
    return '/chat';
  }
  if (!raw.startsWith('/') || raw.startsWith('//')) {
    return '/chat';
  }
  if (raw === '/login' || raw.startsWith('/login?') || raw.startsWith('/login/')) {
    return '/chat';
  }
  return raw;
}

export default function LoginPage() {
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form] = Form.useForm<LoginFormValues>();
  const next = safeNext(searchParams.get('next'));
  // 契约 8.3 第 7 章：登录页也走全站主题（公开页，无 AppShell 包裹）。
  const { resolved: theme } = useTheme();

  // 已登录访问 /login 时直接回首页（CONTRACT.md 8.1）。
  const meQuery = useQuery({
    queryKey: qk.me(),
    retry: false,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/auth/me');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty me response'));
      }
      return data.user;
    },
  });

  const loginMutation = useMutation({
    mutationFn: async (values: LoginFormValues) => {
      // LoginRequest 只接受 password（用户名固定为内置 admin）。
      const { data, error, response } = await api.POST('/api/auth/login', {
        body: { password: values.password },
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
      queryClient.setQueryData(qk.me(), data.user);
      navigate(next, { replace: true });
    },
  });

  if (meQuery.data) {
    return <Navigate to={next} replace />;
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
            使用内置管理员账户登录，以管理服务器、数据库、模型与审计日志。
          </Typography.Paragraph>
          <Typography.Paragraph type="secondary" style={{ marginTop: -8, fontSize: 12 }}>
            未设置 ADMIN_PASSWORD 时，首次启动会生成随机密码并打印在服务日志中 （docker compose logs
            app | grep &apos;admin password&apos;）。
          </Typography.Paragraph>

          {showMeWarning && meFailure ? (
            <ApiErrorAlert failure={meFailure} onRetry={() => void meQuery.refetch()} />
          ) : null}
          {/* 登录页唯一可输入凭据是管理员密码（用户名固定 admin）：AUTH_INVALID
            按场景改写为「管理员密码错误」，不再提示不存在的用户名字段。 */}
          {loginFailure ? (
            <ApiErrorAlert failure={loginFailure} describe={describeLoginFailure} />
          ) : null}

          <Form<LoginFormValues>
            form={form}
            layout="vertical"
            initialValues={{ username: 'admin', password: '' }}
            onFinish={(values) => {
              loginMutation.reset();
              loginMutation.mutate(values);
            }}
            requiredMark={false}
          >
            <Form.Item
              name="username"
              label="用户名"
              rules={[{ required: true, message: '请输入用户名' }]}
              extra="服务端仅支持内置管理员 admin"
            >
              <Input prefix={<UserOutlined />} disabled autoComplete="username" />
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
                placeholder="请输入管理员密码"
                autoComplete="current-password"
                autoFocus
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
