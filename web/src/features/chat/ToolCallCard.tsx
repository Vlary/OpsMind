import {
  CheckCircleOutlined,
  CloseCircleOutlined,
  LoadingOutlined,
  ToolOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { Card, Collapse, Space, Tag, Typography } from 'antd';
import { Link } from 'react-router-dom';
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import type { components } from '../../api/schema';
import ApprovalCard from '../../components/ApprovalCard';
import CodeBlock from '../../components/CodeBlock';
import RiskBadge from '../../components/RiskBadge';
import { ApprovalModeTag } from '../../components/ApprovalModeSelect';
import StatusTag from '../../components/StatusTag';
import { BATCH_STATUS_LABELS, RESULT_STATUS_LABELS } from '../batch/batchView';
import { describeError, showsDuration, type ToolCallView } from './useChatStream';
import { commandPanelOf } from './toolCommandPanel';
import { EXEMPT_EXECUTION_LABEL, badgeInputOf, isExemptExecution } from './toolCallBadges';

type Approval = components['schemas']['Approval'];

/**
 * 五期工具专属结果渲染（契约 8.7）：
 * - batch_execute：状态计数行 + 「查看批量任务」链接（/batch?job=<id>）；
 * - run_diagnosis：摘要 + 「查看诊断会话」链接（/alerts?diagnosis=<id>，
 *   建议行含 RiskBadge 与「执行（需审批）」的操作在诊断会话抽屉内——
 *   不在对话卡片内直接执行）。
 * 返回 null 表示非五期工具，走通用 JSON 折叠。
 * 两个改法（五期走查）：
 * 1) 链接一律 target="_blank" 新标签打开——/batch、/alerts 的审批独立于
 *    本会话流，但 ChatPage 的离开守卫（streaming 时 useBlocker）会拦 SPA
 *    导航并弹「审批将失效」的误导文案；新标签打开不触发 blocker，用户
 *    按工具 note 的引导跳转不再被自己的守卫拦截。
 * 2) 本视图由 ToolCallCard 提升到折叠面板外直渲染（「结果」面板默认收起，
 *    藏在里面用户发现不了跳转入口）；折叠面板内只留原始 JSON。
 */
function phaseFiveResult(view: ToolCallView): ReactNode {
  const result = view.result;
  if (result === null || typeof result !== 'object') {
    return null;
  }
  const record = result as Record<string, unknown>;
  if (view.name === 'batch_execute' && typeof record['batch_job_id'] === 'string') {
    const jobId = record['batch_job_id'];
    const total = typeof record['total'] === 'number' ? record['total'] : null;
    const status = typeof record['status'] === 'string' ? record['status'] : '';
    // 五期 sprint 4：状态 Tag/计数行与批量页同口径中文（batchView 的
    // BATCH_STATUS_LABELS / RESULT_STATUS_LABELS），不再直显英文状态键
    // （awaiting_approval 1 → 待审批 1）。计数键为后端装配的全部逐台结果
    // 状态（runner_batch.go counts[string(res.Status)]++），含 pending/
    // cancelled（旧实现漏列）。
    const counts: string[] = [];
    for (const key of [
      'pending',
      'awaiting_approval',
      'executed',
      'failed',
      'denied',
      'expired',
      'skipped',
      'cancelled',
    ]) {
      if (typeof record[key] === 'number') {
        counts.push(`${RESULT_STATUS_LABELS[key] ?? key} ${record[key] as number}`);
      }
    }
    return (
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        <Space wrap size={12}>
          <Tag>{BATCH_STATUS_LABELS[status] ?? status}</Tag>
          {total !== null ? (
            <Typography.Text type="secondary">共 {total} 台</Typography.Text>
          ) : null}
          {counts.map((line) => (
            <Typography.Text key={line} type="secondary" style={{ fontSize: 12 }}>
              {line}
            </Typography.Text>
          ))}
        </Space>
        <Link to={`/batch?job=${jobId}`} target="_blank" rel="noopener noreferrer">
          查看批量任务
        </Link>
      </Space>
    );
  }
  if (view.name === 'run_diagnosis' && typeof record['diagnosis_id'] === 'string') {
    const summary = typeof record['summary'] === 'string' ? record['summary'] : null;
    const severity = typeof record['severity'] === 'string' ? record['severity'] : null;
    return (
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        {severity ? (
          <Tag color={severity === 'critical' ? 'red' : severity === 'warning' ? 'gold' : 'green'}>
            {severity}
          </Tag>
        ) : null}
        {summary ? <Typography.Text>{summary}</Typography.Text> : null}
        <Link
          to={`/alerts?diagnosis=${record['diagnosis_id']}`}
          target="_blank"
          rel="noopener noreferrer"
        >
          查看诊断会话（建议项的「执行（需审批）」在会话内操作）
        </Link>
      </Space>
    );
  }
  return null;
}

function stringify(value: unknown): string {
  if (value === null || value === undefined) {
    return '';
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

/** 排查用小字：底层英文原因（含错误码），与中文主文案互补。 */
function errorDetail(view: ToolCallView): string {
  return [view.errorMessage, view.errorCode].filter((part) => part.length > 0).join(' · ');
}

function statusIcon(status: string) {
  if (status === 'started' || status === 'awaiting_approval') {
    return <LoadingOutlined />;
  }
  if (status === 'succeeded') {
    return <CheckCircleOutlined style={{ color: 'var(--color-success)' }} />;
  }
  if (status === 'failed' || status === 'denied' || status === 'expired') {
    return <CloseCircleOutlined style={{ color: 'var(--color-danger)' }} />;
  }
  return <ToolOutlined />;
}

export interface ToolCallCardProps {
  toolCall: ToolCallView;
  approvals?: Approval[];
  conversationId?: string;
  onApprovalUpdated?: (approval: Approval) => void;
}

export default function ToolCallCard({
  toolCall,
  approvals = [],
  conversationId,
  onApprovalUpdated,
}: ToolCallCardProps) {
  // 六期打磨 sprint 5（走查 low）：命令面板区分「模型原命令」与「实际
  // 执行」——用户改写后，arguments.command 仍是模型原始参数（契约 2.11
  // 约定 3 有意保留），面板此前只显示它；实际执行的是 edited_command。
  const command = useMemo(
    () => commandPanelOf({ arguments: toolCall.arguments, result: toolCall.result }, approvals),
    [toolCall, approvals],
  );
  // 五期专属视图直渲染在折叠面板外（「结果」面板默认收起，专属视图藏在
  // 里面时用户发现不了跳转入口——走查实测「查看批量任务」不可见）。
  const phaseFive = useMemo(() => phaseFiveResult(toolCall), [toolCall]);
  const highRisk =
    toolCall.riskLevel === 'needs_approval' ||
    toolCall.riskLevel === 'forbidden' ||
    toolCall.errorCode === 'COMMAND_FORBIDDEN';
  // 六期打磨 sprint 1（契约 8.7 / 2.11 约定 2）：免审批档下 needs_approval
  // 级命令实际直执行时，RiskBadge「需审批」与实际不符，且「高风险」Tag
  // 与「免审批」并列会误导——此处把两者改写为单一「高风险 · 免审批执行」
  // 提示（判定纯函数见 toolCallBadges.ts）。
  const exempt = isExemptExecution(badgeInputOf(toolCall, approvals));
  const failed = toolCall.status === 'failed' || toolCall.status === 'denied';

  const collapseItems = [
    ...(command
      ? [
          {
            key: 'command',
            label: (
              // 改写后：面板标题为「模型原命令」，红色高风险标注随之取消
              // ——真正执行的是改写后的安全命令，继续给原命令标红会误导。
              <Typography.Text type={highRisk && !command.edited ? 'danger' : 'secondary'}>
                {command.label}
              </Typography.Text>
            ),
            children: (
              <Space direction="vertical" size={6} style={{ width: '100%' }}>
                <CodeBlock code={command.text} language={command.language} closed />
                {command.executed !== null ? (
                  <div>
                    <Space size={6} wrap>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        实际执行
                      </Typography.Text>
                      <Tag color="purple">用户改写</Tag>
                    </Space>
                    <CodeBlock code={command.executed} language={command.language} closed />
                  </div>
                ) : null}
              </Space>
            ),
          },
        ]
      : []),
    {
      key: 'arguments',
      label: '参数',
      children: <CodeBlock code={stringify(toolCall.arguments) || '{}'} language="json" closed />,
    },
    {
      key: 'result',
      label: '结果',
      children:
        toolCall.result === null && !toolCall.errorCode ? (
          <Typography.Text type="secondary">暂无结果</Typography.Text>
        ) : (
          <Space direction="vertical" size={8} style={{ width: '100%' }}>
            {toolCall.errorCode ? (
              <Space direction="vertical" size={2} style={{ width: '100%' }}>
                <Typography.Text type="danger">{describeError(toolCall.errorCode)}</Typography.Text>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {errorDetail(toolCall)}
                </Typography.Text>
              </Space>
            ) : null}
            {toolCall.result !== null ? (
              <CodeBlock code={stringify(toolCall.result)} language="json" closed />
            ) : null}
          </Space>
        ),
    },
  ];

  return (
    <div className={`chat-toolcall${highRisk ? ' chat-toolcall--danger' : ''}`}>
      <Card
        size="small"
        className="chat-toolcall-card"
        title={
          <Space wrap size={8}>
            {statusIcon(toolCall.status)}
            <Typography.Text strong>{toolCall.name}</Typography.Text>
            <StatusTag status={toolCall.status} />
            {/* 免审批执行时 RiskBadge「需审批」与实际不符，隐藏它（下方
                用「高风险 · 免审批执行」Tag 承担风险提示）。 */}
            {toolCall.riskLevel && !exempt ? <RiskBadge level={toolCall.riskLevel} /> : null}
            {toolCall.approvalMode ? (
              <ApprovalModeTag
                mode={toolCall.approvalMode as components['schemas']['ApprovalMode']}
              />
            ) : null}
            {highRisk ? (
              <Tag color="red" icon={<WarningOutlined />}>
                {exempt ? EXEMPT_EXECUTION_LABEL : '高风险'}
              </Tag>
            ) : null}
            {showsDuration(toolCall) ? (
              <Typography.Text type="secondary">{toolCall.durationMs} ms</Typography.Text>
            ) : null}
          </Space>
        }
      >
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          {toolCall.status === 'awaiting_approval' && approvals.length === 0 ? (
            <Typography.Text type="warning">等待人工审批…</Typography.Text>
          ) : null}

          {phaseFive}

          <Collapse
            ghost
            size="small"
            items={collapseItems}
            defaultActiveKey={command ? ['command'] : []}
          />

          {failed && toolCall.errorCode ? (
            <Space direction="vertical" size={2} style={{ width: '100%' }}>
              <Typography.Text type="danger">
                执行失败：{describeError(toolCall.errorCode)}
              </Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {errorDetail(toolCall)}
              </Typography.Text>
            </Space>
          ) : null}

          {approvals.length > 0
            ? approvals.map((approval) => (
                <ApprovalCard
                  key={approval.id}
                  approval={approval}
                  // 四期：conversation_id 可空（console 审批），工具卡内
                  // ApprovalCard 只需会话维度失效键，回退固定串。
                  conversationId={conversationId ?? approval.conversation_id ?? ''}
                  {...(onApprovalUpdated ? { onUpdated: onApprovalUpdated } : {})}
                />
              ))
            : null}
        </Space>
      </Card>
    </div>
  );
}
