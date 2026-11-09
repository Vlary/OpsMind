// M4 五期：即时巡检弹窗（CONTRACT.md 8.7 巡检页右上「立即巡检」——服务器
// 多选 ≤50 + 模板；POST /api/inspection/run 同步返回完整报告并打开详情）。

import { useMutation, useQuery } from '@tanstack/react-query';
import { Alert, Button, Modal, Select, Space, Typography } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import EmptyState from '../../components/EmptyState';
import { message } from '../../components/antdMessage';
import { describePhase5Failure } from '../alerts/phase5Codes';

type Server = components['schemas']['Server'];
type InspectionReportDetail = components['schemas']['InspectionReportDetail'];

export const RUN_NOW_MAX_SERVERS = 50;

export interface RunNowModalProps {
  open: boolean;
  onClose: () => void;
  /** 同步巡检完成（成功）时回调（打开报告详情）。 */
  onReport: (reportId: string) => void;
}

export default function RunNowModal({ open, onClose, onReport }: RunNowModalProps) {
  // 打开/关闭经调用方 key=String(open) 重挂载，状态天然重置——不在
  // effect 里 setState（react-hooks/set-state-in-effect）。
  const [serverIds, setServerIds] = useState<string[]>([]);
  const [template, setTemplate] = useState<'basic' | 'full'>('basic');
  const [errorText, setErrorText] = useState<string | null>(null);

  const serversQuery = useQuery({
    queryKey: qk.servers,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    enabled: open,
  });

  const serverOptions = (serversQuery.data ?? [])
    .filter((server: Server) => server.device_kind === 'server')
    .map((server: Server) => ({ value: server.id, label: server.name }));

  const runMutation = useMutation({
    mutationFn: async () => {
      const { data, error, response } = await api.POST('/api/inspection/run', {
        params: { header: writeHeaders() },
        body: { server_ids: serverIds, template },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (detail: InspectionReportDetail | undefined) => {
      message.success(`巡检完成：平均分 ${detail?.report.avg_score ?? '—'}`);
      onReport(detail?.report.id ?? '');
      onClose();
    },
    onError: (error) => {
      setErrorText(describePhase5Failure(failureFromError(error)));
    },
  });

  const overLimit = serverIds.length > RUN_NOW_MAX_SERVERS;

  return (
    <Modal title="立即巡检" open={open} onCancel={onClose} footer={null} destroyOnHidden>
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        {errorText ? (
          <Alert
            type="error"
            showIcon
            message={errorText}
            closable
            onClose={() => {
              setErrorText(null);
            }}
          />
        ) : null}
        <Typography.Text type="secondary">
          同步执行（目标 1..{RUN_NOW_MAX_SERVERS} 台），完成后直接展示报告。
        </Typography.Text>
        <Select
          mode="multiple"
          style={{ width: '100%' }}
          placeholder="选择目标服务器"
          aria-label="巡检目标服务器"
          value={serverIds}
          onChange={setServerIds}
          options={serverOptions}
          notFoundContent={<EmptyState description="暂无服务器类资产" />}
        />
        <Select
          style={{ width: '100%' }}
          aria-label="巡检模板"
          value={template}
          onChange={setTemplate}
          options={[
            { value: 'basic', label: '基础模板（服务状态 / 磁盘 / 负载）' },
            { value: 'full', label: '完整模板（含内存 / journal 错误）' },
          ]}
        />
        <Button
          type="primary"
          block
          loading={runMutation.isPending}
          disabled={serverIds.length === 0 || overLimit}
          onClick={() => {
            runMutation.mutate();
          }}
        >
          {runMutation.isPending ? '巡检执行中……' : '开始巡检'}
        </Button>
        {overLimit ? (
          <Alert type="error" showIcon message={`目标超过 ${RUN_NOW_MAX_SERVERS} 台上限`} />
        ) : null}
      </Space>
    </Modal>
  );
}
