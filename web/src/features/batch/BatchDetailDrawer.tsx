// M4 五期：批量任务详情抽屉（CONTRACT.md 8.7 批量页——per-server 表：
// risk_level RiskBadge、status 状态灯、exit_code、输出折叠、错误码中文；
// awaiting_approval 行内「批准/拒绝」直接 POST /api/approvals/:id/decide；
// job 非终态 2s 轮询；取消按钮仅 pending/awaiting_approval 存在时可用；
// 失败行重试=以相同命令+失败 server 集合预填新建表单）。

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { EditOutlined } from '@ant-design/icons';
import {
  Alert,
  Button,
  Collapse,
  Descriptions,
  Drawer,
  Modal,
  Popconfirm,
  Progress,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

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
import { formatDateTime, formatDuration, truncate } from '../../components/admin/format';
import { message } from '../../components/antdMessage';
import ApprovalCommandEditor from '../../components/ApprovalCommandEditor';
import CopyButton from '../../components/CopyButton';
import RiskBadge from '../../components/RiskBadge';

import { describeResultCode } from '../alerts/phase5Codes';
import {
  BATCH_STATUS_LABELS,
  RESULT_STATUS_LABELS,
  canCancelBatch,
  commandCellOf,
  failedServerIds,
  isBatchJobActive,
  progressSummary,
  resultStatusColor,
  type BatchResult,
} from './batchView';

type BatchJobDetail = components['schemas']['BatchJobDetail'];
type ApprovalDecisionResult = components['schemas']['ApprovalDecisionResult'];

export interface BatchDetailDrawerProps {
  open: boolean;
  jobId: string | null;
  onClose: () => void;
  /** 失败行重试：以相同命令+失败 server 预填新建表单。 */
  onRetry: (command: string, serverIds: string[]) => void;
}

export default function BatchDetailDrawer({
  open,
  jobId,
  onClose,
  onRetry,
}: BatchDetailDrawerProps) {
  const queryClient = useQueryClient();
  // 六期打磨 sprint 3：编辑命令的审批 ID（null=未进入编辑态）。
  const [editingApprovalId, setEditingApprovalId] = useState<string | null>(null);

  // 编辑态需要审批对象（原命令/风险级别/server_id）；按钮点击时才取，
  // 与批量详情并行、不参与轮询。
  const editingApprovalQuery = useQuery({
    queryKey: qk.approval(editingApprovalId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/approvals/{id}', {
        params: { path: { id: editingApprovalId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: open && editingApprovalId !== null,
    staleTime: 0,
  });

  const detailQuery = useQuery({
    queryKey: qk.batchJob(jobId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/batch/jobs/{id}', {
        params: { path: { id: jobId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: open && jobId !== null,
    // 契约 8.4：非终态 2s 轮询，终态停止。
    refetchInterval: (query) => {
      const status = query.state.data?.job.status;
      return status && isBatchJobActive(status) ? 2_000 : false;
    },
  });

  function invalidate(): void {
    if (jobId) {
      void queryClient.invalidateQueries({ queryKey: qk.batchJob(jobId) });
    }
    void queryClient.invalidateQueries({ queryKey: qk.batchJobs });
  }

  const decideMutation = useMutation({
    mutationFn: async (input: {
      approvalId: string;
      decision: 'approve' | 'deny';
      /** 六期打磨 sprint 3：编辑态提交的改写后命令（仅 approve 合法）。 */
      editedCommand?: string;
    }) => {
      const { data, error, response } = await api.POST('/api/approvals/{id}/decide', {
        params: { header: writeHeaders(), path: { id: input.approvalId } },
        body: {
          decision: input.decision,
          ...(input.editedCommand !== undefined ? { edited_command: input.editedCommand } : {}),
        },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (result: ApprovalDecisionResult | undefined) => {
      const execution = result?.execution ?? null;
      if (execution) {
        const exitText =
          execution.exit_code === 0
            ? '执行成功'
            : `退出码 ${execution.exit_code}${execution.error_code ? `（${describeResultCode(execution.error_code) || execution.error_code}）` : ''}`;
        // 五期 sprint 3：退出码非 0 = 命令执行失败，此前绿色 success 样式
        // 误导用户以为执行成功。
        if (execution.exit_code === 0) {
          message.success(`命令已执行：${exitText}`);
        } else {
          message.warning(`命令执行失败：${exitText}`);
        }
      } else {
        message.success('审批已更新');
      }
      // 8.4：decide → 失效 batchJob/batchJobs。
      setEditingApprovalId(null);
      invalidate();
    },
    onError: (error) => {
      message.error(describeFailure(failureFromError(error)));
    },
  });

  const cancelMutation = useMutation({
    mutationFn: async () => {
      const { data, error, response } = await api.POST('/api/batch/jobs/{id}/cancel', {
        params: { header: writeHeaders(), path: { id: jobId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      message.success('取消请求已提交');
      invalidate();
    },
    onError: (error) => {
      message.error(describeFailure(failureFromError(error)));
    },
  });

  const detail: BatchJobDetail | undefined = detailQuery.data;
  const results = detail?.results ?? [];
  const progress = progressSummary(results);
  const retryable = failedServerIds(results);

  const columns: TableColumnsType<BatchResult> = [
    {
      title: '',
      key: 'light',
      width: 18,
      render: (_, result) => (
        <span
          aria-label={`状态灯 ${result.server_name}`}
          style={{
            display: 'inline-block',
            width: 10,
            height: 10,
            borderRadius: 5,
            background: resultStatusColor(result.status),
          }}
        />
      ),
    },
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string, result) => name || result.server_id.slice(0, 8),
    },
    {
      // 六期打磨 sprint 5（走查 medium）：每台显示**实际执行**的命令。
      // 用户改写后执行的是 edited_command，此前该列不存在——抽屉里只有
      // 头部的 job 原文命令，用户完全看不出改写痕迹（同刻 API 的
      // results.output 与 approvals.edited_command 都已是改写后命令）。
      // 数据来自 GET /api/batch/jobs/:id 的 results[].edited_command
      // （后端经 approval 关联读出，单 job 50 台不需要逐行回取审批）。
      title: '命令',
      key: 'command',
      render: (_, result) => {
        const cell = commandCellOf(detail?.job.command ?? '', result);
        return (
          <div>
            <Space size={6} wrap>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {cell.prefix}
              </Typography.Text>
              {cell.edited ? <Tag color="purple">用户改写</Tag> : null}
              <CopyButton text={cell.command} label="复制命令" />
            </Space>
            <Tooltip title={cell.command}>
              <div className="opsmind-admin-clamp opsmind-admin-mono">
                {truncate(cell.command, 48)}
              </div>
            </Tooltip>
          </div>
        );
      },
    },
    {
      title: '风险',
      dataIndex: 'risk_level',
      width: 100,
      render: (level: string, result) => (
        <Space size={4}>
          <RiskBadge level={level} />
          {result.rule_id ? (
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {result.rule_id}
            </Typography.Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (status: BatchResult['status']) => RESULT_STATUS_LABELS[status] ?? status,
    },
    {
      title: '退出码',
      dataIndex: 'exit_code',
      width: 80,
      render: (code: number | null) => (code === null ? '—' : code),
    },
    {
      title: '耗时',
      dataIndex: 'duration_ms',
      width: 90,
      render: (value: number) => formatDuration(value),
    },
    {
      title: '错误',
      dataIndex: 'error_code',
      width: 150,
      render: (code: string) => (code ? describeResultCode(code) : '—'),
    },
    {
      title: '操作',
      key: 'actions',
      width: 210,
      render: (_, result) =>
        result.status === 'awaiting_approval' && result.approval_id ? (
          <Space size={4}>
            <Button
              size="small"
              type="primary"
              loading={decideMutation.isPending}
              onClick={() => {
                decideMutation.mutate({
                  approvalId: result.approval_id ?? '',
                  decision: 'approve',
                });
              }}
            >
              批准
            </Button>
            {/* 六期打磨 sprint 3（走查 medium）：批量审批此前只能批准/拒绝，
                六期改写能力只在对话审批卡可用。编辑态复用
                ApprovalCommandEditor（与 ApprovalCard 同一套校验/预览）。
                注意：编辑入口在 decide 前需要审批对象（command/风险级别），
                按 approval_id 现取（GET /api/approvals/{id}）。 */}
            <Button
              size="small"
              icon={<EditOutlined />}
              disabled={decideMutation.isPending}
              onClick={() => setEditingApprovalId(result.approval_id ?? '')}
            >
              编辑
            </Button>
            <Button
              size="small"
              danger
              loading={decideMutation.isPending}
              onClick={() => {
                decideMutation.mutate({
                  approvalId: result.approval_id ?? '',
                  decision: 'deny',
                });
              }}
            >
              拒绝
            </Button>
          </Space>
        ) : null,
    },
  ];

  return (
    <Drawer title="批量任务详情" open={open} onClose={onClose} width={860} destroyOnHidden>
      {detailQuery.isLoading ? (
        <div style={{ padding: 32, textAlign: 'center' }}>加载中……</div>
      ) : detail ? (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Descriptions size="small" column={2} bordered>
            <Descriptions.Item label="状态" span={1}>
              <Tag
                color={
                  detail.job.status === 'done'
                    ? 'green'
                    : detail.job.status === 'partial'
                      ? 'orange'
                      : detail.job.status === 'running'
                        ? 'blue'
                        : 'default'
                }
              >
                {BATCH_STATUS_LABELS[detail.job.status] ?? detail.job.status}
              </Tag>
              {isBatchJobActive(detail.job.status) ? (
                <Typography.Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
                  2s 自动刷新
                </Typography.Text>
              ) : null}
            </Descriptions.Item>
            <Descriptions.Item label="创建时间" span={1}>
              {formatDateTime(detail.job.created_at)}
            </Descriptions.Item>
            <Descriptions.Item label="命令" span={2}>
              {/* 契约 8.7 ⑨（FE-P2 六期）：批量每条命令复制。 */}
              <span className="opsmind-admin-mono">
                {detail.job.command} <CopyButton text={detail.job.command} label="复制命令" />
              </span>
            </Descriptions.Item>
            {detail.job.description ? (
              <Descriptions.Item label="意图描述" span={2}>
                {detail.job.description}
              </Descriptions.Item>
            ) : null}
          </Descriptions>

          <div>
            <Progress
              percent={
                progress.total === 0 ? 0 : Math.round((progress.done / progress.total) * 100)
              }
              status={isBatchJobActive(detail.job.status) ? 'active' : 'normal'}
              format={() => `${progress.done}/${progress.total}`}
            />
            <Space size={16} wrap style={{ marginTop: 4 }}>
              <Typography.Text type="secondary">
                待审批 {progress.awaiting} · 失败 {progress.failed}
              </Typography.Text>
              {canCancelBatch(results) ? (
                <Popconfirm
                  title="取消批量任务"
                  description="仅取消待处理/待审批的服务器；执行中的命令不会被中断。"
                  okText="取消任务"
                  okButtonProps={{ danger: true }}
                  onConfirm={() => {
                    cancelMutation.mutate();
                  }}
                >
                  <Button danger size="small" loading={cancelMutation.isPending}>
                    取消任务
                  </Button>
                </Popconfirm>
              ) : null}
              {retryable.length > 0 && detail.job.command ? (
                <Button
                  size="small"
                  onClick={() => {
                    onRetry(detail.job.command, retryable);
                    onClose();
                  }}
                >
                  重试失败项（{retryable.length} 台）
                </Button>
              ) : null}
            </Space>
          </div>

          {detail.job.source === 'chat' ? (
            <Alert type="info" showIcon message="该任务来自对话中的 batch_execute 工具。" />
          ) : null}

          <Table<BatchResult>
            rowKey="id"
            size="small"
            columns={columns}
            dataSource={results}
            pagination={false}
            expandable={{
              rowExpandable: (result) => result.output !== '',
              expandedRowRender: (result) => (
                <Collapse
                  items={[
                    {
                      key: 'output',
                      label: (
                        <Space size={6}>
                          <span>{`执行输出${result.truncated ? '（已截断）' : ''}`}</span>
                          {/* 契约 8.7 ⑨（FE-P2 六期）：每台输出复制——统一
                              CopyButton 取代 antd Typography copyable（8.2）。
                              六期打磨 sprint 4（走查 medium）：按钮在 Collapse
                              label 区内，点击会冒泡到 header 触发开合（实测
                              点一次「复制输出」输出面板就展开）；外层 span
                              拦下 click/keydown 冒泡。 */}
                          <span
                            onClick={(event) => event.stopPropagation()}
                            onKeyDown={(event) => event.stopPropagation()}
                          >
                            <CopyButton text={result.output} label="复制输出" />
                          </span>
                        </Space>
                      ),
                      children: (
                        <Typography.Paragraph style={{ whiteSpace: 'pre-wrap', marginBottom: 0 }}>
                          <div
                            style={{
                              contentVisibility: 'auto',
                              containIntrinsicSize: 'auto 300px',
                            }}
                          >
                            {result.output}
                          </div>
                        </Typography.Paragraph>
                      ),
                    },
                  ]}
                />
              ),
            }}
          />
        </Space>
      ) : detailQuery.isError ? (
        <Alert
          type="error"
          showIcon
          message={describeFailure(failureFromError(detailQuery.error))}
          action={
            <Button
              size="small"
              onClick={() => {
                void detailQuery.refetch();
              }}
            >
              重试
            </Button>
          }
        />
      ) : null}

      {/* 六期打磨 sprint 3：批量审批行内「编辑」→ 编辑命令并保存批准。
          与对话审批卡同一套校验（空草稿不可提交）/preview-risk 预览/请求体
          构造（approvalEdit.buildDecideBody）。 */}
      <Modal
        open={editingApprovalId !== null}
        title="编辑命令并批准"
        onCancel={() => setEditingApprovalId(null)}
        footer={null}
        destroyOnHidden
      >
        {editingApprovalQuery.isLoading ? (
          <div style={{ padding: 16, textAlign: 'center' }}>加载审批…</div>
        ) : editingApprovalQuery.isError ? (
          <Alert
            type="error"
            showIcon
            message={describeFailure(failureFromError(editingApprovalQuery.error))}
          />
        ) : editingApprovalQuery.data ? (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Typography.Text type="secondary">
              目标服务器：
              <Typography.Text strong>
                {editingApprovalQuery.data.server_name || '未知'}
              </Typography.Text>
            </Typography.Text>
            <Space size={8} wrap>
              <Typography.Text type="secondary">原命令：</Typography.Text>
              <span className="opsmind-admin-mono">{editingApprovalQuery.data.command}</span>
              <CopyButton text={editingApprovalQuery.data.command} label="复制原命令" />
              <RiskBadge level={editingApprovalQuery.data.risk_level} />
            </Space>
            <ApprovalCommandEditor
              command={editingApprovalQuery.data.command}
              riskLevel={editingApprovalQuery.data.risk_level}
              serverId={editingApprovalQuery.data.server_id}
              pending={decideMutation.isPending}
              disabled={editingApprovalQuery.data.status !== 'pending'}
              onSubmit={(body) => {
                if (editingApprovalId === null) {
                  return;
                }
                decideMutation.mutate({
                  approvalId: editingApprovalId,
                  decision: 'approve',
                  ...(body.edited_command !== undefined
                    ? { editedCommand: body.edited_command }
                    : {}),
                });
              }}
            />
          </Space>
        ) : null}
      </Modal>
    </Drawer>
  );
}
