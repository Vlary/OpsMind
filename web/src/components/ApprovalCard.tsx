import {
  CheckOutlined,
  ClockCircleOutlined,
  CloseOutlined,
  FileSearchOutlined,
} from '@ant-design/icons';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Collapse, Space, Tag, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { api } from '../api/client';
import { qk } from '../api/queryKeys';
import type { components } from '../api/schema';
import { approvalOutcome } from '../features/approvals/approvalOutcome';
import { useMe } from '../features/auth/useMe';
import { useGlobalApprovalMode } from '../features/settings/useApprovalSettings';
import { describeError, toApiErrorInfo } from '../features/chat/useChatStream';
import { approvalDecisionState } from '../lib/approvalDecision';
import { hasAnyPermission, PERMS } from '../lib/rbacPermissions';
import { ApprovalModeTag } from './ApprovalModeSelect';
import { normalizeApprovalMode } from './approvalMode';
import ApprovalContextDrawer from './ApprovalContextDrawer';
import { APPROVAL_STATUS_LABELS } from './approvalStatus';
import CodeBlock from './CodeBlock';
import CopyButton from './CopyButton';
import { describeRiskRule } from './riskRules';
import RiskBadge from './RiskBadge';
import StatusTag from './StatusTag';

type Approval = components['schemas']['Approval'];
type ApprovalStatus = components['schemas']['ApprovalStatus'];
type ApprovalExecution = components['schemas']['ApprovalExecution'];

export interface ApprovalCardProps {
  approval: Approval;
  conversationId: string;
  /** HTTP 决策成功后的即时反馈；最终状态仍以 SSE approval_resolved 为准。
   *  execution 仅 source=console/batch 且批准时非 null（服务端已在决定时
   *  同步执行），调用方据此展示 exit_code/error_code。 */
  onUpdated?: (approval: Approval, execution: ApprovalExecution | null) => void;
}

// export 仅作单测钩子（审批状态机纯函数，__tests__/approvalCard.test.ts）。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function formatRemaining(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}

// expired 用中性文案「已过期」（六期打磨 sprint 4 走查 low）：approvals 的
// expired 有两种来源——TTL 到时与回答流断开/服务关闭时的 Cancel
// （note=stream_cancelled / server_shutting_down）。同一张卡上方的
// StatusTag（components/StatusTag.tsx）与回放侧
// （features/chat/replayPlayer.ts 的 approvalStatusLabel）早已统一为
// 「已过期」，此前只有本表仍写「已超时」，同一屏出现两种措辞
// （标签「已过期」+ 正文「审批已超时」）。此处对齐，正文与 Tag 同源。
// 审批状态中文标签见 components/approvalStatus.ts（六期打磨 sprint 4：
// expired 统一为「已过期」，与 StatusTag / 回放同源）。

// 审批 Cancel（internal/approvals/service.go）落库的 note 是内部标识；
// 直接展示会把「流断开自动取消」误读成「等满 120s 超时」。已知值映射
// 中文，未知值原样保留（不丢排查信息）。
const noteLabel: Record<string, string> = {
  stream_cancelled: '回答流已中断，本轮审批自动取消',
  server_shutting_down: '服务关闭，本轮审批自动取消',
};

/** expired 且带取消 note：并非 TTL 超时，而是回答流断开/服务关闭连带取消。 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function isCancelledApproval(approval: Approval): boolean {
  return (
    approval.status === 'expired' &&
    (approval.note === 'stream_cancelled' || approval.note === 'server_shutting_down')
  );
}

/** 审批类型标题（00012：审批者队列也会出现终端 grant / 策略变更）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function approvalCardTitle(approval: Approval): string {
  switch (approval.source) {
    case 'terminal':
      return approval.grant_kind === 'rdp' ? 'RDP 授权' : '终端授权';
    case 'policy':
      return '审批门限变更';
    default:
      return '命令审批';
  }
}

/**
 * 审批卡正文是否含「待执行命令」区（第 2 轮走查 medium）。
 *
 * policy 审批（全局门限变更）与 terminal 审批（JIT 授权）在设计上 command
 * 恒为空串（internal/httpapi/settings_handlers.go / grant_handlers.go 都传
 * Command:""）。此前对所有非 legacy 行一律渲染「待执行命令」+ 空代码块，
 * 审批者会以为漏了命令内容。这里按 source 显式声明，渲染分支据此收敛：
 *   - command：chat/console/batch，有真实命令（可能含历史改写双行）；
 *   - policy：只渲染「策略：from → to」；
 *   - terminal：只渲染目标服务器与授权时长。
 */
export type ApprovalBodyKind = 'command' | 'policy' | 'terminal';

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function approvalBodyKind(source: Approval['source']): ApprovalBodyKind {
  switch (source) {
    case 'policy':
      return 'policy';
    case 'terminal':
      return 'terminal';
    default:
      return 'command';
  }
}

/**
 * 审批卡是否应因单管理员降级而禁用「批准」并展示说明（走查第 3 轮
 * medium）。
 *
 * 单管理员/无第二审批者时，服务端对 approve 一律 409 NO_SECOND_APPROVER。
 * 前端从 GET /api/settings 的只读降级态提前知道该事实：渲染中文说明 +
 * 置灰批准按钮，而不是让审批人反复点、每次吃一个英文 409。
 *
 * 只禁「批准」：拒绝不扩大执行面，服务端也放行（deny 不经过
 * requireSecondApprover），降级期间仍需能拒绝待办。
 *
 * degraded 为 undefined（存储不可用/未加载）时不禁用——权威判定在服务端，
 * 前端不得因未知状态隐藏入口。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function approvalApproveBlockedByDegradedMode(
  status: ApprovalStatus,
  singleAdminDegraded: boolean | undefined,
): boolean {
  return status === 'pending' && singleAdminDegraded === true;
}

export default function ApprovalCard({ approval, conversationId, onUpdated }: ApprovalCardProps) {
  const queryClient = useQueryClient();
  // qk.me() 与认证门/壳层共用缓存，不额外发请求；权限与用户 id 均来自
  // 服务端下发的 MeResponse（00012）。
  const meQuery = useMe();
  // 降级态（单管理员/无第二审批者）：GET /api/settings 的只读字段，与
  // 服务端 requireSecondApprover 同源。undefined=未知（不禁用按钮）。
  const settingsQuery = useGlobalApprovalMode();
  const singleAdminDegraded = settingsQuery.data?.single_admin_degraded;
  const permissions = meQuery.data?.permissions ?? [];
  const currentUserId = meQuery.data?.user.id ?? '';
  const [decisionResult, setDecisionResult] = useState<{
    id: string;
    status: ApprovalStatus;
  } | null>(null);
  // 第 8 轮走查 medium：decide 响应里的 execution 只在这一次响应中出现
  //（console/batch 由服务端在批准时同步执行），必须留在本地展示，否则
  // 审批者只看到「已批准」，无从判断命令是否真的执行成功。
  const [decisionExecution, setDecisionExecution] = useState<{
    id: string;
    approval: Approval;
    execution: ApprovalExecution | null;
  } | null>(null);
  const status: ApprovalStatus =
    decisionResult?.id === approval.id ? decisionResult.status : approval.status;
  const [now, setNow] = useState(() => Date.now());
  // RBAC 走查 sprint 2：审批上下文抽屉（只读，需 approvals:read 才有入口；
  // 服务端仍是对应的唯一授权点，跨用户读取会写 approval.context_viewed）。
  const [contextOpen, setContextOpen] = useState(false);
  const canReadApproval = hasAnyPermission(permissions, [PERMS.approvalsRead]);
  // 本地倒计时只用于提示；是否仍可决策以服务端状态为准（status）。
  const expiresAt = new Date(approval.expires_at).getTime();
  const remaining = Number.isFinite(expiresAt) ? expiresAt - now : 0;
  const timedOut = status === 'pending' && remaining <= 0;
  const effectiveStatus: ApprovalStatus = timedOut ? 'expired' : status;
  const decisionState = approvalDecisionState({
    status: effectiveStatus,
    permissions,
    requestedBy: approval.requested_by,
    currentUserId,
  });
  const approveBlocked =
    decisionState === 'actions' &&
    approvalApproveBlockedByDegradedMode(effectiveStatus, singleAdminDegraded);
  useEffect(() => {
    if (status !== 'pending') {
      return;
    }
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [status]);

  const decision = useMutation({
    mutationFn: async (input: { value: 'approve' | 'deny' }) => {
      const { data, error } = await api.POST('/api/approvals/{id}/decide', {
        params: {
          path: { id: approval.id },
          header: { Origin: window.location.origin },
        },
        // 00012：decide 请求体只允许 decision/note；审批时不允许改写命令
        //（改写会让审批人成为动作作者，绕过 requested_by != decided_by）。
        body: { decision: input.value },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    onSuccess: (result) => {
      // 四期：decide 响应为 {approval, execution} 包装（source=console 且
      // 批准时携带同步执行结果；chat 审批 execution 恒 null）。
      setDecisionResult({ id: result.approval.id, status: result.approval.status });
      setDecisionExecution({
        id: result.approval.id,
        approval: result.approval,
        execution: result.execution ?? null,
      });
      onUpdated?.(result.approval, result.execution ?? null);
      void queryClient.invalidateQueries({ queryKey: qk.approvals(conversationId) });
    },
  });

  // 是否仍可决策以服务端状态为准；本地倒计时只用于提示。
  const disabled = status !== 'pending' || decision.isPending;
  const errorInfo = decision.error ? toApiErrorInfo(decision.error) : null;
  const ruleLabel = describeRiskRule(approval.rule_id);
  const cancelled = isCancelledApproval(approval);
  const bodyKind = approvalBodyKind(approval.source);
  // 历史改写（8.7）：已决策审批 edited_command 非空时双行展示原文（删除线）
  // 与改写文，均可复制；执行使用的是改写后命令（2.11 约定 3）。改写只存在
  // 于命令类审批，policy/terminal 的 command 恒空，不参与。
  const edited =
    bodyKind === 'command' && approval.edited_command !== '' && effectiveStatus !== 'pending';
  // 第 8 轮走查 medium：批准 console/batch 审批时服务端已同步执行，结果只
  // 存在于 decide 响应。execution 非 null 时展示 exit_code/error_code
  //（人话文案复用容器页 describeDecisionOutcome）。
  const executionOutcome =
    decisionExecution && decisionExecution.id === approval.id && decisionExecution.execution
      ? approvalOutcome(decisionExecution.approval, decisionExecution.execution)
      : null;

  const handleApprove = (): void => {
    decision.mutate({ value: 'approve' });
  };
  const handleDeny = (): void => {
    decision.mutate({ value: 'deny' });
  };

  return (
    <Card
      size="small"
      className={`chat-approval chat-approval--${effectiveStatus}`}
      title={
        <Space wrap size={8}>
          <span>{approvalCardTitle(approval)}</span>
          <RiskBadge level={approval.risk_level} />
          <StatusTag status={effectiveStatus} label={cancelled ? '已取消' : undefined} />
          {edited ? <Tag color="purple">用户改写</Tag> : null}
        </Space>
      }
    >
      <Space direction="vertical" size={10} style={{ width: '100%' }}>
        <Space wrap size={16}>
          {/* 第 8 轮走查 low：policy 审批（全局门限变更）没有目标服务器，
              server_name 按设计恒空；此前无条件渲染「目标服务器：未知」，
              审批者会以为目标缺失。policy 改为展示变更对象本身。 */}
          {bodyKind === 'policy' ? (
            <Typography.Text type="secondary">全局审批门限变更</Typography.Text>
          ) : (
            <Typography.Text type="secondary">
              目标服务器：
              <Typography.Text strong>{approval.server_name || '未知'}</Typography.Text>
            </Typography.Text>
          )}
          <Typography.Text type="secondary">
            <ClockCircleOutlined />{' '}
            {effectiveStatus === 'pending'
              ? `剩余 ${formatRemaining(remaining)}`
              : cancelled
                ? '本轮已取消'
                : APPROVAL_STATUS_LABELS[effectiveStatus]}
          </Typography.Text>
        </Space>

        <Space wrap size={8}>
          {/* 契约 6.3/2.11 约定 1：审批卡展示创建时门限快照（「在 <mode> 门限
              下请求审批」）；门限变更不迁移已建审批（approvals.approval_mode
              为创建时快照，schema 必填）。 */}
          <Typography.Text type="secondary">
            在 <ApprovalModeTag mode={approval.approval_mode} /> 门限下请求审批
          </Typography.Text>
        </Space>

        {bodyKind === 'command' ? (
          edited ? (
            // 历史改写双行（8.7）：原命令删除线 + 改写文，两者均可复制。
            <div className="chat-approval-command-diff">
              <div>
                <Space size={6}>
                  <Typography.Text type="secondary" delete>
                    原命令
                  </Typography.Text>
                  <CopyButton text={approval.command} label="复制原命令" />
                </Space>
                <Typography.Paragraph
                  type="secondary"
                  delete
                  className="chat-approval-command-text"
                >
                  {approval.command}
                </Typography.Paragraph>
              </div>
              <div>
                <Space size={6} wrap>
                  <Typography.Text type="secondary">实际执行（用户改写）</Typography.Text>
                  {approval.edited_risk_level ? (
                    <RiskBadge level={approval.edited_risk_level} />
                  ) : null}
                  <CopyButton text={approval.edited_command} label="复制改写后命令" />
                </Space>
                <CodeBlock code={approval.edited_command} language="bash" closed />
              </div>
            </div>
          ) : (
            <div>
              <Space size={6}>
                <Typography.Text type="secondary">
                  {effectiveStatus === 'pending' ? '待执行命令' : '命令'}
                </Typography.Text>
                <CopyButton text={approval.command} label="复制命令" />
              </Space>
              <CodeBlock code={approval.command} language="bash" closed />
            </div>
          )
        ) : null}

        {bodyKind === 'policy' ? (
          // policy 审批没有命令：只呈现策略目标与「本审批不含命令」，
          // 不再渲染空的「待执行命令」代码块（第 2 轮走查 medium）。
          <Space direction="vertical" size={4}>
            <Typography.Text>
              策略：
              <ApprovalModeTag mode={normalizeApprovalMode(approval.policy_from)} /> →{' '}
              <ApprovalModeTag mode={normalizeApprovalMode(approval.policy_to)} />
            </Typography.Text>
            <Typography.Text type="secondary">
              本审批只变更全局审批门限，不含可执行命令。
            </Typography.Text>
          </Space>
        ) : null}
        {bodyKind === 'terminal' ? (
          // terminal 审批同样没有命令：只呈现目标服务器与单次授权时长。
          <Space direction="vertical" size={4}>
            <Typography.Text>
              目标：{approval.server_name || '未知服务器'}（单次授权，30 分钟内有效）
            </Typography.Text>
            <Typography.Text type="secondary">
              本审批只授予一次交互会话，不含可执行命令；批准后由发起人打开
              {approval.grant_kind === 'rdp' ? '桌面' : '终端'}消费。
            </Typography.Text>
          </Space>
        ) : null}

        <div>
          <Typography.Text type="secondary">风险原因：</Typography.Text>
          {ruleLabel ? (
            <>
              <Typography.Text>{ruleLabel}</Typography.Text>
              <Collapse
                ghost
                size="small"
                className="chat-approval-rule-detail"
                items={[
                  {
                    key: 'rule',
                    label: `规则详情（${approval.rule_id}）`,
                    children: <Typography.Text type="secondary">{approval.reason}</Typography.Text>,
                  },
                ]}
              />
            </>
          ) : (
            <Typography.Text>{approval.reason}</Typography.Text>
          )}
        </div>

        {approval.note ? (
          <Tag color="default">备注：{noteLabel[approval.note] ?? approval.note}</Tag>
        ) : null}

        {errorInfo ? (
          <Alert
            type="error"
            showIcon
            message={describeError(errorInfo.code, errorInfo.message)}
            description={errorInfo.requestId ? `request_id: ${errorInfo.requestId}` : null}
          />
        ) : null}

        {effectiveStatus !== 'pending' ? (
          <Alert
            type={effectiveStatus === 'approved' ? 'success' : 'warning'}
            showIcon
            message={
              cancelled
                ? '本轮已取消（回答流已中断，审批未生效）'
                : `审批${APPROVAL_STATUS_LABELS[effectiveStatus]}`
            }
            description={
              edited ? '执行使用的是用户改写后的命令；原命令未执行。' : '最终状态以服务端为准。'
            }
          />
        ) : null}

        {executionOutcome ? (
          <Alert
            data-testid="approval-execution-result"
            type={
              executionOutcome.kind === 'failed'
                ? 'error'
                : executionOutcome.kind === 'denied'
                  ? 'warning'
                  : 'success'
            }
            showIcon
            message={`即时执行结果：${executionOutcome.message}`}
            description={executionOutcome.detail ? `执行明细：${executionOutcome.detail}` : null}
          />
        ) : null}

        {/* 审批上下文入口（RBAC 走查 sprint 2）：只读有界上下文，含审批前
            最多 20 条消息与工具参数；入口按 approvals:read 收敛。 */}
        {canReadApproval ? (
          <Space wrap>
            <Button icon={<FileSearchOutlined />} onClick={() => setContextOpen(true)}>
              查看上下文
            </Button>
          </Space>
        ) : null}

        {/* 走查第 3 轮 medium：单管理员/无第二审批者时，服务端对 approve 一律
            409。在点按钮之前给出中文说明（谁该做什么），并把「批准」置灰；
            「拒绝」保持可用（不扩大执行面，服务端也不拦）。 */}
        {approveBlocked ? (
          <Alert
            type="warning"
            showIcon
            message="当前无法批准：系统缺少第二个可审批的启用账号"
            description="审批需由另一位启用账号批准，但当前只有 1 个可承担审批职责的启用账号（管理员 + 审批者合计）。请管理员在「用户」页创建一个启用审批者；紧急情况下可由主机侧执行 aiops break-glass 单次批准。降级期间仍可拒绝该请求。"
          />
        ) : null}

        {/* 待决策操作行：只允许按原命令批准/拒绝（00012 禁止审批时改写）。
            入口按 approvals:decide 与职责分离收敛（见 approvalDecisionState）。 */}
        {decisionState === 'actions' ? (
          <Space wrap>
            <Button
              type="primary"
              icon={<CheckOutlined />}
              disabled={approveBlocked}
              loading={decision.isPending && decision.variables?.value === 'approve'}
              onClick={handleApprove}
            >
              批准
            </Button>
            <Button
              danger
              icon={<CloseOutlined />}
              disabled={disabled}
              loading={decision.isPending && decision.variables?.value === 'deny'}
              onClick={handleDeny}
            >
              拒绝
            </Button>
          </Space>
        ) : null}

        {decisionState === 'waiting' ? (
          <Alert
            type="info"
            showIcon
            message="等待审批者处理"
            description="当前账号没有审批权限；审批将由具备审批权限的账号处理。"
          />
        ) : null}

        {decisionState === 'self' ? (
          <Alert
            type="info"
            showIcon
            message="不能审批自己发起的请求"
            description="审批需由另一位具备审批权限的账号处理（服务端会拒绝自审）。"
          />
        ) : null}
      </Space>

      <ApprovalContextDrawer
        approvalId={approval.id}
        open={contextOpen}
        onClose={() => setContextOpen(false)}
      />
    </Card>
  );
}
