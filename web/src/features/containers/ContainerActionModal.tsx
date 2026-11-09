// C2 四期：容器操作审批弹窗（契约 8.7 容器页 / 2.9 约定 3）。
// 三态：confirm（说明并创建审批）→ pending（审批卡视觉：命令、服务器名、
// 风险级别、原因、倒计时 + 批准/拒绝）→ result（decide 响应回填：
// execution 非 null 展示 exit_code/duration，失败显示 error_code）。
// REST 不直接执行——POST action 创建 source=console 审批，decide 批准时
// 服务端同步逐字节执行固化命令。

import { useMutation } from '@tanstack/react-query';
import { EditOutlined } from '@ant-design/icons';
import { Alert, Button, Modal, Space, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import {
  ApiRequestError,
  failureFromError,
  toApiFailure,
  writeHeaders,
  type ApiFailure,
} from '../../components/admin/apiError';
import ApprovalCommandEditor from '../../components/ApprovalCommandEditor';
import CodeBlock from '../../components/CodeBlock';
import RiskBadge from '../../components/RiskBadge';
import { describeRiskRule } from '../../components/riskRules';
import { formatRemaining } from '../../components/ApprovalCard';
import { APPROVAL_STATUS_LABELS } from '../../components/approvalStatus';
import { useGlobalApprovalMode } from '../settings/useApprovalSettings';
import { containerActionNeedsApproval } from './containerActionPolicy';
import {
  commandSectionLabel,
  describeContainerFailure,
  describeDecisionOutcome,
  previewContainerCommand,
} from './containerFailures';

type Approval = components['schemas']['Approval'];
type ApprovalDecisionResult = components['schemas']['ApprovalDecisionResult'];
type ContainerAction = components['schemas']['ContainerActionRequest']['action'];

export interface ContainerActionTarget {
  serverId: string;
  serverName: string;
  runtime: string;
  containerId: string;
  containerName: string;
  action: ContainerAction;
  /**
   * 服务器级「自动执行」开关（servers.auto_exec_enabled）。confirm 态据此
   * 预测提交后是直执行还是建审批（见 containerActionPolicy）——仅文案预告，
   * 真正分流由后端 action 响应决定。
   */
  autoExecEnabled: boolean;
}

interface ContainerActionModalProps {
  target: ContainerActionTarget | null;
  onClose: () => void;
  /** decide 完成后的失效刷新（契约 8.4 四期：containers/container/approvals）。 */
  onDecided: (target: ContainerActionTarget) => void;
}

const ACTION_LABELS: Record<ContainerAction, string> = {
  start: '启动',
  stop: '停止',
  restart: '重启',
};

type Phase =
  | { stage: 'confirm' }
  | { stage: 'pending'; approval: Approval }
  // 六期（契约 2.11 约定 2）：action 响应改 wrapper——直执行分支无审批对象
  //（approval=null、execution 非空，全局门限 none/high 时出现）；完整分流
  // UI 由六期 FE-P 落地，此处最小适配直执行进入 result 态。
  | { stage: 'result'; approval: Approval | null; execution: ApprovalDecisionResult['execution'] };

export default function ContainerActionModal({
  target,
  onClose,
  onDecided,
}: ContainerActionModalProps) {
  const [phase, setPhase] = useState<Phase>({ stage: 'confirm' });
  const [actionError, setActionError] = useState<ApiFailure | null>(null);
  const [now, setNow] = useState(() => Date.now());
  // 六期打磨 sprint 3：编辑命令态（此前该入口只能批准/拒绝，六期改写能力
  // 只在对话审批卡可用）。
  const [editing, setEditing] = useState(false);
  // 六期打磨 sprint 6（走查 medium）：confirm 态文案必须与实际分流一致。
  // 全局门限与服务器 auto_exec 决定「提交后是否真的建审批」——生效门限由
  // 后端解析（本页无会话覆盖，容器 action 用全局门限，契约 2.11 约定 2
  // 第 ④ 条）。门限查询失败/未加载时不承诺任一分支（中性文案）。
  const globalModeQuery = useGlobalApprovalMode();

  // 目标变化（打开新操作）经调用方 key 重挂载重置回 confirm 态，不在此
  // 用 effect setState（react-hooks/set-state-in-effect）。

  // pending 态倒计时（同 ApprovalCard 的 1s 心跳；最终状态以服务端为准）。
  useEffect(() => {
    if (phase.stage !== 'pending') {
      return;
    }
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [phase.stage]);

  const createMutation = useMutation({
    mutationFn: async (input: ContainerActionTarget) => {
      const { data, error, response } = await api.POST(
        '/api/servers/{id}/containers/{cid}/action',
        {
          params: {
            path: { id: input.serverId, cid: input.containerId },
            header: writeHeaders(),
          },
          body: { action: input.action },
        },
      );
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data) => {
      setActionError(null);
      // 六期：approval=null 表示门限判定直执行（契约 2.11 约定 2）——
      // 无审批等待，直接进入 result 态展示同步执行结果。
      setPhase(
        data.approval !== null
          ? { stage: 'pending', approval: data.approval }
          : { stage: 'result', approval: null, execution: data.execution },
      );
    },
    onError: (error) => {
      setActionError(failureFromError(error));
    },
  });

  const decideMutation = useMutation({
    mutationFn: async (input: {
      approvalId: string;
      decision: 'approve' | 'deny';
      /** 六期打磨 sprint 3：编辑态提交的改写后命令（仅 approve 合法）。 */
      editedCommand?: string;
    }) => {
      const { data, error, response } = await api.POST('/api/approvals/{id}/decide', {
        params: { path: { id: input.approvalId }, header: writeHeaders() },
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
    onSuccess: (data) => {
      setEditing(false);
      setPhase({ stage: 'result', approval: data.approval, execution: data.execution });
      if (target) {
        onDecided(target);
      }
    },
    onError: (error) => {
      setActionError(failureFromError(error));
    },
  });

  if (!target) {
    return null;
  }

  const actionLabel = ACTION_LABELS[target.action];
  const containerLabel =
    target.containerName !== '' ? target.containerName : target.containerId.slice(0, 12);
  const title = `${actionLabel}容器 · ${containerLabel}`;
  // pending 与 result 两阶段都有 approval 对象在手：真命令以
  // approval.command 为准（逐字节，与 action/decide 契约一致）；仅 confirm
  // 阶段（审批未创建）用本地合成预览（评审发现 result 阶段曾回退合成串）。
  // 六期直执行分支（result 且 approval=null）无审批对象，同样回退合成
  // 预览——命令即 action/容器 ID 决定，与实际下发一致。
  const preview =
    phase.stage === 'confirm'
      ? previewContainerCommand(target.runtime, target.containerId, target.action)
      : phase.approval !== null
        ? phase.approval.command
        : previewContainerCommand(target.runtime, target.containerId, target.action);
  const outcome =
    phase.stage === 'result' ? describeDecisionOutcome(phase.approval, phase.execution) : null;
  const pendingApproval = phase.stage === 'pending' ? phase.approval : null;
  const remaining = pendingApproval ? new Date(pendingApproval.expires_at).getTime() - now : 0;
  // 与复用的 ApprovalCard 超时语义一致（评审发现）：pending 且倒计时归零
  // 即锁定决策按钮——过期审批点击只会得到 409，UI 不得先给可点假象。
  const timedOut =
    pendingApproval !== null && pendingApproval.status === 'pending' && remaining <= 0;
  // confirm 态分流预测（六期打磨 sprint 6）：null=门限未加载（中性文案）。
  const confirmNeedsApproval = containerActionNeedsApproval(
    globalModeQuery.data?.approval_mode,
    target.autoExecEnabled,
    target.action,
  );
  const confirmText =
    confirmNeedsApproval === true
      ? '操作不直接执行——将创建一条需人工批准的审批，批准后服务端按运行时固化命令同步执行并记录审计。'
      : confirmNeedsApproval === false
        ? '当前审批门限判定为免审批——点击后将立即执行该命令（不经人工批准），并记录审计。'
        : '提交后由服务端按当前审批门限判定：或创建需人工批准的审批，或直接执行；两者都会记录审计。';
  const confirmButtonLabel = confirmNeedsApproval === false ? '确认执行' : '创建审批';

  return (
    <Modal
      open
      title={title}
      onCancel={onClose}
      // 契约 8.7：批准按钮默认焦点不在危险动作上——footer 自渲染且无
      // 默认 ok/cancel 按钮，Modal 不自动聚焦任何操作按钮。
      footer={null}
    >
      <Space direction="vertical" size={12} style={{ width: '100%', marginTop: 8 }}>
        {phase.stage === 'confirm' ? (
          <Typography.Text>
            {/* p4 sprint 4：正文与标题（上方 containerLabel）同口径——无名称
                容器（name=''）回退 12 位短 ID，不再留空白（walk9）。
                六期打磨 sprint 6（走查 medium）：分流文案按门限预测渲染——
                此前恒说「不直接执行…需人工批准的审批」，而 safe+auto_exec
                （standard）或 none/high 档实际直接执行、不产生审批（破坏性
                命令已真实执行，用户以为在排队审批）。 */}
            即将对容器 <Typography.Text strong>{containerLabel}</Typography.Text> （服务器{' '}
            {target.serverName}）执行「{actionLabel}」。{confirmText}
          </Typography.Text>
        ) : null}

        {pendingApproval ? (
          <Space wrap size={16}>
            <Typography.Text type="secondary">
              目标服务器：
              <Typography.Text strong>
                {pendingApproval.server_name || target.serverName}
              </Typography.Text>
            </Typography.Text>
            <RiskBadge level={pendingApproval.risk_level} />
            <Typography.Text type={timedOut ? 'danger' : 'secondary'}>
              {/* 六期打磨 sprint 6（走查 low）：TTL 到期措辞与全站同源——
                  此前这里写「已超时」，而 StatusTag / ApprovalCard 正文 /
                  回放统一是「已过期」（components/approvalStatus.ts）。 */}
              {timedOut ? APPROVAL_STATUS_LABELS.expired : `剩余 ${formatRemaining(remaining)}`}
            </Typography.Text>
          </Space>
        ) : null}

        <div>
          <Typography.Text type="secondary">
            {/* p4 sprint 5：命令区标签按阶段取文案（walk5）——confirm 预告
                将申请、pending 待执行、result 已决（denied 细分未执行，与
                同屏 outcome 文案同口径）。六期打磨 sprint 6：confirm 再按
                门限预测细分「将直接执行」（免审批档），不再一律称申请。 */}
            {commandSectionLabel(
              phase.stage,
              phase.stage === 'confirm' ? null : phase.approval,
              phase.stage === 'confirm' ? confirmNeedsApproval : null,
            )}
          </Typography.Text>
          <CodeBlock code={preview} language="bash" closed />
        </div>

        {pendingApproval ? (
          <Typography.Text type="secondary">
            风险原因：
            {describeRiskRule(pendingApproval.rule_id) ?? pendingApproval.reason}
            （规则 {pendingApproval.rule_id}）
          </Typography.Text>
        ) : null}

        {outcome ? (
          <Alert
            type={
              outcome.kind === 'success' ? 'success' : outcome.kind === 'failed' ? 'error' : 'info'
            }
            showIcon
            message={outcome.message}
            description="最终状态以服务端为准。"
          />
        ) : null}

        {actionError ? (
          <Alert
            type="error"
            showIcon
            message={describeContainerFailure(actionError, target.runtime)}
            description={actionError.requestId ? `request_id: ${actionError.requestId}` : null}
          />
        ) : null}

        {phase.stage === 'confirm' ? (
          <Space>
            <Button onClick={onClose}>取消</Button>
            {/* 六期打磨 sprint 6：按钮文案跟随预测分流——免审批档「确认执行」，
                不再一律「创建审批」（点击后并没有审批产生）。 */}
            <Button
              type="primary"
              danger={confirmNeedsApproval === false}
              loading={createMutation.isPending}
              onClick={() => createMutation.mutate(target)}
            >
              {confirmButtonLabel}
            </Button>
          </Space>
        ) : null}

        {phase.stage === 'pending' && editing && pendingApproval ? (
          // 六期打磨 sprint 3：编辑命令（复用 ApprovalCommandEditor 的
          // 校验/预览/请求体构造，与对话审批卡同一口径）。
          <ApprovalCommandEditor
            command={pendingApproval.command}
            riskLevel={pendingApproval.risk_level}
            serverId={pendingApproval.server_id}
            pending={decideMutation.isPending}
            disabled={timedOut}
            onSubmit={(body) => {
              decideMutation.mutate({
                approvalId: pendingApproval.id,
                decision: 'approve',
                ...(body.edited_command !== undefined
                  ? { editedCommand: body.edited_command }
                  : {}),
              });
            }}
          />
        ) : null}

        {phase.stage === 'pending' && !editing ? (
          <Space>
            {/* p4 sprint 2：原「稍后处理」承诺了无法兑现的操作——关闭后全产品
                无 pending console 审批的再决策入口（无审批列表页），只能静默
                过期；改回中性「关闭」，并用旁注明示后果。 */}
            <Button onClick={onClose}>关闭</Button>
            <Button
              danger
              loading={decideMutation.isPending && decideMutation.variables?.decision === 'deny'}
              disabled={decideMutation.isPending || timedOut}
              onClick={() => {
                if (pendingApproval) {
                  decideMutation.mutate({ approvalId: pendingApproval.id, decision: 'deny' });
                }
              }}
            >
              拒绝
            </Button>
            <Button
              type="primary"
              loading={decideMutation.isPending && decideMutation.variables?.decision === 'approve'}
              disabled={decideMutation.isPending || timedOut}
              onClick={() => {
                if (pendingApproval) {
                  decideMutation.mutate({ approvalId: pendingApproval.id, decision: 'approve' });
                }
              }}
            >
              批准
            </Button>
            {/* 六期打磨 sprint 3：编辑入口（此前三个 console/batch 审批入口
                都没有，六期改写能力只能在对话审批卡使用）。 */}
            <Button
              icon={<EditOutlined />}
              disabled={decideMutation.isPending || timedOut || pendingApproval === null}
              onClick={() => setEditing(true)}
            >
              编辑命令
            </Button>
          </Space>
        ) : null}

        {phase.stage === 'pending' && editing ? (
          <Button onClick={() => setEditing(false)} disabled={decideMutation.isPending}>
            取消编辑
          </Button>
        ) : null}

        {phase.stage === 'pending' ? (
          <Typography.Text type="secondary">
            关闭后审批将在倒计时结束时自动过期；如仍需操作，请重新发起并创建新审批。
          </Typography.Text>
        ) : null}

        {phase.stage === 'result' ? (
          <Button type="primary" onClick={onClose}>
            关闭
          </Button>
        ) : null}
      </Space>
    </Modal>
  );
}
