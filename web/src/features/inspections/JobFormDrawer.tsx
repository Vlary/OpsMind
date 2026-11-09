// M4 五期：巡检任务表单抽屉（CONTRACT.md 8.7——cron（UTC 标注）/
// target_tags/template/通知开关/渠道绑定）。

import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, Drawer, Form, Input, Select, Switch, Typography } from 'antd';
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
import { describePhase5Failure } from '../alerts/phase5Codes';

import { looksLikeCron } from './inspectionView';

type InspectionJob = components['schemas']['InspectionJob'];
type NotificationChannel = components['schemas']['NotificationChannel'];

interface JobFormValues {
  name: string;
  cron: string;
  target_tags: string[];
  template: 'basic' | 'full';
  notify_enabled: boolean;
  notify_channel_id?: string;
  clear_notify_channel?: boolean;
  enabled: boolean;
}

interface JobFormDrawerProps {
  open: boolean;
  job: InspectionJob | null;
  channels: NotificationChannel[];
  onClose: () => void;
}

const JOB_FIELDS = [
  'name',
  'cron',
  'target_tags',
  'template',
  'notify_enabled',
  'notify_channel_id',
  'enabled',
];

export default function JobFormDrawer({ open, job, channels, onClose }: JobFormDrawerProps) {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<JobFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);

  // 目标变化经调用方 key 重挂载；挂载期初始值一次成型（不在 effect 里
  // setState/reset，容器操作先例）。
  const initialValues = useMemo<Partial<JobFormValues>>(() => {
    if (!job) {
      return {};
    }
    const values: Partial<JobFormValues> = {
      name: job.name,
      cron: job.cron,
      target_tags: job.target_tags,
      template: job.template,
      notify_enabled: job.notify_enabled,
      clear_notify_channel: false,
      enabled: job.enabled,
    };
    if (job.notify_channel_id) {
      values.notify_channel_id = job.notify_channel_id;
    }
    return values;
  }, [job]);

  const saveMutation = useMutation({
    mutationFn: async (values: JobFormValues) => {
      const tags = (values.target_tags ?? []).map((tag) => tag.trim()).filter((tag) => tag !== '');
      if (job) {
        const body: components['schemas']['InspectionJobUpdate'] = {
          name: values.name.trim(),
          cron: values.cron.trim(),
          target_tags: tags,
          template: values.template,
          notify_enabled: values.notify_enabled,
          enabled: values.enabled,
        };
        if (values.clear_notify_channel) {
          body.clear_notify_channel = true;
        } else if (values.notify_channel_id) {
          body.notify_channel_id = values.notify_channel_id;
        } else if (job.notify_channel_id) {
          // 编辑时用户 allowClear 清空了渠道（表单值 undefined 而任务仍有
          // 旧绑定）——PATCH 无法表达「清除」，服务端会静默保留旧渠道并
          // 提示成功（五期评审发现）。显式走 clear 语义。
          body.clear_notify_channel = true;
        }
        const { data, error, response } = await api.PATCH('/api/inspection/jobs/{id}', {
          params: { header: writeHeaders(), path: { id: job.id } },
          body,
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const { data, error, response } = await api.POST('/api/inspection/jobs', {
        params: { header: writeHeaders() },
        body: {
          name: values.name.trim(),
          cron: values.cron.trim(),
          target_tags: tags,
          template: values.template,
          notify_enabled: values.notify_enabled,
          ...(values.notify_enabled && values.notify_channel_id
            ? { notify_channel_id: values.notify_channel_id }
            : {}),
          enabled: values.enabled,
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      message.success(job ? '任务已更新' : '任务已创建');
      // 8.4：job CRUD → 失效 inspectionJobs/inspectionJob。
      void queryClient.invalidateQueries({ queryKey: qk.inspectionJobs });
      if (job) {
        void queryClient.invalidateQueries({ queryKey: qk.inspectionJob(job.id) });
      }
      onClose();
    },
    onError: (error) => {
      const apiFailure = failureFromError(error);
      setFailure(apiFailure);
      applyFieldErrors(apiFailure.details, JOB_FIELDS, (items) => {
        void form.setFields(items as never);
      });
    },
  });

  return (
    <Drawer
      title={job ? '编辑巡检任务' : '新建巡检任务'}
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
          name: '',
          cron: '0 8 * * *',
          target_tags: [],
          template: 'basic',
          notify_enabled: false,
          enabled: true,
          ...initialValues,
        }}
        onFinish={(values) => {
          saveMutation.mutate(values);
        }}
      >
        <Form.Item
          name="name"
          label="任务名称"
          rules={[{ required: true, message: '请输入任务名称' }]}
        >
          <Input maxLength={100} placeholder="如：工作日晨检" />
        </Form.Item>
        <Form.Item
          name="cron"
          label="cron 表达式（UTC）"
          extra="五段：分 时 日 月 周；如 “0 8 * * *”=每天 UTC 08:00。"
          rules={[
            { required: true, message: '请输入 cron 表达式' },
            {
              validator: (_rule, value: string) =>
                looksLikeCron(value)
                  ? Promise.resolve()
                  : Promise.reject(new Error('应为五段表达式（分 时 日 月 周）')),
            },
          ]}
        >
          <Input className="opsmind-admin-mono" placeholder="0 8 * * *" />
        </Form.Item>
        <Form.Item
          name="target_tags"
          label="目标 tags（空 = 全部服务器类资产）"
          extra="巡检只覆盖 device_kind=server 且命中所选任一 tag 的资产。"
        >
          <Select
            mode="tags"
            open={false}
            placeholder="输入 tag 后回车"
            tokenSeparators={[',', ' ']}
          />
        </Form.Item>
        <Form.Item name="template" label="巡检模板">
          <Select
            options={[
              { value: 'basic', label: '基础模板（服务状态 / 磁盘 / 负载）' },
              { value: 'full', label: '完整模板（含内存 / journal 错误）' },
            ]}
          />
        </Form.Item>
        <Form.Item name="notify_enabled" label="完成后通知" valuePropName="checked">
          <Switch checkedChildren="开" unCheckedChildren="关" />
        </Form.Item>
        <Form.Item
          noStyle
          shouldUpdate={(prev, next) => prev.notify_enabled !== next.notify_enabled}
        >
          {({ getFieldValue }) =>
            getFieldValue('notify_enabled') ? (
              <Form.Item name="notify_channel_id" label="通知渠道">
                <Select
                  allowClear
                  placeholder="选择渠道"
                  options={channels.map((channel) => ({
                    value: channel.id,
                    label: `${channel.name}（${channel.kind}）`,
                  }))}
                />
              </Form.Item>
            ) : null
          }
        </Form.Item>
        <Form.Item name="enabled" label="启用任务" valuePropName="checked">
          <Switch checkedChildren="启用" unCheckedChildren="停用" />
        </Form.Item>
        <Form.Item style={{ marginBottom: 0 }}>
          <Button type="primary" htmlType="submit" loading={saveMutation.isPending}>
            保存
          </Button>
        </Form.Item>
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
          巡检为只读采集 + 规则评分，不使用 LLM；报告保留历史并可对比上次评分。
        </Typography.Text>
      </Form>
    </Drawer>
  );
}
