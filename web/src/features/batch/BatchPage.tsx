// M4 五期：批量操作页（CONTRACT.md 8.1 /batch、8.7 批量页）。
// 新建三段：自然语言描述（可选 0..2000）/ 命令（必填，实时 preview-risk
// 预览）/ 目标多选（≤50，仅 device_kind=server，按 tags 快速筛选）。
// 创建成功 → 失效 batchJobs 并打开详情抽屉（2s 轮询、逐台审批）。
// 支持 /batch?job=<id> 深链（对话卡 batch_execute 跳转，8.7）。

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Form, Input, Select, Space, Table, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  describeFailure,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import RiskBadge from '../../components/RiskBadge';
import { message } from '../../components/antdMessage';
import { describePhase5Failure } from '../alerts/phase5Codes';
import '../../styles/admin.css';

import BatchDetailDrawer from './BatchDetailDrawer';
import { BATCH_STATUS_LABELS, isBatchJobActive } from './batchView';

type Server = components['schemas']['Server'];
type BatchJob = components['schemas']['BatchJob'];
type RiskPreviewResult = components['schemas']['RiskPreviewResult'];

export const BATCH_MAX_TARGETS = 50;

interface BatchFormValues {
  description?: string;
  command: string;
  server_ids: string[];
}

export default function BatchPage() {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<BatchFormValues>();
  const [searchParams, setSearchParams] = useSearchParams();
  // 深链 /batch?job=<id>（对话 batch_execute 卡片跳转）：挂载期一次读取，
  // 不在 effect 里 setState（react-hooks/set-state-in-effect）。
  const [jobId, setJobId] = useState<string | null>(() => searchParams.get('job'));
  const [tagFilter, setTagFilter] = useState<string[]>([]);

  const serversQuery = useQuery({
    queryKey: qk.servers,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/servers');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const serverAssets = useMemo(
    () => (serversQuery.data ?? []).filter((server: Server) => server.device_kind === 'server'),
    [serversQuery.data],
  );

  // 全部可用 tag（快速筛选 chips）。
  const allTags = useMemo(() => {
    const tags = new Set<string>();
    for (const server of serverAssets) {
      for (const tag of server.tags) {
        tags.add(tag);
      }
    }
    return [...tags].sort();
  }, [serverAssets]);

  const serverOptions = useMemo(
    () =>
      serverAssets
        .filter(
          (server) => tagFilter.length === 0 || server.tags.some((tag) => tagFilter.includes(tag)),
        )
        .map((server) => ({ value: server.id, label: server.name })),
    [serverAssets, tagFilter],
  );

  const jobsQuery = useQuery({
    queryKey: qk.batchJobs,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/batch/jobs', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    // 页面文案承诺「有进行中的任务：列表 15s 自动刷新」——按此实现条件
    // 轮询（无进行中任务时停轮询；五期评审发现文案与实现不符）。
    refetchInterval: (query) =>
      (query.state.data ?? []).some((job) => isBatchJobActive(job.status)) ? 15_000 : false,
  });

  const command = Form.useWatch('command', form) ?? '';

  // 实时风险预览（8.7：命令必填实时调 preview-risk；相同命令 15s staleTime 去重）。
  const previewQuery = useQuery({
    queryKey: qk.batchRiskPreview(command),
    queryFn: async () => {
      const { data, error, response } = await api.POST('/api/ops/preview-risk', {
        params: { header: writeHeaders() },
        // 批量目标均为服务器资产；逐台实际分类以后端分派为准，这里按
        // linux 分册给出预览提示（契约 8.7 批量页：实时预览风险）。
        body: { command, os: 'linux', netdev_brand: 'generic' },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: command.trim() !== '',
    staleTime: 15_000,
    retry: false,
  });

  const createMutation = useMutation({
    mutationFn: async (values: BatchFormValues) => {
      const { data, error, response } = await api.POST('/api/batch/jobs', {
        params: { header: writeHeaders() },
        body: {
          command: values.command,
          server_ids: values.server_ids,
          ...(values.description && values.description.trim() !== ''
            ? { description: values.description.trim() }
            : {}),
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (detail) => {
      message.success('批量任务已创建');
      // 8.4：创建成功 → 失效 batchJobs 并打开详情。创建响应可能是
      // BatchJobDetail{job} 或 BatchJob（M3 实现形态），统一取 id 后由
      // batchJob(id) 查询拉详情。
      void queryClient.invalidateQueries({ queryKey: qk.batchJobs });
      form.resetFields();
      const createdId =
        detail && typeof detail === 'object' && 'job' in detail
          ? (detail as { job: { id: string } }).job.id
          : ((detail as { id?: string } | undefined)?.id ?? null);
      if (createdId) {
        setJobId(createdId);
      }
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  function closeDetail(): void {
    setJobId(null);
    const next = new URLSearchParams(searchParams);
    next.delete('job');
    setSearchParams(next, { replace: true });
    // 关闭详情后列表立即对齐最新状态（此前停留首加载值）。
    void queryClient.invalidateQueries({ queryKey: qk.batchJobs });
  }

  const columns: TableColumnsType<BatchJob> = [
    {
      title: '命令 / 描述',
      key: 'command',
      render: (_, job) => (
        <Space direction="vertical" size={0}>
          <span className="opsmind-admin-mono opsmind-admin-cell-title">{job.command}</span>
          {job.description ? (
            <span className="opsmind-admin-cell-sub">{job.description}</span>
          ) : null}
        </Space>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (status: string) => (
        <Tag
          color={
            status === 'done'
              ? 'green'
              : status === 'partial'
                ? 'orange'
                : status === 'running'
                  ? 'blue'
                  : status === 'failed'
                    ? 'red'
                    : 'default'
          }
        >
          {BATCH_STATUS_LABELS[status] ?? status}
        </Tag>
      ),
    },
    {
      title: '进度',
      key: 'progress',
      width: 100,
      render: (_, job) => `${job.done_count}/${job.total_count}`,
    },
    {
      title: '来源',
      dataIndex: 'source',
      width: 80,
      render: (source: string) => (source === 'chat' ? '对话' : '页面'),
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      width: 170,
      render: (value: string) => formatDateTime(value),
    },
    {
      title: '操作',
      key: 'actions',
      width: 90,
      render: (_, job) => (
        <Button
          size="small"
          onClick={() => {
            setJobId(job.id);
          }}
        >
          详情
        </Button>
      ),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="批量"
        description="一条命令在多台服务器上分类执行：safe 直接跑、需审批逐台确认、禁止跳过。"
      />

      <Card size="small" title="新建批量任务">
        {serversQuery.isError ? (
          <QueryErrorState
            error={serversQuery.error}
            onRetry={() => {
              void serversQuery.refetch();
            }}
          />
        ) : (
          <Form
            form={form}
            layout="vertical"
            onFinish={(values) => {
              createMutation.mutate(values);
            }}
          >
            <Form.Item
              name="description"
              label="自然语言描述（可选）"
              extra="记录本次操作的意图，便于审计回溯；0..2000 字符。"
            >
              <Input.TextArea
                rows={2}
                maxLength={2000}
                placeholder="如：清理全部 web 节点的 apt 缓存"
              />
            </Form.Item>
            <Form.Item
              name="command"
              label="命令"
              rules={[{ required: true, message: '请输入要执行的命令' }]}
            >
              <Input.TextArea
                rows={2}
                className="opsmind-admin-mono"
                placeholder="df -h /"
                autoComplete="off"
              />
            </Form.Item>
            <RiskPreview preview={previewQuery.data} error={previewQuery.error} />
            <Form.Item
              name="server_ids"
              label={
                <Space size={8} wrap>
                  <span>目标服务器（≤{BATCH_MAX_TARGETS}）</span>
                  {allTags.map((tag) => (
                    <Tag.CheckableTag
                      key={tag}
                      checked={tagFilter.includes(tag)}
                      onChange={(checked) => {
                        setTagFilter((prev) =>
                          checked ? [...prev, tag] : prev.filter((item) => item !== tag),
                        );
                      }}
                    >
                      {tag}
                    </Tag.CheckableTag>
                  ))}
                </Space>
              }
              rules={[
                { required: true, message: '请选择至少一台服务器' },
                {
                  validator: (_rule, value: string[] | undefined) =>
                    (value?.length ?? 0) <= BATCH_MAX_TARGETS
                      ? Promise.resolve()
                      : Promise.reject(new Error(`最多 ${BATCH_MAX_TARGETS} 台`)),
                },
              ]}
              extra={
                tagFilter.length > 0
                  ? `已按 tags 筛选：${tagFilter.join(' / ')}（可多选 tag 取并集）`
                  : '点击上方 tag 快速筛选，或直接搜索选择。'
              }
            >
              <Select
                mode="multiple"
                style={{ width: '100%' }}
                placeholder="选择目标服务器"
                aria-label="目标服务器"
                options={serverOptions}
                showSearch
                optionFilterProp="label"
                notFoundContent={<EmptyState description="暂无服务器类资产" />}
              />
            </Form.Item>
            <Button type="primary" htmlType="submit" loading={createMutation.isPending}>
              创建并分派
            </Button>
          </Form>
        )}
      </Card>

      <Card size="small" title="任务列表">
        {jobsQuery.isError ? (
          <QueryErrorState
            error={jobsQuery.error}
            onRetry={() => {
              void jobsQuery.refetch();
            }}
          />
        ) : (
          <Table<BatchJob>
            rowKey="id"
            size="small"
            loading={jobsQuery.isLoading}
            columns={columns}
            dataSource={jobsQuery.data ?? []}
            locale={{ emptyText: <EmptyState description="暂无批量任务" /> }}
            pagination={false}
          />
        )}
        {jobsQuery.data?.some((job) => isBatchJobActive(job.status)) ? (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            有进行中的任务：列表 15s 自动刷新；打开详情可看逐台实时进度。
          </Typography.Text>
        ) : null}
      </Card>

      <BatchDetailDrawer
        open={jobId !== null}
        jobId={jobId}
        onClose={closeDetail}
        onRetry={(commandText, serverIds) => {
          // 失败行重试：预填新建表单（相同命令 + 失败服务器集合）。
          form.setFieldsValue({ command: commandText, server_ids: serverIds });
          message.info(`已用失败的 ${serverIds.length} 台预填新建表单，确认后重新分派`);
        }}
      />
    </div>
  );
}

function RiskPreview({
  preview,
  error,
}: {
  preview: RiskPreviewResult | undefined;
  error: unknown;
}) {
  if (error) {
    return (
      <Alert
        type="error"
        showIcon
        message="风险预览失败"
        description={describeFailure(failureFromError(error))}
        style={{ marginBottom: 16 }}
      />
    );
  }
  if (!preview) {
    return null;
  }
  const hint =
    preview.risk_level === 'safe'
      ? '全部目标将直接执行'
      : preview.risk_level === 'needs_approval'
        ? '每台目标将创建审批，逐台批准后执行'
        : '命令被风险规则禁止：全部目标将跳过';
  return (
    <Alert
      type={
        preview.risk_level === 'safe'
          ? 'success'
          : preview.risk_level === 'needs_approval'
            ? 'warning'
            : 'error'
      }
      showIcon
      message={
        <Space size={8}>
          <RiskBadge level={preview.risk_level} />
          <span>{hint}</span>
        </Space>
      }
      description={
        <span className="opsmind-admin-meta">
          规则 {preview.rule_id} · {preview.reason}
        </span>
      }
      style={{ marginBottom: 16 }}
    />
  );
}
