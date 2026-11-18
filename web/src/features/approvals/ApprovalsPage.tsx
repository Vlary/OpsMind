// 审批队列页（00012 RBAC，契约 12.2：审批者只审批不发起）。
//
// 此前审批者登录后导航里没有任何审批入口，只能看到资产页、无法履职
//（评审 high）。本页列出 pending 审批，复用对话页 ApprovalCard 的
// 批准/拒绝交互（同一 decide 端点、同一职责分离约束：不能审批自己发起
// 的请求，服务端强制）。审批时禁止改写命令，需修改请拒绝后由发起人重发。
//
// 第 2 轮走查（low）：批准一条终端授权后，该行从 pending 队列消失、
// 批准者侧再也看不到自己刚做了什么（也不知道授权还在 30 分钟有效期内）。
// 这里补一个「最近已决策」区块（approved/denied/expired 各取最近一页），
// 终端授权额外展示有效期与是否已被消费。

import { ReloadOutlined } from '@ant-design/icons';
import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Card, Space, Spin, Table, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import { ApiRequestError, failureFromError, toApiFailure } from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import ApprovalCard, { approvalCardTitle } from '../../components/ApprovalCard';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import StatusTag from '../../components/StatusTag';
import { useGlobalApprovalMode } from '../settings/useApprovalSettings';
import { approvalOutcome, type ApprovalOutcome } from './approvalOutcome';
import '../../styles/admin.css';

type Approval = components['schemas']['Approval'];

const APPROVAL_QUEUE_LIMIT = 100;
// 已决策列表只做「刚刚发生了什么」的回看，条数与队列同量级即可（有界）。
const DECIDED_LIMIT = 50;

const approvalQueueKey = ['approvals', 'queue', 'pending'] as const;
const decidedApprovalsKey = ['approvals', 'queue', 'decided'] as const;

/**
 * 已决策审批的简要行（第 2 轮走查 low）。
 *
 * 只渲染可核对的字段：类型、目标、状态、决策时间；终端授权额外标注
 * 「待消费 / 已消费 / 已过期」——批准者能看到自己给的授权是否已被使用，
 * 发起人也能据此确认下一步该去终端页。
 */
function decidedDescription(approval: Approval): string {
  if (approval.source === 'terminal') {
    const expires = approval.grant_expires_at ? formatDateTime(approval.grant_expires_at) : '';
    if (approval.consumed_at) {
      return `已于 ${formatDateTime(approval.consumed_at)} 消费`;
    }
    if (approval.grant_expires_at && new Date(approval.grant_expires_at).getTime() <= Date.now()) {
      return `授权已过期${expires ? `（有效期至 ${expires}）` : ''}`;
    }
    return `待发起人使用${expires ? `，有效期至 ${expires}` : ''}`;
  }
  if (approval.source === 'policy') {
    return `策略：${approval.policy_from ?? '—'} → ${approval.policy_to ?? '—'}`;
  }
  return approval.command || '—';
}

export default function ApprovalsPage() {
  // 第 8 轮走查 medium：decide 成功后该审批立即离开 pending 队列（卡片被
  // 卸载），而 execution 只存在于 decide 响应里。把最近一次决定的结果留在
  // 页面顶部，审批者才能看到 exit_code/error_code，而不是只闪过「已批准」。
  const [lastDecision, setLastDecision] = useState<{
    id: string;
    title: string;
    outcome: ApprovalOutcome;
  } | null>(null);
  const approvalsQuery = useQuery({
    queryKey: approvalQueueKey,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/approvals', {
        params: { query: { status: 'pending', limit: APPROVAL_QUEUE_LIMIT } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    refetchInterval: 10_000,
  });

  // 最近已决策（三种终态合并，按 requested_at 倒序由后端给出）。
  const decidedQuery = useQuery({
    queryKey: decidedApprovalsKey,
    queryFn: async () => {
      const statuses = ['approved', 'denied', 'expired'] as const;
      const pages = await Promise.all(
        statuses.map(async (status) => {
          const { data, error, response } = await api.GET('/api/approvals', {
            params: { query: { status, limit: DECIDED_LIMIT } },
          });
          if (error) {
            throw new ApiRequestError(toApiFailure(error, response));
          }
          return data?.items ?? [];
        }),
      );
      return pages
        .flat()
        .sort((a, b) => new Date(b.requested_at).getTime() - new Date(a.requested_at).getTime())
        .slice(0, DECIDED_LIMIT);
    },
    refetchInterval: 15_000,
  });

  const items: Approval[] = approvalsQuery.data ?? [];
  const decided: Approval[] = decidedQuery.data ?? [];
  // 降级态（单管理员/无第二审批者）：GET /api/settings 的只读字段，与启动
  // 告警 rbac_single_admin_warning 及 requireSecondApprover 同源。
  const settingsQuery = useGlobalApprovalMode();

  const decidedColumns: TableColumnsType<Approval> = [
    {
      title: '类型',
      dataIndex: 'source',
      width: 120,
      render: (_, approval) =>
        approval.source === 'terminal'
          ? approval.grant_kind === 'rdp'
            ? 'RDP 授权'
            : '终端授权'
          : approval.source === 'policy'
            ? '审批门限变更'
            : '命令审批',
    },
    {
      title: '发起人',
      dataIndex: 'requested_by_name',
      width: 140,
      render: (name: string) => name || '—',
    },
    {
      title: '内容',
      key: 'description',
      render: (_, approval) => (
        <Typography.Text ellipsis={{ tooltip: decidedDescription(approval) }}>
          {decidedDescription(approval)}
        </Typography.Text>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (_, approval) => <StatusTag status={approval.status} />,
    },
    {
      title: '决策时间',
      dataIndex: 'decided_at',
      width: 180,
      render: (value: string | null) => (value ? formatDateTime(value) : '—'),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="审批"
        description="待处理审批；聊天与终端/RDP 授权批准后由发起人执行，控制台/批量/诊断修复类审批在批准时由服务端立即执行并回填结果（可能失败），策略变更批准即生效。审批人不能改写命令或审批自己发起的请求。"
        extra={
          <Button
            icon={<ReloadOutlined />}
            loading={approvalsQuery.isFetching}
            onClick={() => {
              void approvalsQuery.refetch();
              void decidedQuery.refetch();
            }}
          >
            刷新
          </Button>
        }
      />

      {approvalsQuery.isError ? (
        <ApiErrorAlert failure={failureFromError(approvalsQuery.error)} />
      ) : null}

      {/* 第 8 轮走查 medium：决定结果（尤其 console/batch 批准时服务端
          同步执行产生的 exit_code/error_code）在卡片离开 pending 队列后
          仍然可见。 */}
      {lastDecision ? (
        <Alert
          data-testid="approval-decision-feedback"
          type={
            lastDecision.outcome.kind === 'failed'
              ? 'error'
              : lastDecision.outcome.kind === 'denied'
                ? 'warning'
                : 'success'
          }
          showIcon
          closable
          onClose={() => setLastDecision(null)}
          style={{ marginBottom: 12 }}
          message={`刚刚处理「${lastDecision.title}」：${lastDecision.outcome.message}`}
          description={
            lastDecision.outcome.detail
              ? `执行明细：${lastDecision.outcome.detail}（最终状态以服务端为准）`
              : '最终状态以服务端为准。'
          }
        />
      ) : null}

      {/* 走查第 3 轮 medium：降级时页顶先给出中文说明（此前审批人只能在点
          「批准」后看到一段英文 409，既读不懂也不知道该找谁）。第 4 轮
          走查 medium：标题改为「可审批账号不足」——判定口径是
          admin+approver 的启用账号数，不是管理员人数。 */}
      {settingsQuery.data?.single_admin_degraded ? (
        <Alert
          type="warning"
          showIcon
          data-testid="degraded-mode-banner"
          style={{ marginBottom: 12 }}
          message="可审批账号不足降级模式：审批不可批准"
          description={`系统中可承担审批职责的启用账号只有 ${
            settingsQuery.data.enabled_approvers ?? 1
          } 个（管理员 + 审批者合计）。请管理员在「用户」页创建至少一个启用的审批者（approver）；紧急情况下可由主机侧执行 aiops break-glass 单次批准。降级期间仍可拒绝待处理审批。`}
        />
      ) : null}

      {approvalsQuery.isPending ? (
        <div className="app-center">
          <Spin />
        </div>
      ) : null}

      {approvalsQuery.isSuccess && items.length === 0 ? (
        <EmptyState description="当前没有待处理的审批" />
      ) : null}

      {items.length > 0 ? (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          {items.map((approval) => (
            <div key={approval.id}>
              <Typography.Text type="secondary">
                {approval.requested_by_name ? `发起人：${approval.requested_by_name}` : null}
              </Typography.Text>
              <ApprovalCard
                approval={approval}
                conversationId={approval.conversation_id ?? ''}
                onUpdated={(decided, execution) => {
                  setLastDecision({
                    id: decided.id,
                    title: approvalCardTitle(decided),
                    outcome: approvalOutcome(decided, execution),
                  });
                  void approvalsQuery.refetch();
                  void decidedQuery.refetch();
                }}
              />
            </div>
          ))}
        </Space>
      ) : null}

      {items.length >= APPROVAL_QUEUE_LIMIT ? (
        <Card size="small" style={{ marginTop: 12 }}>
          <Typography.Text type="secondary">
            仅显示最近 {APPROVAL_QUEUE_LIMIT} 条待处理审批；请先处理当前队列。
          </Typography.Text>
        </Card>
      ) : null}

      <div data-testid="decided-approvals" style={{ marginTop: 16 }}>
        <Card
          size="small"
          title="最近已决策"
          extra={
            <Typography.Text type="secondary">
              最近 {DECIDED_LIMIT} 条；终端/桌面授权在有效期内仍可被发起人使用
            </Typography.Text>
          }
        >
          {decidedQuery.isError ? (
            <Alert
              type="warning"
              showIcon
              message="已决策列表加载失败"
              description={failureFromError(decidedQuery.error).message}
            />
          ) : (
            <Table<Approval>
              rowKey="id"
              size="small"
              loading={decidedQuery.isPending}
              columns={decidedColumns}
              dataSource={decided}
              locale={{ emptyText: <EmptyState description="还没有已决策的审批" /> }}
              pagination={{ pageSize: 10, hideOnSinglePage: true }}
            />
          )}
          {decided.some(
            (approval) => approval.source === 'terminal' && approval.status === 'approved',
          ) ? (
            <Tag color="blue" style={{ marginTop: 8 }}>
              终端/桌面授权批准后由发起人自行打开会话消费
            </Tag>
          ) : null}
        </Card>
      </div>
    </div>
  );
}
