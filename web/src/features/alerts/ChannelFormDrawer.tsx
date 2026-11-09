// M4 五期：通知渠道表单抽屉（CONTRACT.md 8.7——按 kind 切字段；已保存凭据
// 永不回填（留空=不修改）；config_hint 只读展示；测试按钮调 POST .../test，
// 结果展示 error_code 或成功耗时，不进缓存）。

import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Checkbox, Drawer, Form, Input, InputNumber, Select, Space } from 'antd';
import { useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  applyFieldErrors,
  failureFromError,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import { message } from '../../components/antdMessage';

import {
  CHANNEL_KIND_LABELS,
  buildEmailConfig,
  buildUrlConfig,
  describeTestResult,
  isUrlKind,
  type ChannelKind,
} from './channelForm';
import { describePhase5Failure } from './phase5Codes';

type NotificationChannel = components['schemas']['NotificationChannel'];
type NotifyTestResult = components['schemas']['NotifyTestResult'];

interface ChannelFormDrawerProps {
  open: boolean;
  channel: NotificationChannel | null;
  onClose: () => void;
}

interface ChannelFormShape {
  name: string;
  kind: ChannelKind;
  url?: string;
  secret?: string;
  smtp_host?: string;
  smtp_port?: number;
  username?: string;
  password?: string;
  from?: string;
  to?: string;
  use_tls?: boolean;
}

const CHANNEL_FIELDS = [
  'name',
  'kind',
  'url',
  'secret',
  'smtp_host',
  'smtp_port',
  'username',
  'password',
  'from',
  'to',
  'use_tls',
];

export default function ChannelFormDrawer({ open, channel, onClose }: ChannelFormDrawerProps) {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<ChannelFormShape>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const [testResult, setTestResult] = useState<string | null>(null);

  const editing = channel !== null;

  // 目标变化经调用方 key 重挂载；挂载期初始值一次成型（不回填任何凭据，
  // 8.7），不在 effect 里 setState/reset。
  const initialValues = useMemo<Partial<ChannelFormShape>>(
    () => (channel ? { name: channel.name, kind: channel.kind, use_tls: true } : {}),
    [channel],
  );

  const kind = Form.useWatch('kind', form) ?? 'webhook';

  /**
   * 构造 config 明文：仅在需要携带 config 时调用——新建必带（全量）；
   * 编辑仅在用户填写了任一配置字段时携带（整体替换），留空=不修改。
   */
  function buildConfig(
    values: ChannelFormShape,
  ): { ok: true; config: Record<string, unknown> } | { ok: false; error: string } {
    if (isUrlKind(kind)) {
      return buildUrlConfig({ url: values.url ?? '', secret: values.secret ?? '' });
    }
    return buildEmailConfig({
      smtp_host: values.smtp_host ?? '',
      smtp_port: values.smtp_port ?? 465,
      username: values.username ?? '',
      password: values.password ?? '',
      from: values.from ?? '',
      to: values.to ?? '',
      use_tls: values.use_tls ?? true,
    });
  }

  /** 编辑态用户是否填写了任一配置字段（决定 PATCH 是否携带 config）。 */
  function touchedConfig(values: ChannelFormShape): boolean {
    if (isUrlKind(kind)) {
      return (values.url ?? '').trim() !== '' || (values.secret ?? '').trim() !== '';
    }
    return (
      (values.smtp_host ?? '').trim() !== '' ||
      (values.from ?? '').trim() !== '' ||
      (values.to ?? '').trim() !== ''
    );
  }

  const saveMutation = useMutation({
    mutationFn: async (values: ChannelFormShape) => {
      if (channel) {
        const body: components['schemas']['NotificationChannelUpdate'] = {
          name: values.name.trim(),
        };
        // config 留空表示不修改：仅在用户填写了任一配置字段时携带（整体替换）。
        if (touchedConfig(values)) {
          const built = buildConfig(values);
          if (!built.ok) {
            throw new ApiRequestError({
              code: 'NOTIFICATION_CONFIG_INVALID',
              message: built.error,
              requestId: '',
              details: null,
              status: null,
            });
          }
          body.config = built.config;
        }
        const { data, error, response } = await api.PATCH('/api/notification-channels/{id}', {
          params: { header: writeHeaders(), path: { id: channel.id } },
          body,
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const built = buildConfig(values);
      if (!built.ok) {
        throw new ApiRequestError({
          code: 'NOTIFICATION_CONFIG_INVALID',
          message: built.error,
          requestId: '',
          details: null,
          status: null,
        });
      }
      const { data, error, response } = await api.POST('/api/notification-channels', {
        params: { header: writeHeaders() },
        body: {
          name: values.name.trim(),
          kind: values.kind ?? 'webhook',
          config: built.config,
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      message.success(channel ? '渠道已更新' : '渠道已创建');
      // 8.4：渠道 CRUD → 失效 notificationChannels。
      void queryClient.invalidateQueries({ queryKey: qk.notificationChannels });
      onClose();
    },
    onError: (error) => {
      const apiFailure = failureFromError(error);
      setFailure(apiFailure);
      applyFieldErrors(apiFailure.details, CHANNEL_FIELDS, (items) => {
        void form.setFields(items as never);
      });
    },
  });

  const testMutation = useMutation({
    mutationFn: async () => {
      if (!channel) {
        throw new Error('尚未保存的渠道无法测试');
      }
      const { data, error, response } = await api.POST('/api/notification-channels/{id}/test', {
        params: { header: writeHeaders(), path: { id: channel.id } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (result: NotifyTestResult | undefined) => {
      if (result) {
        // 测试结果不进缓存（8.4），本地展示。
        setTestResult(
          describeTestResult(result, (code) =>
            describePhase5Failure({
              code,
              message: '',
              requestId: '',
              details: null,
              status: null,
            }),
          ),
        );
      }
    },
    onError: (error) => {
      const apiFailure = failureFromError(error);
      setTestResult(`测试投递失败：${describePhase5Failure(apiFailure)}（${apiFailure.code}）`);
    },
  });

  return (
    <Drawer
      title={channel ? '编辑通知渠道' : '新建通知渠道'}
      open={open}
      onClose={onClose}
      width={480}
      destroyOnHidden
    >
      {failure ? (
        <ApiErrorAlert
          failure={failure}
          describe={describePhase5Failure}
          closable
          onClose={() => {
            setFailure(null);
          }}
        />
      ) : null}
      {channel ? (
        <Alert
          type="info"
          showIcon
          message={`已保存配置：${channel.config_hint || '（无配置）'}`}
          description="凭据不会回填；留空表示不修改。"
          style={{ marginBottom: 16 }}
        />
      ) : null}
      {testResult ? (
        <Alert
          type={testResult.includes('成功') ? 'success' : 'error'}
          showIcon
          message={testResult}
          closable
          onClose={() => {
            setTestResult(null);
          }}
          style={{ marginBottom: 16 }}
        />
      ) : null}
      <Form
        form={form}
        layout="vertical"
        initialValues={{ kind: 'webhook', smtp_port: 465, use_tls: true, ...initialValues }}
        onFinish={(values) => {
          saveMutation.mutate(values);
        }}
      >
        <Form.Item
          name="name"
          label="渠道名称"
          rules={[{ required: true, message: '请输入渠道名称' }]}
        >
          <Input placeholder="如：值班群 webhook" maxLength={100} />
        </Form.Item>
        <Form.Item name="kind" label="类型">
          <Select
            disabled={channel !== null}
            options={Object.entries(CHANNEL_KIND_LABELS).map(([value, label]) => ({
              value,
              label,
            }))}
          />
        </Form.Item>
        {isUrlKind(kind) ? (
          <>
            <Form.Item
              name="url"
              label="回调 URL"
              rules={[{ required: !editing, message: '请输入 URL' }]}
              extra={editing ? '留空表示不修改' : 'http(s) 地址；secret 用于 HMAC 签名（可选）'}
            >
              <Input placeholder={editing ? '留空表示不修改' : 'https://example.com/hook'} />
            </Form.Item>
            <Form.Item name="secret" label="签名密钥（可选）">
              <Input.Password
                placeholder={editing ? '留空表示不修改' : 'HMAC-SHA256 签名密钥'}
                autoComplete="new-password"
              />
            </Form.Item>
          </>
        ) : (
          <>
            <Form.Item
              name="smtp_host"
              label="SMTP 服务器"
              rules={[{ required: !editing, message: '请输入 SMTP 地址' }]}
              extra={editing ? '整组配置留空表示不修改；填写则整体替换' : undefined}
            >
              <Input placeholder={editing ? '留空表示不修改' : 'smtp.example.com'} />
            </Form.Item>
            <Form.Item
              name="smtp_port"
              label="端口"
              rules={[{ required: !editing, message: '请输入端口' }]}
            >
              <InputNumber min={1} max={65535} precision={0} style={{ width: '100%' }} />
            </Form.Item>
            <Form.Item name="username" label="用户名（可选）">
              <Input autoComplete="off" />
            </Form.Item>
            <Form.Item name="password" label="密码（可选）">
              <Input.Password autoComplete="new-password" />
            </Form.Item>
            <Form.Item
              name="from"
              label="发件人"
              rules={[{ required: !editing, message: '请输入发件人' }]}
            >
              <Input placeholder={editing ? '留空表示不修改' : 'ops@example.com'} />
            </Form.Item>
            <Form.Item
              name="to"
              label="收件人（逗号/换行分隔多个）"
              rules={[{ required: !editing, message: '请输入至少一个收件人' }]}
            >
              <Input.TextArea
                rows={2}
                placeholder={editing ? '留空表示不修改' : 'a@example.com, b@example.com'}
              />
            </Form.Item>
            <Form.Item name="use_tls" label="使用 TLS" valuePropName="checked">
              <Checkbox>SMTPS / STARTTLS</Checkbox>
            </Form.Item>
          </>
        )}
        <Form.Item style={{ marginBottom: 0 }}>
          <Space>
            <Button type="primary" htmlType="submit" loading={saveMutation.isPending}>
              保存
            </Button>
            {channel ? (
              <Button
                loading={testMutation.isPending}
                onClick={() => {
                  testMutation.mutate();
                }}
              >
                发送测试消息
              </Button>
            ) : null}
          </Space>
        </Form.Item>
      </Form>
    </Drawer>
  );
}
