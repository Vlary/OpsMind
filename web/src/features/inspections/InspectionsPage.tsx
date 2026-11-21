// M4 五期：巡检页（CONTRACT.md 8.1 /inspections、8.7——任务表 name/cron(UTC)/
// 目标 tags/template/enabled/next_run，行操作启停/立即运行/编辑/删除；
// 报告列表（存在未完成报告时 10s 轮询）；右上「立即巡检」即时入口）。

import {
  CaretRightOutlined,
  DeleteOutlined,
  EditOutlined,
  PlusOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Flex, Popconfirm, Space, Switch, Table, Tabs, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import { message } from '../../components/antdMessage';
import { useNotificationChannels } from '../alerts/useNotificationChannels';
import { describePhase5Failure } from '../alerts/phase5Codes';
import '../../styles/admin.css';

import InspectionScoreTrendChart from '../monitor/charts/InspectionScoreTrendChart';
import { pickDefaultJobId } from '../monitor/charts/chartModel';

import JobFormDrawer from './JobFormDrawer';
import ReportDrawer from './ReportDrawer';
import RunNowModal from './RunNowModal';
import { TEMPLATE_LABELS, scoreColor } from './inspectionView';

type InspectionJob = components['schemas']['InspectionJob'];
type InspectionReport = components['schemas']['InspectionReport'];

export default function InspectionsPage() {
  const [activeTab, setActiveTab] = useState('jobs');
  const [jobDrawer, setJobDrawer] = useState<{ open: boolean; job: InspectionJob | null }>({
    open: false,
    job: null,
  });
  const [createOpen, setCreateOpen] = useState(false);
  const [reportId, setReportId] = useState<string | null>(null);
  const [runNowOpen, setRunNowOpen] = useState(false);

  const channelsQuery = useNotificationChannels();
  const channels = channelsQuery.data ?? [];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="巡检"
        description="cron 定时任务与即时巡检；规则评分报告支持与上次对比。"
        extra={
          <Button
            type="primary"
            icon={<ThunderboltOutlined />}
            onClick={() => {
              setRunNowOpen(true);
            }}
          >
            立即巡检
          </Button>
        }
      />
      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={[
          {
            key: 'jobs',
            label: '任务',
            children: (
              <JobsTab
                onEdit={(job) => setJobDrawer({ open: true, job })}
                onCreate={() => setCreateOpen(true)}
              />
            ),
          },
          { key: 'reports', label: '报告', children: <ReportsTab onOpen={setReportId} /> },
        ]}
      />

      <JobFormDrawer
        key={jobDrawer.job?.id ?? 'new'}
        open={jobDrawer.open || createOpen}
        job={jobDrawer.job}
        channels={channels}
        onClose={() => {
          setJobDrawer({ open: false, job: null });
          setCreateOpen(false);
        }}
      />
      <ReportDrawer
        key={reportId ?? 'none'}
        open={reportId !== null}
        reportId={reportId}
        onClose={() => {
          setReportId(null);
        }}
      />
      <RunNowModal
        key={String(runNowOpen)}
        open={runNowOpen}
        onClose={() => {
          setRunNowOpen(false);
        }}
        onReport={(id) => {
          setReportId(id);
        }}
      />
    </div>
  );
}

function JobsTab({
  onEdit,
  onCreate,
}: {
  onEdit: (job: InspectionJob) => void;
  onCreate: () => void;
}) {
  const queryClient = useQueryClient();
  const [busyId, setBusyId] = useState<string | null>(null);

  const jobsQuery = useQuery({
    queryKey: qk.inspectionJobs,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/inspection/jobs');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });

  const toggleMutation = useMutation({
    mutationFn: async (input: { job: InspectionJob; enabled: boolean }) => {
      const { data, error, response } = await api.PATCH('/api/inspection/jobs/{id}', {
        params: { header: writeHeaders(), path: { id: input.job.id } },
        body: { enabled: input.enabled },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onMutate: (input) => {
      setBusyId(input.job.id);
    },
    onSettled: () => {
      setBusyId(null);
    },
    onSuccess: (_, input) => {
      message.success(input.enabled ? '任务已启用' : '任务已停用');
      void queryClient.invalidateQueries({ queryKey: qk.inspectionJobs });
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const runMutation = useMutation({
    mutationFn: async (jobId: string) => {
      const { error, response } = await api.POST('/api/inspection/jobs/{id}/run', {
        params: { header: writeHeaders(), path: { id: jobId } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onMutate: (jobId) => {
      setBusyId(jobId);
    },
    onSettled: () => {
      setBusyId(null);
    },
    onSuccess: () => {
      message.success('已触发巡检：报告稍后出现在报告列表');
      void queryClient.invalidateQueries({ queryKey: qk.inspectionJobs });
      void queryClient.invalidateQueries({ queryKey: qk.inspectionReportsPrefix });
      // 窄竞态补拉（五期打磨 sprint 2）：POST /run 202 返回时后端 goroutine
      // 尚未 INSERT 报告行（实测约 1.3s 后落库）；上方立即 invalidate 的
      // refetch 若落在落库前会拿到空列表——ReportsTab 的 refetchInterval
      // 只在「存在 finished_at=null 的报告」时开启，空列表即停轮询，新报告
      // 不会自动出现。延迟 4s 再补一次 invalidate 覆盖落库窗口（幂等，
      // 组件卸载后触发也无害）。
      window.setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: qk.inspectionReportsPrefix });
      }, 4_000);
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (jobId: string) => {
      const { error, response } = await api.DELETE('/api/inspection/jobs/{id}', {
        params: { header: writeHeaders(), path: { id: jobId } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
    },
    onMutate: (jobId) => {
      setBusyId(jobId);
    },
    onSettled: () => {
      setBusyId(null);
    },
    onSuccess: () => {
      message.success('任务已删除');
      void queryClient.invalidateQueries({ queryKey: qk.inspectionJobs });
      // 报告级联删除后同步失效报告缓存（五期 sprint 3 走查）：Tabs 面板
      // 不销毁不重挂，ReportsTab 的 useQuery 在无未完成报告时
      // refetchInterval=false——不失效则切 Tab 仍显示已删任务的幽灵行，
      // 点「详情」404。
      void queryClient.invalidateQueries({ queryKey: qk.inspectionReportsPrefix });
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  const columns: TableColumnsType<InspectionJob> = [
    {
      title: '名称',
      dataIndex: 'name',
      render: (name: string, job) => (
        <Space orientation="vertical" size={0}>
          <span className="opsmind-admin-cell-title">{name}</span>
          <span className="opsmind-admin-cell-sub">
            上次 {job.last_run_at ? formatDateTime(job.last_run_at) : '未运行'}
          </span>
        </Space>
      ),
    },
    {
      title: 'cron（UTC）',
      dataIndex: 'cron',
      render: (cron: string) => <span className="opsmind-admin-mono">{cron}</span>,
    },
    {
      title: '下次运行',
      dataIndex: 'next_run_at',
      width: 170,
      render: (value: string | null) => (value ? formatDateTime(value) : '—'),
    },
    {
      title: '目标',
      dataIndex: 'target_tags',
      render: (tags: string[]) =>
        tags.length > 0 ? (
          <Space size={4} wrap>
            {tags.map((tag) => (
              <Tag key={tag}>{tag}</Tag>
            ))}
          </Space>
        ) : (
          <Typography.Text type="secondary">全部服务器</Typography.Text>
        ),
    },
    {
      title: '模板',
      dataIndex: 'template',
      width: 110,
      render: (template: string) => TEMPLATE_LABELS[template] ?? template,
    },
    {
      title: '启用',
      dataIndex: 'enabled',
      width: 72,
      render: (enabled: boolean, job) => (
        <Switch
          size="small"
          checked={enabled}
          loading={busyId === job.id}
          aria-label={`启用任务 ${job.name}`}
          onChange={(checked) => {
            toggleMutation.mutate({ job, enabled: checked });
          }}
        />
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 140,
      render: (_, job) => (
        <Space size={0}>
          <Button
            size="small"
            type="text"
            icon={<CaretRightOutlined />}
            aria-label={`立即运行 ${job.name}`}
            loading={busyId === job.id}
            onClick={() => {
              runMutation.mutate(job.id);
            }}
          />
          <Button
            size="small"
            type="text"
            icon={<EditOutlined />}
            aria-label={`编辑任务 ${job.name}`}
            onClick={() => {
              onEdit(job);
            }}
          />
          <Popconfirm
            title="删除巡检任务"
            description={`确定删除「${job.name}」？历史报告将级联删除。`}
            okText="删除"
            okButtonProps={{ danger: true }}
            onConfirm={() => {
              deleteMutation.mutate(job.id);
            }}
          >
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              aria-label={`删除任务 ${job.name}`}
            />
          </Popconfirm>
        </Space>
      ),
    },
  ];

  if (jobsQuery.isError) {
    return (
      <QueryErrorState
        error={jobsQuery.error}
        onRetry={() => {
          void jobsQuery.refetch();
        }}
      />
    );
  }

  return (
    <Flex vertical gap={12}>
      <Flex justify="flex-end">
        <Button type="primary" icon={<PlusOutlined />} onClick={onCreate}>
          新建任务
        </Button>
      </Flex>
      <Table<InspectionJob>
        rowKey="id"
        size="small"
        loading={jobsQuery.isLoading}
        columns={columns}
        dataSource={jobsQuery.data ?? []}
        locale={{ emptyText: <EmptyState description="暂无巡检任务" /> }}
        pagination={false}
      />
    </Flex>
  );
}

function ReportsTab({ onOpen }: { onOpen: (reportId: string) => void }) {
  const reportsQuery = useQuery({
    queryKey: qk.inspectionReports({}),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/inspection/reports', {
        params: { query: { limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    // 契约 8.4：存在无 finished_at 的报告时 10s 轮询，否则停。
    refetchInterval: (query) => {
      const items = query.state.data ?? [];
      return items.some((report: InspectionReport) => report.finished_at === null) ? 10_000 : false;
    },
  });

  // 任务下拉与 JobsTab 共用 qk.inspectionJobs 缓存（图表化本轮）。
  const jobsQuery = useQuery({
    queryKey: qk.inspectionJobs,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/inspection/jobs');
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
  });
  const jobs = useMemo(
    () => (jobsQuery.data ?? []).map((job) => ({ id: job.id, name: job.name })),
    [jobsQuery.data],
  );
  // 默认「最近一份报告所属任务」：报告按 created_at DESC，取首条即可。
  // 走查第 6 轮 high：最近一份是即时巡检（job_id=null）时默认「全部任务」，
  // 否则只会按某个定时任务的 job_id 查询，图空而下方表格有报告。
  const latestReport = reportsQuery.data?.[0];
  const defaultJobId =
    latestReport !== undefined && latestReport.job_id === null
      ? null
      : pickDefaultJobId(jobs, latestReport?.job_id ?? null);
  // undefined = 未手动选择；null = 用户明确选择「全部任务」。
  const [jobChoice, setJobChoice] = useState<string | null | undefined>(undefined);
  // 任务被删除后选择可能失效：回落到默认任务，避免带着不存在的 job_id 发请求。
  const validChoice =
    jobChoice !== undefined && (jobChoice === null || jobs.some((job) => job.id === jobChoice))
      ? jobChoice
      : undefined;
  const trendJobId = validChoice === undefined ? defaultJobId : validChoice;

  const trendQuery = useQuery({
    queryKey: qk.inspectionTrend(trendJobId, 30),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/inspection/trend', {
        params: {
          query: trendJobId === null ? { limit: 30 } : { job_id: trendJobId, limit: 30 },
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    // job_id 缺省 = 全部报告（含即时巡检）；等任务/报告列表就绪再发，
    // 避免先用 null 查一次「全部」再按默认任务重查。走查第 6 轮 high：
    // 此前 trendJobId 为 null 时直接不发请求，只有即时报告时图恒空。
    enabled: jobsQuery.isFetched && reportsQuery.isFetched,
    refetchInterval: reportsQuery.data?.some((report) => report.finished_at === null)
      ? 10_000
      : false,
  });

  const columns: TableColumnsType<InspectionReport> = [
    {
      title: '任务',
      dataIndex: 'job_name',
      render: (name: string) => name || '即时巡检',
    },
    {
      title: '均分',
      dataIndex: 'avg_score',
      width: 90,
      render: (score: number) => <Tag color={scoreColor(score)}>{score}</Tag>,
    },
    {
      title: '覆盖 / 异常',
      key: 'coverage',
      width: 120,
      render: (_, report) => `${report.server_count} 台 / ${report.anomaly_count} 项`,
    },
    {
      title: '时间',
      dataIndex: 'created_at',
      width: 170,
      render: (value: string, row) => (
        <Space orientation="vertical" size={0}>
          <span>{formatDateTime(value)}</span>
          <span className="opsmind-admin-cell-sub">
            {row.finished_at === null ? '进行中' : '已完成'}
          </span>
        </Space>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 90,
      render: (_, report) => (
        <Button
          size="small"
          onClick={() => {
            onOpen(report.id);
          }}
        >
          详情
        </Button>
      ),
    },
  ];

  if (reportsQuery.isError) {
    return (
      <QueryErrorState
        error={reportsQuery.error}
        onRetry={() => {
          void reportsQuery.refetch();
        }}
      />
    );
  }

  return (
    <Flex vertical gap={12}>
      <Card size="small" title="均分走势">
        {/* key：数据就绪后重挂载一次（图表在 skeleton 阶段挂载会错过
            ResizeObserver，宽度停在 fallback）。loading 只反映图表自身
            请求，任务/报告列表的加载不阻塞既有图表。 */}
        <InspectionScoreTrendChart
          key={trendQuery.data?.points.length ? 'trend-ready' : 'trend-pending'}
          points={trendQuery.data?.points ?? []}
          jobs={jobs}
          jobId={trendJobId}
          defaultJobId={defaultJobId}
          onJobChange={(nextJobId) => {
            setJobChoice(nextJobId);
          }}
          truncated={trendQuery.data?.truncated ?? false}
          loading={trendQuery.isLoading || !trendQuery.isFetched}
          error={
            trendQuery.isError ? (
              <QueryErrorState
                error={trendQuery.error}
                onRetry={() => {
                  void trendQuery.refetch();
                }}
              />
            ) : undefined
          }
        />
      </Card>
      <Card size="small">
        <Table<InspectionReport>
          rowKey="id"
          size="small"
          loading={reportsQuery.isLoading}
          columns={columns}
          dataSource={reportsQuery.data ?? []}
          // 走查第 3 轮 medium：390 宽下「每日基线巡检」被压成逐字竖排。
          // 固定 900px 表宽后横向滚动，任务列至少 ~430px。
          scroll={{ x: 900 }}
          tableLayout="fixed"
          locale={{ emptyText: <EmptyState description="暂无巡检报告" /> }}
          pagination={false}
        />
      </Card>
    </Flex>
  );
}
