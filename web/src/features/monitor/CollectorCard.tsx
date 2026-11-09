// M4 五期：采集配置卡（CONTRACT.md 8.7 监控页——enabled 开关 +
// interval_seconds 30..3600，保存调 PUT /api/monitor/collector；成功后失效
// collector 与 monitorOverview，8.4 五期失效规则）。

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Flex, Form, InputNumber, Switch, Typography } from 'antd';
import { useEffect } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import {
  ApiRequestError,
  describeFailure,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import { message } from '../../components/antdMessage';

type CollectorConfig = components['schemas']['CollectorConfig'];

export default function CollectorCard() {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<{ enabled: boolean; interval_seconds: number }>();

  const configQuery = useQuery({
    queryKey: qk.collector,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/monitor/collector');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
  });

  // 配置回读只填一次（避免轮询期间覆盖用户正在编辑的表单值）。
  useEffect(() => {
    if (configQuery.data) {
      form.setFieldsValue({
        enabled: configQuery.data.enabled,
        interval_seconds: configQuery.data.interval_seconds,
      });
    }
  }, [configQuery.data, form]);

  const saveMutation = useMutation({
    mutationFn: async (values: { enabled: boolean; interval_seconds: number }) => {
      const { data, error, response } = await api.PUT('/api/monitor/collector', {
        params: { header: writeHeaders() },
        body: values,
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (saved: CollectorConfig | undefined) => {
      message.success('采集配置已保存');
      // 契约 8.4：PUT 成功 → 失效 collector 与 monitorOverview。
      void queryClient.invalidateQueries({ queryKey: qk.collector });
      void queryClient.invalidateQueries({ queryKey: qk.monitorOverview });
      if (saved) {
        form.setFieldsValue({ enabled: saved.enabled, interval_seconds: saved.interval_seconds });
      }
    },
    onError: (error) => {
      message.error(describeFailure(failureFromError(error)));
    },
  });

  if (configQuery.isLoading) {
    return (
      <Card title="指标采集" size="small">
        <AdminLoading rows={2} />
      </Card>
    );
  }

  if (configQuery.isError) {
    return (
      <Card title="指标采集" size="small">
        <QueryErrorState
          error={configQuery.error}
          onRetry={() => {
            void configQuery.refetch();
          }}
        />
      </Card>
    );
  }

  const enabled = configQuery.data?.enabled ?? false;

  return (
    <Card title="指标采集" size="small">
      {!enabled ? (
        <Alert
          type="warning"
          showIcon
          message="采集已暂停"
          description="历史指标仍可查看，折线不再自动刷新。"
          style={{ marginBottom: 12 }}
        />
      ) : null}
      <Form
        form={form}
        layout="vertical"
        initialValues={{ enabled, interval_seconds: configQuery.data?.interval_seconds ?? 60 }}
        onFinish={(values) => {
          saveMutation.mutate(values);
        }}
      >
        <Flex gap={16} align="flex-end" wrap="wrap">
          <Form.Item
            name="enabled"
            label="启用采集"
            valuePropName="checked"
            style={{ marginBottom: 0 }}
          >
            <Switch checkedChildren="开" unCheckedChildren="关" />
          </Form.Item>
          <Form.Item
            name="interval_seconds"
            label="采集间隔（秒）"
            rules={[{ required: true, message: '请输入采集间隔' }]}
            style={{ marginBottom: 0 }}
          >
            <InputNumber min={30} max={3600} step={30} style={{ width: 140 }} />
          </Form.Item>
          <Form.Item style={{ marginBottom: 0 }}>
            <Button
              type="primary"
              htmlType="submit"
              loading={saveMutation.isPending}
              aria-label="保存采集配置"
            >
              保存
            </Button>
          </Form.Item>
        </Flex>
      </Form>
      <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
        间隔范围 30..3600 秒；变更在下一轮采集生效。采集目标为全部服务器类资产（network_device
        不采集）。
      </Typography.Text>
    </Card>
  );
}
