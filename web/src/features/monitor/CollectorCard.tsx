// M4 五期：采集配置卡（CONTRACT.md 8.7 监控页——enabled 开关 +
// interval_seconds 30..3600，保存调 PUT /api/monitor/collector；成功后失效
// collector 与 monitorOverview，8.4 五期失效规则）。
//
// RBAC 第 2 轮走查（medium）：PUT /api/monitor/collector 要求
// monitor:manage（authz.PermAlertConfig），而本卡此前对任何能进监控页的
// 角色（有 monitor:read 的 operator）都渲染可点的开关/间隔/保存——点下去
// 必吃 403。这里按权限收敛：无 monitor:manage 时只读展示，不渲染可提交
// 表单（与 alerts/RulesTab、ChannelsTab 的 canConfigure 写法一致）。
// 服务端 403 保持不变，前端只负责不给出必然失败的入口。

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
import { hasAnyPermission, PERMS } from '../../lib/rbacPermissions';
import { usePermissions } from '../auth/useMe';

type CollectorConfig = components['schemas']['CollectorConfig'];

export default function CollectorCard() {
  // 采集配置的写入口要求 monitor:manage（后端 authz.PermAlertConfig）。
  // 无该权限的角色（operator 只有 monitor:read）看到的是只读回显，
  // 不再渲染必吃 403 的开关/输入框/保存按钮。
  const permissions = usePermissions();
  const canConfigure = hasAnyPermission(permissions, [PERMS.alertsWrite]);

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
  const intervalSeconds = configQuery.data?.interval_seconds ?? 60;

  return (
    <Card title="指标采集" size="small">
      {!enabled ? (
        <Alert
          type="warning"
          showIcon
          title="采集已暂停"
          description="历史指标仍可查看，折线不再自动刷新。"
          style={{ marginBottom: 12 }}
        />
      ) : null}
      {canConfigure ? (
        <CollectorConfigForm enabled={enabled} intervalSeconds={intervalSeconds} />
      ) : (
        // 只读回显：无 monitor:manage 的角色能看到当前配置，但不给可提交的
        // 控件（服务端 PUT 仍是 403，这里只是不制造必然失败的入口）。
        <Flex gap={24} wrap="wrap" data-testid="collector-readonly">
          <Typography.Text type="secondary">
            启用采集：<Typography.Text strong>{enabled ? '开' : '关'}</Typography.Text>
          </Typography.Text>
          <Typography.Text type="secondary">
            采集间隔：<Typography.Text strong>{intervalSeconds} 秒</Typography.Text>
          </Typography.Text>
          <Typography.Text type="secondary">当前角色只读，不能修改采集配置</Typography.Text>
        </Flex>
      )}
      <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
        间隔范围 30..3600 秒；变更在下一轮采集生效。采集目标为全部服务器类资产（network_device
        不采集）。
      </Typography.Text>
    </Card>
  );
}

interface CollectorConfigFormProps {
  enabled: boolean;
  intervalSeconds: number;
}

/**
 * 可写采集表单（走查第 5 轮 low）。
 *
 * `Form.useForm()` 必须在**只在 canConfigure 时挂载**的子组件里调用：
 * 此前 CollectorCard 顶层无条件创建 form 实例，只读角色的 `<Form>` 分支
 * 不渲染，antd 每次都会在 console 警告「Instance created by `useForm` is
 * not connected to any Form element」。
 */
function CollectorConfigForm({ enabled, intervalSeconds }: CollectorConfigFormProps) {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<{ enabled: boolean; interval_seconds: number }>();

  // 配置回读只填一次（避免轮询期间覆盖用户正在编辑的表单值）。
  useEffect(() => {
    form.setFieldsValue({ enabled, interval_seconds: intervalSeconds });
  }, [enabled, intervalSeconds, form]);

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
      // 固定 key：连续两次保存（暂停 → 恢复）时复用同一条 toast，避免
      // 两条「采集配置已保存」同时可见导致 e2e 定位器 strict mode 冲突。
      message.success({ content: '采集配置已保存', key: 'collector-saved' });
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

  return (
    <Form
      form={form}
      layout="vertical"
      initialValues={{ enabled, interval_seconds: intervalSeconds }}
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
  );
}
