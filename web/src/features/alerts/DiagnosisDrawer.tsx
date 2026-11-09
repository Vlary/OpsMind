// M4 五期：AI 诊断会话抽屉（/alerts 页诊断入口，CONTRACT.md 8.7 告警页
// 「诊断报告入口」+ 2.10 告警→诊断→报告→修复走审批）。
// - 会话非终态（pending/diagnosing）3s 轮询至终态（8.4）。
// - 报告：summary/severity/root_cause/evidence 折叠/suggestions 列表
//   （RiskBadge + 「发起修复审批」——fix 端点要求逐字节等于建议之一，
//   创建 source=console 审批后在本抽屉内批准/拒绝 decide）。
// - awaiting_fix 状态提供「标记完成」（complete）。

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { EditOutlined } from '@ant-design/icons';
import { Alert, Button, Collapse, Descriptions, Drawer, Space, Spin, Tag, Typography } from 'antd';
import { useState } from 'react';

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
import { message } from '../../components/antdMessage';
import ApprovalCommandEditor from '../../components/ApprovalCommandEditor';
import RiskBadge from '../../components/RiskBadge';
import CodeBlock from '../../components/CodeBlock';

import { describePhase5Failure, describeResultCode } from './phase5Codes';
import {
  DIAGNOSIS_STATUS_COLOR,
  DIAGNOSIS_STATUS_LABELS,
  isDiagnosisActive,
} from './diagnosisStatus';

type DiagnosisSession = components['schemas']['DiagnosisSession'];
type Approval = components['schemas']['Approval'];
type FixSuggestion = components['schemas']['FixSuggestion'];
type DiagnosisEvidence = components['schemas']['DiagnosisEvidence'];

export interface DiagnosisDrawerProps {
  open: boolean;
  sessionId: string | null;
  onClose: () => void;
}

export default function DiagnosisDrawer({ open, sessionId, onClose }: DiagnosisDrawerProps) {
  const queryClient = useQueryClient();
  // fix 创建的审批（source=console），在本抽屉内 decide。
  const [pendingApproval, setPendingApproval] = useState<Approval | null>(null);

  const sessionQuery = useQuery({
    queryKey: qk.diagnosisSession(sessionId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/diagnosis/sessions/{id}', {
        params: { path: { id: sessionId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: open && sessionId !== null,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status && isDiagnosisActive(status) ? 3_000 : false;
    },
  });

  // 打开时恢复该服务器挂起的 console 审批（五期评审发现：pendingApproval
  // 只存内存态，关闭抽屉/刷新后 decide 入口丢失，120s TTL 后静默过期——
  // 从审批列表找回 pending 的 console 审批按 server_id 匹配恢复）。
  const session = sessionQuery.data;
  const pendingApprovalsQuery = useQuery({
    queryKey: qk.pendingApprovals({ status: 'pending', limit: 50 }),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/approvals', {
        params: { query: { status: 'pending', limit: 50 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data?.items ?? [];
    },
    enabled: open && sessionQuery.data?.status === 'awaiting_fix',
    refetchInterval: 10_000,
  });
  // 恢复逻辑改为渲染期派生：内存态优先；否则从审批列表找回该服务器
  // pending 的 console 审批（关闭抽屉/刷新后 decide 入口不丢失）。
  const serverID = session?.server_id ?? null;
  const effectiveApproval: Approval | null =
    pendingApproval ??
    (serverID === null
      ? null
      : (pendingApprovalsQuery.data?.find(
          (approval) => approval.source === 'console' && approval.server_id === serverID,
        ) ?? null));

  function invalidate(): void {
    if (sessionId) {
      void queryClient.invalidateQueries({ queryKey: qk.diagnosisSession(sessionId) });
    }
    void queryClient.invalidateQueries({ queryKey: qk.diagnosisSessions({}) });
  }

  const fixMutation = useMutation({
    mutationFn: async (command: string) => {
      const { data, error, response } = await api.POST('/api/diagnosis/sessions/{id}/fix', {
        params: { header: writeHeaders(), path: { id: sessionId ?? '' } },
        body: { command },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data) => {
      // 双保险剥包（五期 sprint 3）：契约 200 直返 Approval，但旧后端/
      // 网关缓存可能仍包 {approval, diagnosis_id} wrapper——历史上前端
      // 把 wrapper 整体当 Approval 存入，.id/.command 全 undefined，首点
      // decide 打到字面模板 URL 500 且命令区空白。
      const approval = ((data as { approval?: Approval } | undefined)?.approval ?? data) as
        | Approval
        | undefined;
      message.success('已创建修复审批，请批准或拒绝');
      if (approval) {
        setPendingApproval(approval);
      }
      invalidate();
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
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
    onSuccess: (result) => {
      const execution = result?.execution ?? null;
      if (execution) {
        const exitText =
          execution.exit_code === 0
            ? '执行成功'
            : `退出码 ${execution.exit_code}${execution.error_code ? `（${describeResultCode(execution.error_code) || execution.error_code}）` : ''}`;
        // 五期 sprint 3：退出码非 0 = 命令执行失败（如 SSH 不可达退出码
        // 255），此前一律 message.success 绿色样式，语义误导用户以为成功。
        if (execution.exit_code === 0) {
          message.success(`修复命令已执行：${exitText}`);
        } else {
          message.warning(`修复命令执行失败：${exitText}`);
        }
      } else {
        message.success('审批已更新');
      }
      setPendingApproval(null);
      invalidate();
    },
    onError: (error) => {
      message.error(describeFailure(failureFromError(error)));
    },
  });

  const completeMutation = useMutation({
    mutationFn: async () => {
      const { data, error, response } = await api.POST('/api/diagnosis/sessions/{id}/complete', {
        params: { header: writeHeaders(), path: { id: sessionId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      message.success('诊断会话已标记完成');
      invalidate();
    },
    onError: (error) => {
      message.error(describePhase5Failure(failureFromError(error)));
    },
  });

  return (
    <Drawer
      title="AI 诊断"
      open={open}
      onClose={() => {
        setPendingApproval(null);
        onClose();
      }}
      width={640}
      destroyOnHidden
    >
      {sessionQuery.isLoading ? (
        <div style={{ textAlign: 'center', padding: 32 }}>
          <Spin />
        </div>
      ) : sessionQuery.isError ? (
        <QueryErrorState
          error={sessionQuery.error}
          onRetry={() => {
            void sessionQuery.refetch();
          }}
        />
      ) : session ? (
        <DiagnosisBody
          session={session}
          pendingApproval={effectiveApproval}
          fixPending={fixMutation.isPending}
          decidePending={decideMutation.isPending}
          completePending={completeMutation.isPending}
          onFix={(command) => {
            fixMutation.mutate(command);
          }}
          onDecide={(decision, editedCommand) => {
            // .id 防御：审批对象若缺 id（异常数据），decide 会打到字面
            // 模板 URL 500——不触发而非报错。
            if (effectiveApproval?.id) {
              decideMutation.mutate({
                approvalId: effectiveApproval.id,
                decision,
                ...(editedCommand !== undefined ? { editedCommand } : {}),
              });
            }
          }}
          onComplete={() => {
            completeMutation.mutate();
          }}
        />
      ) : null}
    </Drawer>
  );
}

interface DiagnosisBodyProps {
  session: DiagnosisSession;
  pendingApproval: Approval | null;
  fixPending: boolean;
  decidePending: boolean;
  completePending: boolean;
  onFix: (command: string) => void;
  /** editedCommand 仅编辑态「保存并批准」携带（六期打磨 sprint 3）。 */
  onDecide: (decision: 'approve' | 'deny', editedCommand?: string) => void;
  onComplete: () => void;
}

function DiagnosisBody({
  session,
  pendingApproval,
  fixPending,
  decidePending,
  completePending,
  onFix,
  onDecide,
  onComplete,
}: DiagnosisBodyProps) {
  const report = session.report;
  // 六期打磨 sprint 3：修复审批的编辑命令态（此前该入口只能批准/拒绝）。
  const [editing, setEditing] = useState(false);
  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Descriptions size="small" column={2} bordered>
        <Descriptions.Item label="服务器" span={1}>
          {session.server_name || session.server_id.slice(0, 8)}
        </Descriptions.Item>
        <Descriptions.Item label="状态" span={1}>
          <Tag color={DIAGNOSIS_STATUS_COLOR[session.status] ?? 'default'}>
            {DIAGNOSIS_STATUS_LABELS[session.status] ?? session.status}
          </Tag>
        </Descriptions.Item>
        <Descriptions.Item label="开始时间" span={1}>
          {formatDateTime(session.started_at ?? session.created_at)}
        </Descriptions.Item>
        <Descriptions.Item label="触发来源" span={1}>
          {session.trigger === 'alert'
            ? '告警自动'
            : session.trigger === 'manual'
              ? '手动'
              : '对话'}
        </Descriptions.Item>
      </Descriptions>

      {session.error_code ? (
        <Alert
          type="error"
          showIcon
          message={describePhase5Failure({
            code: session.error_code,
            message: session.error_code,
            requestId: '',
            details: null,
            status: null,
          })}
        />
      ) : null}

      {isDiagnosisActive(session.status) ? (
        <Alert
          type="info"
          showIcon
          message="AI 正在采集证据并生成诊断报告……"
          description="诊断引擎在独立会话内执行安全采集命令（仅 safe 级别），完成后自动展示报告。"
        />
      ) : null}

      {report ? <ReportView report={report} fixPending={fixPending} onFix={onFix} /> : null}

      {pendingApproval ? (
        <Alert
          type="warning"
          showIcon
          message="待审批的修复命令"
          description={
            <div>
              <Typography.Paragraph style={{ marginBottom: 8 }}>
                <span className="opsmind-admin-mono">{pendingApproval.command}</span>
              </Typography.Paragraph>
              <Space wrap>
                <RiskBadge level={pendingApproval.risk_level} />
                <Button
                  size="small"
                  type="primary"
                  loading={decidePending}
                  onClick={() => {
                    onDecide('approve');
                  }}
                >
                  批准执行
                </Button>
                <Button
                  size="small"
                  danger
                  loading={decidePending}
                  onClick={() => {
                    onDecide('deny');
                  }}
                >
                  拒绝
                </Button>
                {/* 六期打磨 sprint 3（走查 medium）：诊断修复审批此前只能
                    批准/拒绝，六期改写能力只在对话审批卡可用。编辑态复用
                    ApprovalCommandEditor（同一套校验/预览/请求体构造）。 */}
                <Button
                  size="small"
                  icon={<EditOutlined />}
                  disabled={decidePending}
                  onClick={() => setEditing((value) => !value)}
                >
                  {editing ? '取消编辑' : '编辑命令'}
                </Button>
              </Space>
              {editing ? (
                <div style={{ marginTop: 8 }}>
                  <ApprovalCommandEditor
                    command={pendingApproval.command}
                    riskLevel={pendingApproval.risk_level}
                    serverId={pendingApproval.server_id}
                    pending={decidePending}
                    onSubmit={(body) => {
                      onDecide('approve', body.edited_command);
                    }}
                  />
                </div>
              ) : null}
            </div>
          }
        />
      ) : null}

      {session.status === 'awaiting_fix' ? (
        <Button type="primary" loading={completePending} onClick={onComplete} block>
          标记处理完成（不再修复）
        </Button>
      ) : null}
    </Space>
  );
}

function ReportView({
  report,
  fixPending,
  onFix,
}: {
  report: NonNullable<DiagnosisSession['report']>;
  fixPending: boolean;
  onFix: (command: string) => void;
}) {
  const severityColor: Record<string, string> = {
    critical: 'red',
    warning: 'gold',
    info: 'blue',
  };
  return (
    <>
      <Descriptions size="small" column={3} bordered>
        <Descriptions.Item label="严重度" span={1}>
          <Tag color={severityColor[report.severity] ?? 'default'}>{report.severity}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="置信度" span={1}>
          {report.confidence === 'high' ? '高' : report.confidence === 'medium' ? '中' : '低'}
        </Descriptions.Item>
        <Descriptions.Item label="建议数" span={1}>
          {report.suggestions.length}
        </Descriptions.Item>
      </Descriptions>
      <Typography.Paragraph>{report.summary}</Typography.Paragraph>
      {report.root_cause ? (
        <>
          <Typography.Title level={5} style={{ marginBottom: 8 }}>
            原因分析
          </Typography.Title>
          <Typography.Paragraph type="secondary">{report.root_cause}</Typography.Paragraph>
        </>
      ) : null}
      {report.evidence.length > 0 ? (
        <Collapse
          items={report.evidence.map((evidence: DiagnosisEvidence, index: number) => ({
            key: index,
            label: `证据 ${index + 1}：${evidence.source}${evidence.note ? ` · ${evidence.note}` : ''}`,
            children: (
              <Space direction="vertical" size={4} style={{ width: '100%' }}>
                {evidence.command ? (
                  <Typography.Text code>{evidence.command}</Typography.Text>
                ) : null}
                <Typography.Paragraph style={{ whiteSpace: 'pre-wrap', marginBottom: 0 }}>
                  {evidence.output_excerpt}
                </Typography.Paragraph>
              </Space>
            ),
          }))}
        />
      ) : null}
      {report.suggestions.length > 0 ? (
        <>
          <Typography.Title level={5} style={{ marginBottom: 8 }}>
            修复建议
          </Typography.Title>
          <Space direction="vertical" size={8} style={{ width: '100%' }}>
            {report.suggestions.map((suggestion: FixSuggestion) => (
              <div
                key={suggestion.command}
                style={{
                  border: '1px solid var(--color-border-subtle)',
                  borderRadius: 8,
                  padding: '8px 12px',
                }}
              >
                <Space direction="vertical" size={4} style={{ width: '100%' }}>
                  <CodeBlock language="bash" code={suggestion.command} closed />
                  <Space size={8} wrap>
                    <RiskBadge level={suggestion.risk_level} />
                    <Typography.Text type="secondary">{suggestion.reason}</Typography.Text>
                    <Button
                      size="small"
                      type="primary"
                      loading={fixPending}
                      onClick={() => {
                        onFix(suggestion.command);
                      }}
                    >
                      执行（需审批）
                    </Button>
                  </Space>
                </Space>
              </div>
            ))}
          </Space>
        </>
      ) : null}
    </>
  );
}
