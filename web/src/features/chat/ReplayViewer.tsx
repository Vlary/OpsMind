// FE-P2（六期）：会话执行回放播放器（契约 2.11 约定 4 / 8.7「回放播放器」）。
//
// 数据只来自 GET /api/conversations/:id/replay（ReplayResponse，openapi 已
// 固定字段全集；未用字段为 null）。播放器：播放/暂停/上一步/下一步/速度
// （1x/2x/4x → 800/400/200ms 步进）/进度条（seq 比例）；播放逐条高亮推进
// （已播放步渲染、当前步高亮 + 自动滚动）；步骤卡片按 kind 分色渲染；
// execution 步的命令/输出/退出码全部带 CopyButton；approval 步展示批准/
// 拒绝/超时与「用户改写」双行命令。空态/加载态/错误态齐全。

import {
  CaretRightOutlined,
  PauseOutlined,
  StepBackwardOutlined,
  StepForwardOutlined,
} from '@ant-design/icons';
import { useQuery } from '@tanstack/react-query';
import {
  Alert,
  Button,
  Collapse,
  Modal,
  Select,
  Slider,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import CodeBlock from '../../components/CodeBlock';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import ErrorBanner from '../../components/ErrorBanner';
import LazyMarkdownView from '../../components/LazyMarkdownView';
// FE-P1（六期）的深度思考块：回放 reasoning 步复用（契约 8.6 第 13 条）。
// 六期打磨 sprint 1：回放步骤载荷补 reasoning_duration_ms 后透传 durationMs，
// 摘要与消息页一致显示「深度思考 · 耗时 Ns」（旧数据为 null 时显示「耗时 —」）；
// 默认收起、展开可复制全文、折叠不丢内容。
import ReasoningBlock from '../../components/ReasoningBlock';
import RiskBadge from '../../components/RiskBadge';
import { stripContextPrefix } from './markdownBlocks';
import {
  REPLAY_KIND_CLASS,
  REPLAY_KIND_LABELS,
  REPLAY_SPEEDS,
  advanceTick,
  approvalSourceLabel,
  approvalStatusLabel,
  approvedToolCallIds,
  clampStepIndex,
  executionStatusLabel,
  isEditedApproval,
  isReplayExemptExecution,
  isReplaySpeed,
  progressToIndex,
  replayProgress,
  stepBy,
  stepIntervalMs,
  stepSummary,
  toolCallStatusLabel,
  type ReplaySpeed,
  type ReplayStep,
} from './replayPlayer';
import { EXEMPT_EXECUTION_LABEL } from './toolCallBadges';
import { describeError, toApiErrorInfo } from './useChatStream';
import './replay.css';

export interface ReplayViewerProps {
  open: boolean;
  conversationId: string;
  /** 会话标题（Modal 标题展示，可选）。 */
  title?: string | undefined;
  onClose: () => void;
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

/**
 * reasoning 步：复用 FE-P1 的 ReasoningBlock（默认收起摘要「深度思考 · 耗时 —」、
 * 展开可复制全文、折叠不丢内容）。外层补紫斜体的 kind 分色边框
 * （StepCard 的 replay-step--purple 已覆盖）。
 */

function StepCard({
  step,
  active,
  hasApproval,
}: {
  step: ReplayStep;
  active: boolean;
  /** 同一 tool_call_id 是否存在 approval 步（真走了审批就不算免审批执行）。 */
  hasApproval: boolean;
}) {
  const cls = REPLAY_KIND_CLASS[step.kind];
  // 免审批档直执行（六期打磨 sprint 2）：与聊天工具卡同一判定，见
  // isReplayExemptExecution。
  const exempt = step.kind === 'tool_call' && isReplayExemptExecution(step, hasApproval);
  return (
    <div
      className={`replay-step replay-step--${cls}${active ? ' replay-step--active' : ''}`}
      data-seq={step.seq}
      data-kind={step.kind}
    >
      <div className="replay-step-head">
        <span className="replay-step-seq">#{step.seq}</span>
        <span className="replay-step-kind">{REPLAY_KIND_LABELS[step.kind]}</span>
        {step.kind === 'tool_call' ? (
          <>
            <Typography.Text strong>{step.name ?? '—'}</Typography.Text>
            <Tag>{toolCallStatusLabel(step.status)}</Tag>
            {/* 免审批直执行时隐藏 RiskBadge「需审批」，改由右下的
                「高风险 · 免审批执行」承担风险提示。 */}
            {step.risk_level && !exempt ? <RiskBadge level={step.risk_level} /> : null}
            {exempt ? <Tag color="red">{EXEMPT_EXECUTION_LABEL}</Tag> : null}
            {step.error_code ? (
              <Tooltip title={step.error_code}>
                <Tag color="red">错误</Tag>
              </Tooltip>
            ) : null}
          </>
        ) : null}
        {step.kind === 'approval' ? (
          <>
            <Tag
              color={
                step.status === 'approved'
                  ? 'green'
                  : step.status === 'pending'
                    ? 'gold'
                    : 'default'
              }
            >
              {approvalStatusLabel(step.status)}
            </Tag>
            {isEditedApproval(step) ? <Tag color="purple">用户改写</Tag> : null}
            {step.server_name ? (
              <Typography.Text type="secondary">{step.server_name}</Typography.Text>
            ) : null}
            {/* 六期打磨 sprint 5（走查 low）：来源中文化——此前直出
                chat/console/batch，是全站唯一的中英混排。 */}
            {step.source ? <Tag>{approvalSourceLabel(step.source)}</Tag> : null}
          </>
        ) : null}
        {step.kind === 'execution' ? (
          <>
            <Tag color={step.status === 'succeeded' ? 'green' : 'red'}>
              {executionStatusLabel(step.status)}
            </Tag>
            {step.exit_code !== null && step.exit_code !== undefined ? (
              <Tag>退出码 {step.exit_code}</Tag>
            ) : null}
            {step.duration_ms !== null && step.duration_ms !== undefined ? (
              <Typography.Text type="secondary">{step.duration_ms} ms</Typography.Text>
            ) : null}
          </>
        ) : null}
      </div>

      {step.kind === 'user_message' || step.kind === 'assistant_text' ? (
        // 六期打磨 sprint 2（走查 medium）：user_message 的 content 可能仍带
        // ChatPage 的 `<!--opsmind-context … -->` 内部上下文前缀（后端已在
        // buildReplaySteps 剥离；这里对旧缓存/旧后端响应再兜一层，渲染口径
        // 与聊天气泡的 stripContextPrefix 完全一致）。
        //
        // 六期打磨 sprint 6（走查 medium）：正文改走 LazyMarkdownView（与
        // MessageItem 同源，不引入新依赖）——此前纯文本 <p> 把 Markdown
        // 代码围栏原样显示成 ```bash 字面量，代码块既无高亮也没有「复制代码」
        // 按钮，与聊天页同一条回答的呈现不一致。user_message 保持纯文本气泡
        // 语义（与 MessageItem 的用户侧 chat-user-text 一致：用户输入不渲染
        // Markdown）；assistant_text 用 Markdown 渲染。
        step.kind === 'assistant_text' ? (
          <div className="replay-step-content replay-step-content--markdown">
            <LazyMarkdownView content={step.content ?? ''} />
          </div>
        ) : (
          <p className="replay-step-content">{stripContextPrefix(step.content ?? '')}</p>
        )
      ) : null}

      {step.kind === 'reasoning' ? (
        <ReasoningBlock
          reasoning={step.content ?? ''}
          durationMs={step.reasoning_duration_ms ?? null}
        />
      ) : null}

      {step.kind === 'tool_call' ? (
        <div style={{ marginTop: 6 }}>
          <CodeBlock code={stringify(step.arguments) || '{}'} language="json" closed />
        </div>
      ) : null}

      {step.kind === 'approval' ? (
        <div style={{ marginTop: 6, display: 'flex', flexDirection: 'column', gap: 6 }}>
          {isEditedApproval(step) ? (
            <div>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                原命令
              </Typography.Text>
              <div className="replay-command-original">{step.original_command}</div>
            </div>
          ) : null}
          <div>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {isEditedApproval(step) ? '改写后命令' : '命令'}
            </Typography.Text>
            <CodeBlock code={step.command ?? ''} language="bash" closed />
          </div>
          {step.risk_level ? <RiskBadge level={step.risk_level} /> : null}
          {step.edited_risk_level ? (
            <Space size={4}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                改写后级别
              </Typography.Text>
              <RiskBadge level={step.edited_risk_level} />
            </Space>
          ) : null}
        </div>
      ) : null}

      {step.kind === 'execution' ? (
        <div style={{ marginTop: 6, display: 'flex', flexDirection: 'column', gap: 6 }}>
          {/* 六期打磨 sprint 3（走查 low）：无 result 的通道失败步（后端
              replayExecutionStep 的 failed 分支）不设 command——此前直接渲染
              `step.command ?? ''`，出现空白「命令」代码块 + 禁用状态的复制
              按钮（空串无可复制）。没有命令事实就如实说「未知」，同时保留
              下方 error_code 说明失败原因。 */}
          {step.command ? (
            <div>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                命令
              </Typography.Text>
              <CodeBlock code={step.command} language="bash" closed />
            </div>
          ) : (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              命令未知（通道失败前未下发）
            </Typography.Text>
          )}
          {step.output !== null && step.output !== undefined && step.output.length > 0 ? (
            // 契约 2.11/8.7「execution 步折叠输出」：长输出默认收起，
            // 标题带「输出」+ 复制按钮；展开态仍走 content-visibility:auto
            // （11 节防 OOM 第 17 条——只对可见区布局）。
            <Collapse
              className="replay-output-collapse"
              items={[
                {
                  key: 'output',
                  // 六期打磨 sprint 4（走查 medium）：复制按钮放在 Collapse
                  // 的 label 区，点击会冒泡到 header 触发开合——用户点一次
                  // 「复制输出」面板就自行展开/收起，阅读位置丢失。外层
                  // span 拦下 click 与 keydown（键盘回车同样会冒泡到 header
                  // 的 role=button 处理器）。
                  label: (
                    <Space size={6}>
                      <span>输出</span>
                      <span
                        onClick={(event) => event.stopPropagation()}
                        onKeyDown={(event) => event.stopPropagation()}
                      >
                        <CopyButton text={step.output} label="复制输出" />
                      </span>
                    </Space>
                  ),
                  children: (
                    <div className="replay-output">
                      <CodeBlock code={step.output} closed />
                    </div>
                  ),
                },
              ]}
            />
          ) : null}
          {step.error_code ? (
            <Typography.Text type="danger" style={{ fontSize: 12 }}>
              {describeError(step.error_code)}
            </Typography.Text>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

export default function ReplayViewer({ open, conversationId, title, onClose }: ReplayViewerProps) {
  const [index, setIndex] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState<ReplaySpeed>(1);
  const listRef = useRef<HTMLDivElement>(null);

  const replayQuery = useQuery({
    queryKey: qk.replay(conversationId),
    queryFn: async () => {
      const { data, error } = await api.GET('/api/conversations/{id}/replay', {
        params: { path: { id: conversationId } },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    // 契约 8.4：staleTime 0，每次打开重取（避免读到流式半途数据）。
    enabled: open && conversationId.length > 0,
    staleTime: 0,
    refetchOnMount: 'always',
  });

  const steps = useMemo(() => replayQuery.data?.steps ?? [], [replayQuery.data]);
  // 已创建审批的 tool_call_id 集合（六期打磨 sprint 2）：同一次调用真走了
  // 审批时，回放不得把它显示成「免审批执行」。
  const approvedIds = useMemo(() => approvedToolCallIds(steps), [steps]);
  const truncated = replayQuery.data?.truncated ?? false;
  // 截断原因二选一文案（六期打磨 sprint 4 走查 low）：此前两种截断共用
  // truncated 标志，横幅硬编码「超出 20000 步上限」——审批表单状态满
  // 200 条时用户看到 200 步却被告知 20000 上限，自相矛盾。
  const truncatedReason = replayQuery.data?.truncated_reason ?? '';
  const total = steps.length;
  const truncatedBanner = !truncated
    ? null
    : truncatedReason === 'approvals'
      ? `审批记录过多，仅显示最近 ${total} 步（更早的审批未纳入回放）`
      : `超出 20000 步上限，仅显示最近 ${total} 步`;
  // 派生安全下标：refetch 后 total 缩小时越界 index 在渲染期钳制
  //（不用 effect setState——react-hooks/set-state-in-effect）。会话切换由
  // 调用方 key={conversationId} 重挂载复位，无需 effect。
  const safeIndex = clampStepIndex(index, total);
  const atEnd = total > 0 && safeIndex >= total - 1;
  // 有效播放态：到末步/空列表自动视为暂停（终止条件），原始 playing 状态
  // 由下一次操作（播放=从头重放 / 暂停 / 拖动）覆写。
  const effectivePlaying = playing && !atEnd && total > 0;

  // 自动播放：步进定时器随速度换档重建；effectivePlaying=false 时不排程。
  useEffect(() => {
    if (!effectivePlaying) {
      return undefined;
    }
    const timer = window.setTimeout(
      () =>
        setIndex(
          (current) =>
            advanceTick({ index: clampStepIndex(current, total), playing: true }, total).index,
        ),
      stepIntervalMs(speed),
    );
    return () => window.clearTimeout(timer);
  }, [effectivePlaying, safeIndex, speed, total]);

  // 播放推进时当前步滚动入视野。
  useEffect(() => {
    const seq = steps[safeIndex]?.seq;
    if (seq === undefined) {
      return;
    }
    listRef.current
      ?.querySelector(`[data-seq="${seq}"]`)
      ?.scrollIntoView({ block: 'nearest', behavior: effectivePlaying ? 'smooth' : 'auto' });
  }, [safeIndex, steps, effectivePlaying]);

  const handlePlay = () => {
    if (total === 0) {
      return;
    }
    if (atEnd) {
      // 末步再按播放：从头重放。
      setIndex(0);
      setPlaying(true);
      return;
    }
    setPlaying((value) => !value);
  };

  const handleStep = (delta: number) => {
    setPlaying(false);
    setIndex((current) => stepBy({ index: current, playing: false }, delta, total).index);
  };

  const handleProgress = (percent: number) => {
    setPlaying(false);
    setIndex(progressToIndex(percent, total));
  };

  const errorInfo = replayQuery.error ? toApiErrorInfo(replayQuery.error) : null;
  const currentStep = steps[safeIndex];
  const body = replayQuery.isPending ? (
    <div style={{ display: 'flex', justifyContent: 'center', padding: 48 }}>
      <Spin />
    </div>
  ) : errorInfo ? (
    <ErrorBanner
      code={errorInfo.code}
      message={describeError(errorInfo.code, errorInfo.message)}
      requestId={errorInfo.requestId}
      onRetry={() => void replayQuery.refetch()}
    />
  ) : total === 0 ? (
    <EmptyState description="该会话暂无可回放的步骤" />
  ) : (
    <div ref={listRef} className="replay-steps" data-testid="replay-steps">
      {steps.slice(0, safeIndex + 1).map((step, i) => (
        <StepCard
          key={step.seq}
          step={step}
          active={i === safeIndex}
          hasApproval={typeof step.tool_call_id === 'string' && approvedIds.has(step.tool_call_id)}
        />
      ))}
    </div>
  );

  return (
    <Modal
      open={open}
      onCancel={() => {
        setPlaying(false);
        onClose();
      }}
      title={`执行回放${title ? ` · ${title}` : ''}`}
      width="min(100vw - 32px, 1080px)"
      footer={null}
      styles={{
        body: {
          height: 'calc(100vh - 160px)',
          minHeight: 320,
          display: 'flex',
          flexDirection: 'column',
          paddingInline: 0,
        },
      }}
      destroyOnHidden
    >
      {truncatedBanner !== null ? (
        <Alert
          type="warning"
          showIcon
          style={{ margin: '0 16px 8px' }}
          message={truncatedBanner}
        />
      ) : null}

      {body}

      <div className="replay-controls">
        <div className="replay-controls-row">
          <Button.Group>
            <Button
              icon={<StepBackwardOutlined />}
              aria-label="上一步"
              disabled={total === 0 || safeIndex === 0}
              onClick={() => handleStep(-1)}
            />
            <Button
              icon={effectivePlaying ? <PauseOutlined /> : <CaretRightOutlined />}
              aria-label={effectivePlaying ? '暂停' : '播放'}
              disabled={total === 0}
              onClick={handlePlay}
            />
            <Button
              icon={<StepForwardOutlined />}
              aria-label="下一步"
              disabled={total === 0 || atEnd}
              onClick={() => handleStep(1)}
            />
          </Button.Group>
          <Select<ReplaySpeed>
            aria-label="播放速度"
            value={speed}
            style={{ width: 84 }}
            options={REPLAY_SPEEDS.map((value) => ({ value, label: `${value}x` }))}
            onChange={(value) => {
              if (isReplaySpeed(value)) {
                setSpeed(value);
              }
            }}
          />
          <span className="replay-counter">
            {total > 0 ? `第 ${safeIndex + 1} / ${total} 步` : '0 步'}
          </span>
          <span className="replay-summary">
            {currentStep
              ? `${REPLAY_KIND_LABELS[currentStep.kind]} · ${stepSummary(currentStep)}`
              : ''}
          </span>
        </div>
        <Slider
          min={0}
          max={100}
          step={0.5}
          value={replayProgress(safeIndex, total)}
          disabled={total === 0}
          tooltip={{
            formatter: () => (total > 0 ? `第 ${safeIndex + 1} / ${total} 步` : ''),
          }}
          onChange={(value) => handleProgress(value)}
          aria-label="回放进度"
        />
      </div>
    </Modal>
  );
}
