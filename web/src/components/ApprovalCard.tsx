import { CheckOutlined, ClockCircleOutlined, CloseOutlined, EditOutlined } from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Collapse, Input, Space, Tag, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { api } from '../api/client';
import { qk } from '../api/queryKeys';
import type { components } from '../api/schema';
import { describeError, toApiErrorInfo } from '../features/chat/useChatStream';
import { ApprovalModeTag } from './ApprovalModeSelect';
import { APPROVAL_STATUS_LABELS } from './approvalStatus';
import {
  buildDecideBody,
  canSubmitEditedCommand,
  describeRiskChange,
  EDITED_COMMAND_MAX_LENGTH,
  hasEditedChange,
  validateEditedCommand,
} from './approvalEdit';
import CodeBlock from './CodeBlock';
import CopyButton from './CopyButton';
import { describeRiskRule } from './riskRules';
import RiskBadge from './RiskBadge';
import StatusTag from './StatusTag';

type Approval = components['schemas']['Approval'];
type ApprovalStatus = components['schemas']['ApprovalStatus'];

export interface ApprovalCardProps {
  approval: Approval;
  conversationId: string;
  /** HTTP 决策成功后的即时反馈；最终状态仍以 SSE approval_resolved 为准。 */
  onUpdated?: (approval: Approval) => void;
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

export default function ApprovalCard({ approval, conversationId, onUpdated }: ApprovalCardProps) {
  const queryClient = useQueryClient();
  const [decisionResult, setDecisionResult] = useState<{
    id: string;
    status: ApprovalStatus;
  } | null>(null);
  const status: ApprovalStatus =
    decisionResult?.id === approval.id ? decisionResult.status : approval.status;
  const [now, setNow] = useState(() => Date.now());
  // 本地倒计时只用于提示；是否仍可决策以服务端状态为准（status）。
  const expiresAt = new Date(approval.expires_at).getTime();
  const remaining = Number.isFinite(expiresAt) ? expiresAt - now : 0;
  const timedOut = status === 'pending' && remaining <= 0;
  const effectiveStatus: ApprovalStatus = timedOut ? 'expired' : status;
  // 六期编辑态：draft 预填原命令；null=未进入编辑。draft 是本地草稿，
  // 不随 approval prop 刷新重置——流式期间审批卡反复重渲染（SSE 刷新），
  // 用户正在输入的改写必须保留（任务要求：流式期间审批卡行为不回退）。
  const [draft, setDraft] = useState<string | null>(null);
  const editing = draft !== null && effectiveStatus === 'pending';

  useEffect(() => {
    if (status !== 'pending') {
      return;
    }
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [status]);

  // 服务端 decide 时按审批 server_id 取服务器重新分类（契约 2.11 约定 3）；
  // server_id 为空的审批不允许改写（400）。前端同口径：无 server_id 不给
  // 编辑入口。server_id 同时作为 preview-risk 的上下文（服务端按服务器
  // 实际 os/netdev_brand 覆盖显式分册参数，与 decide 重分类一致）。
  const editable = effectiveStatus === 'pending' && approval.server_id !== null;

  const decision = useMutation({
    mutationFn: async (input: { value: 'approve' | 'deny'; editedCommand: string | null }) => {
      const { data, error } = await api.POST('/api/approvals/{id}/decide', {
        params: {
          path: { id: approval.id },
          header: { Origin: window.location.origin },
        },
        body: buildDecideBody(input.value, approval.command, input.editedCommand),
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
      onUpdated?.(result.approval);
      void queryClient.invalidateQueries({ queryKey: qk.approvals(conversationId) });
    },
  });

  // 改写命令的实时风险预览（8.7 审批卡编辑）：仅在编辑态且确有改写时查询；
  // 相同命令 15s staleTime 去重（同批量页 preview-risk 口径）。预览只是提前
  // 提示——最终裁决在服务端 decide 重分类，预览失败不拦截提交。
  const draftCommand = draft ?? '';
  const draftChanged = editing && hasEditedChange(approval.command, draftCommand);
  const previewQuery = useQuery({
    queryKey: qk.approvalRiskPreview(draftCommand),
    queryFn: async () => {
      const { data, error } = await api.POST('/api/ops/preview-risk', {
        params: { header: { Origin: window.location.origin } },
        body: {
          command: draftCommand,
          os: 'linux',
          netdev_brand: 'generic',
          ...(approval.server_id ? { server_id: approval.server_id } : {}),
        },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    enabled: editing && draftCommand.trim() !== '' && draftChanged,
    staleTime: 15_000,
    retry: false,
  });

  const preview = previewQuery.data ?? null;
  const previewLevel = preview?.risk_level ?? null;
  const riskChange =
    draftChanged && previewLevel ? describeRiskChange(approval.risk_level, previewLevel) : null;
  const draftError = editing ? validateEditedCommand(draftCommand) : null;
  const previewForbidden = previewLevel === 'forbidden';

  // 是否仍可决策以服务端状态为准；本地倒计时只用于提示。
  const disabled = status !== 'pending' || decision.isPending;
  const errorInfo = decision.error ? toApiErrorInfo(decision.error) : null;
  const ruleLabel = describeRiskRule(approval.rule_id);
  const cancelled = isCancelledApproval(approval);
  // 历史改写（8.7）：已决策审批 edited_command 非空时双行展示原文（删除线）
  // 与改写文，均可复制；执行使用的是改写后命令（2.11 约定 3）。
  const edited = approval.edited_command !== '' && effectiveStatus !== 'pending';

  const handleApprove = (): void => {
    // 六期打磨 sprint 2（走查 high）：编辑态空草稿绝不提交——按钮已禁用，
    // 这里再兜底一层（键盘回车/程序化触发同样不得按原命令批准执行）。
    if (editing && draftCommand.trim() === '') {
      return;
    }
    decision.mutate({ value: 'approve', editedCommand: editing ? draftCommand : null });
  };
  const handleDeny = (): void => {
    // 拒绝流程不变（契约 8.7）：deny 不携带改写，按原命令拒绝。
    decision.mutate({ value: 'deny', editedCommand: null });
  };

  return (
    <Card
      size="small"
      className={`chat-approval chat-approval--${effectiveStatus}`}
      title={
        <Space wrap size={8}>
          <span>命令审批</span>
          <RiskBadge level={approval.risk_level} />
          <StatusTag status={effectiveStatus} label={cancelled ? '已取消' : undefined} />
          {edited ? <Tag color="purple">用户改写</Tag> : null}
        </Space>
      }
    >
      <Space direction="vertical" size={10} style={{ width: '100%' }}>
        <Space wrap size={16}>
          <Typography.Text type="secondary">
            目标服务器：<Typography.Text strong>{approval.server_name || '未知'}</Typography.Text>
          </Typography.Text>
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

        {edited ? (
          // 历史改写双行（8.7）：原命令删除线 + 改写文，两者均可复制。
          <div className="chat-approval-command-diff">
            <div>
              <Space size={6}>
                <Typography.Text type="secondary" delete>
                  原命令
                </Typography.Text>
                <CopyButton text={approval.command} label="复制原命令" />
              </Space>
              <Typography.Paragraph type="secondary" delete className="chat-approval-command-text">
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
        )}

        {!editing ? (
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
                      children: (
                        <Typography.Text type="secondary">{approval.reason}</Typography.Text>
                      ),
                    },
                  ]}
                />
              </>
            ) : (
              <Typography.Text>{approval.reason}</Typography.Text>
            )}
          </div>
        ) : null}

        {/* 编辑态（8.7 审批卡编辑）：文本域预填原命令，实时 preview-risk
            预览新级别/RuleID/理由；仅「保存并批准」携带 edited_command。 */}
        {editing ? (
          <div className="chat-approval-edit">
            <Typography.Text type="secondary">
              改写命令（保存并批准时提交，服务端将重新分类）
            </Typography.Text>
            <Input.TextArea
              value={draftCommand}
              onChange={(event) => setDraft(event.target.value)}
              rows={3}
              maxLength={EDITED_COMMAND_MAX_LENGTH}
              showCount
              autoSize={{ minRows: 2, maxRows: 6 }}
              aria-label="改写命令"
            />
            {draftError ? (
              <Alert type="warning" showIcon message={draftError} />
            ) : draftChanged && previewQuery.isFetching ? (
              <Typography.Text type="secondary">正在预览改写后的风险…</Typography.Text>
            ) : draftChanged && preview ? (
              <Space size={8} wrap>
                <Typography.Text type="secondary">改写后风险：</Typography.Text>
                <RiskBadge level={preview.risk_level} />
                {riskChange ? (
                  <Tag
                    color={
                      riskChange.kind === 'forbidden'
                        ? 'red'
                        : riskChange.kind === 'up'
                          ? 'volcano'
                          : riskChange.kind === 'down'
                            ? 'green'
                            : 'default'
                    }
                  >
                    {riskChange.label}
                  </Tag>
                ) : null}
                <Typography.Text type="secondary">
                  {describeRiskRule(preview.rule_id) ?? preview.reason}
                </Typography.Text>
              </Space>
            ) : draftChanged && previewQuery.isError ? (
              <Typography.Text type="secondary">
                风险预览暂不可用，提交时服务端仍会重新分类。
              </Typography.Text>
            ) : null}
            {previewForbidden ? (
              <Alert
                type="error"
                showIcon
                message={`改写后的命令被风险规则禁止（${preview?.rule_id ?? '未知规则'}），无法批准`}
              />
            ) : null}
            {/* decide 返回 403 COMMAND_FORBIDDEN（预览与 decide 竞态：预览通过
                但服务端按最新规则判禁）——契约 8.7：编辑态内联展示，审批保持
                待决策，草稿不丢，用户可继续修改。 */}
            {editing && errorInfo?.code === 'COMMAND_FORBIDDEN' ? (
              <Alert
                type="error"
                showIcon
                message="改写后的命令被风险规则禁止，请修改命令后重试；审批仍待处理。"
                description={errorInfo.requestId ? `request_id: ${errorInfo.requestId}` : null}
              />
            ) : null}
            <Space wrap>
              <Button
                type="primary"
                icon={<CheckOutlined />}
                disabled={disabled || !canSubmitEditedCommand(draftCommand, previewLevel)}
                loading={decision.isPending && decision.variables?.value === 'approve'}
                onClick={handleApprove}
              >
                保存并批准
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
              <Button disabled={decision.isPending} onClick={() => setDraft(null)}>
                取消编辑
              </Button>
            </Space>
          </div>
        ) : null}

        {approval.note ? (
          <Tag color="default">备注：{noteLabel[approval.note] ?? approval.note}</Tag>
        ) : null}

        {errorInfo && !(editing && errorInfo.code === 'COMMAND_FORBIDDEN') ? (
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

        {/* 非编辑的待决策操作行：编辑入口 + 直接批准/拒绝。 */}
        {effectiveStatus === 'pending' && !editing ? (
          <Space wrap>
            <Button
              type="primary"
              icon={<CheckOutlined />}
              disabled={disabled}
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
            {editable ? (
              <Button
                icon={<EditOutlined />}
                disabled={disabled}
                onClick={() => setDraft(approval.command)}
              >
                编辑命令
              </Button>
            ) : null}
          </Space>
        ) : null}

        {/* pending 但不可编辑（server_id 为空）时提示口径：不是 bug，
            契约规定 server_id 为空的审批不允许改写（2.11 约定 3）。 */}
        {effectiveStatus === 'pending' && !editing && !editable ? (
          <Typography.Text type="secondary">
            该审批未绑定目标服务器，不支持编辑命令。
          </Typography.Text>
        ) : null}
      </Space>
    </Card>
  );
}
