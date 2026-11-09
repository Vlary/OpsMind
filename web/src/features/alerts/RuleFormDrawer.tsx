// M4 五期：告警规则编辑抽屉（CONTRACT.md 8.7 规则表单——metric/
// comparator/threshold/窗口/冷却/等级/静默/渠道绑定）。静默设置放本抽屉
// （silent_until 日期时间 + 清空按钮 → clear_silent_until）。

import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, DatePicker, Drawer, Form, Input, InputNumber, Modal, Select, Switch } from 'antd';
import { useMemo, useState } from 'react';

import dayjs from 'dayjs';

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
import { formatDateTime } from '../../components/admin/format';
import { message } from '../../components/antdMessage';

import {
  METRIC_LABELS,
  SEVERITY_LABELS,
  clampThreshold,
  metricThresholdBounds,
  toCreatePayload,
  toUpdatePayload,
  type RuleFormValues,
} from './ruleForm';
import { describePhase5Failure } from './phase5Codes';

type AlertRule = components['schemas']['AlertRule'];
type NotificationChannel = components['schemas']['NotificationChannel'];

interface RuleFormDrawerProps {
  open: boolean;
  rule: AlertRule | null;
  channels: NotificationChannel[];
  onClose: () => void;
}

const RULE_FIELDS = [
  'name',
  'metric',
  'comparator',
  'threshold',
  'duration_windows',
  'cooldown_seconds',
  'severity',
  'silent_until',
  'notify_channel_id',
  'enabled',
];

export default function RuleFormDrawer({ open, rule, channels, onClose }: RuleFormDrawerProps) {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<RuleFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);

  // 目标变化经调用方 key 重挂载（容器操作先例），初始值在挂载期一次成型，
  // 不在 effect 里 setState/reset（react-hooks/set-state-in-effect）。
  const initialValues = useMemo<Partial<RuleFormValues>>(
    () =>
      rule
        ? {
            name: rule.name,
            metric: rule.metric,
            comparator: rule.comparator,
            threshold: rule.threshold,
            duration_windows: rule.duration_windows,
            cooldown_seconds: rule.cooldown_seconds,
            severity: rule.severity,
            clear_silent_until: false,
            clear_notify_channel: false,
            enabled: rule.enabled,
            ...(rule.notify_channel_id ? { notify_channel_id: rule.notify_channel_id } : {}),
          }
        : {},
    [rule],
  );

  const saveMutation = useMutation({
    mutationFn: async (values: RuleFormValues) => {
      if (rule) {
        const { data, error, response } = await api.PATCH('/api/alert-rules/{id}', {
          params: { header: writeHeaders(), path: { id: rule.id } },
          body: toUpdatePayload(values),
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const { data, error, response } = await api.POST('/api/alert-rules', {
        params: { header: writeHeaders() },
        body: toCreatePayload(values),
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      message.success(rule ? '规则已更新' : '规则已创建');
      // 8.4：规则 CRUD → 失效 alertRules/alertRule。
      void queryClient.invalidateQueries({ queryKey: qk.alertRules });
      if (rule) {
        void queryClient.invalidateQueries({ queryKey: qk.alertRule(rule.id) });
      }
      onClose();
    },
    onError: (error) => {
      const apiFailure = failureFromError(error);
      setFailure(apiFailure);
      applyFieldErrors(apiFailure.details, RULE_FIELDS, (items) => {
        void form.setFields(items as never);
      });
    },
  });

  const metric = Form.useWatch('metric', form) ?? 'cpu_pct';
  const bounds = metricThresholdBounds(metric);

  return (
    <Drawer
      title={rule ? '编辑告警规则' : '新建告警规则'}
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
      <Form
        form={form}
        layout="vertical"
        initialValues={{
          metric: 'cpu_pct',
          comparator: 'gt',
          threshold: 90,
          duration_windows: 3,
          cooldown_seconds: 300,
          severity: 'warning',
          enabled: true,
          ...initialValues,
        }}
        onFinish={(values) => {
          saveMutation.mutate(values);
        }}
      >
        <Form.Item
          name="name"
          label="规则名称"
          rules={[
            { required: true, message: '请输入规则名称' },
            { max: 100, message: '名称最长 100 字符' },
          ]}
        >
          <Input placeholder="如：CPU 持续过高" maxLength={100} />
        </Form.Item>
        <Form.Item name="metric" label="指标">
          <Select
            options={Object.entries(METRIC_LABELS).map(([value, label]) => ({ value, label }))}
            onChange={(value) => {
              // 指标切换时把越界阈值钳回合法域。
              const current = form.getFieldValue('threshold') as number;
              form.setFieldsValue({ threshold: clampThreshold(value, Number(current)) });
            }}
          />
        </Form.Item>
        <Form.Item name="comparator" label="比较方式">
          <Select
            options={[
              { value: 'gt', label: '大于（>）' },
              { value: 'lt', label: '小于（<）' },
            ]}
          />
        </Form.Item>
        <Form.Item
          name="threshold"
          label={`阈值${bounds.max !== null ? `（0..${bounds.max}）` : '（≥0）'}`}
          rules={[{ required: true, message: '请输入阈值' }]}
        >
          <InputNumber
            min={bounds.min}
            {...(bounds.max !== null ? { max: bounds.max } : {})}
            step={1}
            style={{ width: '100%' }}
          />
        </Form.Item>
        <Form.Item
          name="duration_windows"
          label="持续窗口（个采集点）"
          extra="连续 N 个采集点越限才触发；1..30。"
          rules={[{ required: true, message: '请输入持续窗口' }]}
        >
          <InputNumber min={1} max={30} precision={0} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item
          name="cooldown_seconds"
          label="冷却（秒）"
          extra="恢复后再次触发前的最小间隔；60..86400。"
          rules={[{ required: true, message: '请输入冷却秒数' }]}
        >
          <InputNumber min={60} max={86400} precision={0} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item name="severity" label="等级">
          <Select
            options={Object.entries(SEVERITY_LABELS).map(([value, label]) => ({ value, label }))}
          />
        </Form.Item>
        <Form.Item
          name="silent_until"
          label="静默截止时间"
          extra={
            rule?.silent_until
              ? `当前静默至 ${formatDateTime(rule.silent_until)}（清空后不再静默）`
              : '静默期内触发不投递通知（事件仍记录）。'
          }
          getValueProps={(value) => ({ value: value ? dayjs(value as string) : null })}
          normalize={(value) => (value ? (value as dayjs.Dayjs).toISOString() : null)}
        >
          <DatePicker
            showTime
            style={{ width: '100%' }}
            disabledDate={(date) => date.isBefore(new Date(), 'day')}
          />
        </Form.Item>
        <Form.Item
          name="notify_channel_id"
          label="通知渠道"
          extra="触发后向该渠道投递一次；不选则不通知。"
        >
          <Select
            allowClear
            placeholder="不绑定渠道"
            options={channels.map((channel) => ({
              value: channel.id,
              label: `${channel.name}（${channel.kind}）`,
            }))}
          />
        </Form.Item>
        <Form.Item name="enabled" label="启用规则" valuePropName="checked">
          <Switch checkedChildren="启用" unCheckedChildren="停用" />
        </Form.Item>
        <Form.Item style={{ marginBottom: 0 }}>
          <Button
            type="primary"
            htmlType="submit"
            loading={saveMutation.isPending}
            style={{ marginRight: 8 }}
          >
            保存
          </Button>
          {rule && (rule.silent_until || rule.notify_channel_id) ? (
            <Button
              danger
              onClick={() => {
                Modal.confirm({
                  title: '清除静默/渠道绑定？',
                  content: '将清除静默时间与渠道绑定（规则本体保留）。',
                  okText: '清除',
                  onOk: () => {
                    saveMutation.mutate({
                      ...(form.getFieldsValue() as RuleFormValues),
                      clear_silent_until: true,
                      clear_notify_channel: true,
                      silent_until: null,
                      notify_channel_id: null,
                    });
                  },
                });
              }}
            >
              清除静默/渠道
            </Button>
          ) : null}
        </Form.Item>
      </Form>
    </Drawer>
  );
}
