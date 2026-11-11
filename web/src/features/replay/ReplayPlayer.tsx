// FE-R（八期）：独立回放页的右栏播放器（契约 2.12 约定 1/2/5，8.7「回放页」）。
//
// 由六期 `features/chat/ReplayViewer.tsx`（Modal）的播放器逻辑迁移而来——
// Modal 形态下线，**全站只有这一份播放器实现**（禁止第二套）。六期礼数
// 逐条保留：时间轴逐步渲染、步进/自动播放/倍速、kind 分色、truncated 四选
// 一文案、命令改写双行、门限徽标、reasoning 耗时、输出折叠、复制全覆盖。
//
// 八期新增：@tanstack/react-virtual 虚拟滚动（20000 步时 DOM 节点数与总步数
// 无关）、kind/工具/风险筛选（已加载 steps 上的客户端过滤）、大小写不敏感
// 搜索与命中跳转、步骤跳转输入、快捷键、execution 步的「命令/动作」标注与
// result_summary / affected_servers / result_truncated / 诊断巡检批量深链。

import {
  CaretRightOutlined,
  PauseOutlined,
  StepBackwardOutlined,
  StepForwardOutlined,
} from '@ant-design/icons';
import { useQuery } from '@tanstack/react-query';
import { useVirtualizer } from '@tanstack/react-virtual';
import {
  Alert,
  Button,
  Collapse,
  Input,
  Select,
  Slider,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { InputRef, RefSelectProps } from 'antd';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import CodeBlock from '../../components/CodeBlock';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import ErrorBanner from '../../components/ErrorBanner';
import LazyMarkdownView from '../../components/LazyMarkdownView';
import ReasoningBlock from '../../components/ReasoningBlock';
import RiskBadge from '../../components/RiskBadge';
import { ApprovalModeTag } from '../../components/ApprovalModeSelect';
import { stripContextPrefix } from '../chat/markdownBlocks';
import { EXEMPT_EXECUTION_LABEL } from '../chat/toolCallBadges';
import { describeError, toApiErrorInfo } from '../chat/useChatStream';
import {
  EMPTY_REPLAY_FILTER,
  REPLAY_ACTION_TOOLS,
  REPLAY_KIND_CLASS,
  REPLAY_KINDS,
  REPLAY_KIND_LABELS,
  REPLAY_LOCAL_TOOLS,
  REPLAY_RISK_LABELS,
  REPLAY_RISK_LEVELS,
  REPLAY_SPEEDS,
  advanceTick,
  approvalSourceLabel,
  approvalStatusLabel,
  approvedToolCallIds,
  clampStepIndex,
  executionCommandLabel,
  executionStatusLabel,
  filterSteps,
  firstMatchExcerpt,
  hotkeyAction,
  isActivationTarget,
  isEditedApproval,
  isEditableTarget,
  isFilterEmpty,
  isReplayExemptExecution,
  isReplaySpeed,
  matchLabel,
  mergeFilterLabel,
  nextMatch,
  parseStepJump,
  prevMatch,
  progressToIndex,
  replayProgress,
  searchSteps,
  stepBy,
  stepIntervalMs,
  stepSummary,
  toolCallStatusLabel,
  truncatedBanner,
  type ReplayFilter,
  type ReplayHotkeyAction,
  type ReplayRiskFilter,
  type ReplaySpeed,
  type ReplayStep,
} from './replayPlayer';
import './replay.css';

export interface ReplayPlayerProps {
  /** 要回放的会话 ID；空串/undefined 表示未选中（不取数）。 */
  conversationId?: string | undefined;
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

/** 工具筛选项（按契约 7.8 的 11 动作类 / 7 本地查询分组）。 */
const TOOL_FILTER_OPTIONS = [
  {
    label: '动作类（触达真实系统）',
    options: REPLAY_ACTION_TOOLS.map((name) => ({ value: name, label: name })),
  },
  {
    label: '本地查询 / 交互',
    options: REPLAY_LOCAL_TOOLS.map((name) => ({ value: name, label: name })),
  },
];

/**
 * 步骤卡片（六期 StepCard 迁移 + 八期扩展）。
 *
 * 只渲染可视区附近的步（虚拟滚动），所以这里的解析/序列化成本有界；
 * 20000 步时挂载的卡片数仍由 overscan 决定。
 */
function StepCard({
  step,
  active,
  hasApproval,
  query,
}: {
  step: ReplayStep;
  active: boolean;
  /** 同一 tool_call_id 是否存在 approval 步（真走了审批就不算免审批执行）。 */
  hasApproval: boolean;
  /** 当前搜索词（用于命中片段高亮；空串不渲染）。 */
  query: string;
}) {
  const cls = REPLAY_KIND_CLASS[step.kind];
  // 免审批档直执行（六期打磨 sprint 2）：与聊天工具卡同一判定。
  const exempt = step.kind === 'tool_call' && isReplayExemptExecution(step, hasApproval);
  const excerpt = query.trim() === '' ? null : firstMatchExcerpt(step, query);
  const json = useMemo(() => JSON.stringify(step), [step]);

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
            {/* 门限徽标（契约 2.12 约定 7 六期礼数）：none/high 档的
                「高风险 · 免审批执行」之外，仍展示判定时的门限快照。 */}
            {step.approval_mode && step.approval_mode !== 'null' ? (
              <ApprovalModeTag mode={step.approval_mode as components['schemas']['ApprovalMode']} />
            ) : null}
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
            {/* 六期打磨 sprint 5：来源中文化——此前直出 chat/console/batch。 */}
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
            {/* 八期：输出是否完整（契约 2.12 约定 3，唯一判据）。 */}
            {step.result_truncated === true ? <Tag color="orange">输出已截断</Tag> : null}
          </>
        ) : null}
        {/* 每步完整 JSON（契约 8.7 ⑩「复制本步」）。 */}
        <span className="replay-step-copy">
          <CopyButton text={json} label="复制本步" />
        </span>
      </div>

      {/* 搜索命中片段（契约 8.7 ④「命中高亮」）：只渲染第一处命中，
          上下文各截断到固定长度，避免长输出把卡片撑爆。 */}
      {excerpt !== null ? (
        <div className="replay-search-hit" data-field={excerpt.field}>
          <Tag color="blue">命中 · {excerpt.field}</Tag>
          <span>
            {excerpt.before}
            <mark>{excerpt.hit}</mark>
            {excerpt.after}
          </span>
        </div>
      ) : null}

      {step.kind === 'user_message' || step.kind === 'assistant_text' ? (
        // user_message 保持纯文本气泡语义；assistant_text 走 LazyMarkdownView
        // （与 MessageItem 同源）。user_message 的 content 可能仍带
        // ChatPage 的 `<!--opsmind-context … -->` 前缀（旧缓存/旧后端），
        // 这里再兜一层，口径与聊天气泡一致。
        <div className="replay-step-body">
          {step.kind === 'assistant_text' ? (
            <div className="replay-step-content replay-step-content--markdown">
              <LazyMarkdownView content={step.content ?? ''} />
            </div>
          ) : (
            <p className="replay-step-content">{stripContextPrefix(step.content ?? '')}</p>
          )}
          <span className="replay-step-copy">
            <CopyButton text={step.content ?? ''} label="复制正文" />
          </span>
        </div>
      ) : null}

      {step.kind === 'reasoning' ? (
        // 六期礼数：ReasoningBlock 默认收起摘要（耗时来自服务端
        // reasoning_duration_ms）、展开可复制全文、折叠不丢内容。
        <ReasoningBlock
          reasoning={step.content ?? ''}
          durationMs={step.reasoning_duration_ms ?? null}
        />
      ) : null}

      {step.kind === 'tool_call' ? (
        <div className="replay-step-section">
          {step.result_summary ? (
            <Space size={6} wrap>
              <Typography.Text type="secondary" className="replay-inline-label">
                结果
              </Typography.Text>
              <Typography.Text>{step.result_summary}</Typography.Text>
              <CopyButton text={step.result_summary} label="复制结果摘要" />
            </Space>
          ) : null}
          {step.arguments === null || step.arguments === undefined ? (
            // 八期体积预算：arguments 被后端清空（truncated_reason=payload）。
            // 不得渲染成空 JSON 块——那会让用户以为工具没有参数。
            <Typography.Text type="secondary" className="replay-inline-note">
              参数未下发（体积预算）
            </Typography.Text>
          ) : (
            <div>
              <div className="replay-section-label">
                <Typography.Text type="secondary" className="replay-inline-label">
                  参数
                </Typography.Text>
                <CopyButton text={stringify(step.arguments)} label="复制参数" />
              </div>
              <CodeBlock code={stringify(step.arguments) || '{}'} language="json" closed />
            </div>
          )}
        </div>
      ) : null}

      {step.kind === 'approval' ? (
        <div className="replay-step-section">
          {isEditedApproval(step) ? (
            <div>
              <div className="replay-section-label">
                <Typography.Text type="secondary" className="replay-inline-label">
                  原命令
                </Typography.Text>
                <CopyButton text={step.original_command ?? ''} label="复制原命令" />
              </div>
              <div className="replay-command-original">{step.original_command}</div>
            </div>
          ) : null}
          <div>
            <div className="replay-section-label">
              <Typography.Text type="secondary" className="replay-inline-label">
                {isEditedApproval(step) ? '改写后命令' : '命令'}
              </Typography.Text>
              <CopyButton text={step.command ?? ''} label="复制命令" />
            </div>
            <CodeBlock code={step.command ?? ''} language="bash" closed />
          </div>
          {step.risk_level ? <RiskBadge level={step.risk_level} /> : null}
          {step.edited_risk_level ? (
            <Space size={4}>
              <Typography.Text type="secondary" className="replay-inline-label">
                改写后级别
              </Typography.Text>
              <RiskBadge level={step.edited_risk_level} />
            </Space>
          ) : null}
        </div>
      ) : null}

      {step.kind === 'execution' ? (
        <div className="replay-step-section">
          {/* 六期打磨 sprint 3：无 result 的通道失败步不设 command——此前渲染
              空白「命令」代码块 + 禁用复制按钮。没有命令事实就如实说明。 */}
          {step.command ? (
            <div>
              <div className="replay-section-label">
                <Typography.Text type="secondary" className="replay-inline-label">
                  {executionCommandLabel(step.name)}
                </Typography.Text>
                <CopyButton text={step.command} label={`复制${executionCommandLabel(step.name)}`} />
              </div>
              <CodeBlock code={step.command} language="bash" closed />
            </div>
          ) : (
            <Typography.Text type="secondary" className="replay-inline-note">
              命令未知（通道失败前未下发）
            </Typography.Text>
          )}
          {step.result_summary ? (
            <Space size={6} wrap>
              <Typography.Text type="secondary" className="replay-inline-label">
                结果
              </Typography.Text>
              <Typography.Text>{step.result_summary}</Typography.Text>
              <CopyButton text={step.result_summary} label="复制结果摘要" />
            </Space>
          ) : null}
          {step.affected_servers && step.affected_servers.length > 0 ? (
            <Space size={6} wrap>
              <Typography.Text type="secondary" className="replay-inline-label">
                影响主机
              </Typography.Text>
              {step.affected_servers.length > 3 ? (
                <Tooltip title={step.affected_servers.join('、')}>
                  <Tag>{step.affected_servers.length} 台</Tag>
                </Tooltip>
              ) : (
                step.affected_servers.map((name) => <Tag key={name}>{name || '（已删除）'}</Tag>)
              )}
            </Space>
          ) : null}
          {step.output !== null && step.output !== undefined && step.output.length > 0 ? (
            // 六期礼数：长输出默认收起（Collapse），标题带复制按钮；
            // 展开态仍走 content-visibility:auto（防 OOM 第 17 条）。
            <Collapse
              className="replay-output-collapse"
              items={[
                {
                  key: 'output',
                  label: (
                    <Space size={6}>
                      <span>输出</span>
                      <span
                        onClick={(event) => event.stopPropagation()}
                        onKeyDown={(event) => event.stopPropagation()}
                      >
                        <CopyButton text={step.output ?? ''} label="复制输出" />
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
          ) : step.result_truncated === true ? (
            // 8 MiB 预算把 output 清空：输出存在但未下发（与 null 不同）。
            <Typography.Text type="secondary" className="replay-inline-note">
              输出因体积预算未下发（见顶部截断说明）
            </Typography.Text>
          ) : null}
          {step.error_code ? (
            <Typography.Text type="danger" className="replay-inline-note">
              {describeError(step.error_code)}
            </Typography.Text>
          ) : null}
          {/* 八期深链（与对话工具卡同目标）。 */}
          {step.diagnosis_id || step.inspection_report_id || step.batch_job_id ? (
            <Space size={12} wrap>
              {step.diagnosis_id ? (
                <Space size={4}>
                  <Link to={`/alerts?diagnosis=${encodeURIComponent(step.diagnosis_id)}`}>
                    查看诊断会话
                  </Link>
                  <CopyButton text={step.diagnosis_id} label="复制诊断 ID" />
                </Space>
              ) : null}
              {step.inspection_report_id ? (
                <Space size={4}>
                  <Link to="/inspections">查看巡检报告</Link>
                  <CopyButton text={step.inspection_report_id} label="复制巡检报告 ID" />
                </Space>
              ) : null}
              {step.batch_job_id ? (
                <Space size={4}>
                  <Link to={`/batch?job=${encodeURIComponent(step.batch_job_id)}`}>
                    查看批量任务
                  </Link>
                  <CopyButton text={step.batch_job_id} label="复制批量任务 ID" />
                </Space>
              ) : null}
            </Space>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/**
 * 右栏播放器。
 *
 * 数据只来自 `GET /api/conversations/:id/replay`（ReplayResponse，openapi
 * 已固定字段全集；未用字段为 null）。`staleTime: 0` + 每次挂载重取，
 * 与六期一致（避免读到流式半途数据）。
 */
export default function ReplayPlayer({ conversationId }: ReplayPlayerProps) {
  const [index, setIndex] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState<ReplaySpeed>(1);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<ReplayFilter>(EMPTY_REPLAY_FILTER);
  const [matchCursor, setMatchCursor] = useState(0);
  const [jumpText, setJumpText] = useState('');

  const scrollRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<InputRef>(null);
  const kindFilterRef = useRef<RefSelectProps>(null);

  const enabled = typeof conversationId === 'string' && conversationId.length > 0;
  const replayQuery = useQuery({
    queryKey: qk.replay(conversationId ?? ''),
    queryFn: async () => {
      const { data, error } = await api.GET('/api/conversations/{id}/replay', {
        params: { path: { id: conversationId ?? '' } },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    enabled,
    staleTime: 0,
    refetchOnMount: 'always',
  });

  const steps = useMemo(() => replayQuery.data?.steps ?? [], [replayQuery.data]);
  // 已创建审批的 tool_call_id 集合：同一次调用真走了审批时，回放不得把它
  // 显示成「免审批执行」（六期打磨 sprint 2）。
  const approvedIds = useMemo(() => approvedToolCallIds(steps), [steps]);

  // 八期：客户端筛选（不重发 /replay）。空条件返回原数组（同一引用）。
  const visibleSteps = useMemo(() => filterSteps(steps, filter), [steps, filter]);

  // 搜索在**过滤后的可见序列**上做（与筛选叠加，契约 8.7 ④）。
  const matches = useMemo(() => searchSteps(visibleSteps, query), [visibleSteps, query]);
  const matchCount = matches.length;
  const cursor = matchCount === 0 ? 0 : Math.min(Math.max(matchCursor, 0), matchCount - 1);

  const total = visibleSteps.length;
  const rawTotal = steps.length;
  // 派生安全下标：过滤/refetch 后 total 缩小时越界 index 在渲染期钳制
  // （不用 effect setState——react-hooks/set-state-in-effect）。
  const safeIndex = clampStepIndex(index, total);
  const atEnd = total > 0 && safeIndex >= total - 1;
  // 有效播放态：到末步/空列表自动视为暂停（终止条件）。
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

  // 20000 步也只在可视区挂载 DOM：列表包含全部可见步骤（可自由前后滚动查看
  // 时间轴），实际节点数由 overscan 决定（契约 8.7 ⑦「只渲染可视区 ± overscan；
  // 20000 步时 DOM 节点数与总步数无关」）。
  // eslint-disable-next-line react-hooks/incompatible-library -- TanStack Virtual 官方说明需跳过 React Compiler memo 化
  const virtualizer = useVirtualizer({
    count: total,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 140,
    overscan: 8,
    getItemKey: (row) => visibleSteps[row]?.seq ?? row,
  });

  // 当前步滚动入视野（虚拟滚动下用 scrollToIndex 而非 querySelector）。
  const scrollToActive = useCallback(
    (row: number) => {
      virtualizer.scrollToIndex(row, { align: 'auto' });
    },
    [virtualizer],
  );
  useEffect(() => {
    if (total === 0) {
      return;
    }
    scrollToActive(safeIndex);
  }, [safeIndex, total, scrollToActive]);

  const handlePlay = useCallback(() => {
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
  }, [atEnd, total]);

  const handleStep = useCallback(
    (delta: number) => {
      setPlaying(false);
      setIndex((current) => stepBy({ index: current, playing: false }, delta, total).index);
    },
    [total],
  );

  const handleFirst = useCallback(() => {
    setPlaying(false);
    setIndex(0);
  }, []);

  const handleLast = useCallback(() => {
    setPlaying(false);
    setIndex(clampStepIndex(total - 1, total));
  }, [total]);

  const handleProgress = useCallback(
    (percent: number) => {
      setPlaying(false);
      setIndex(progressToIndex(percent, total));
    },
    [total],
  );

  const jumpToMatch = useCallback(
    (delta: 1 | -1) => {
      if (matchCount === 0) {
        return;
      }
      const nextCursor =
        delta === 1 ? nextMatch(cursor, matchCount) : prevMatch(cursor, matchCount);
      setMatchCursor(nextCursor);
      const row = matches[nextCursor];
      if (row !== undefined) {
        setPlaying(false);
        setIndex(row);
      }
    },
    [cursor, matchCount, matches],
  );

  const handleQueryChange = useCallback(
    (value: string) => {
      setQuery(value);
      setMatchCursor(0);
      // 输入即跳到第一个命中（契约 8.7 ④「可直接跳转到第 k 个命中」）。
      const hits = searchSteps(visibleSteps, value);
      const first = hits[0];
      if (first !== undefined) {
        setPlaying(false);
        setIndex(first);
      }
    },
    [visibleSteps],
  );

  const clearFilter = useCallback(() => setFilter(EMPTY_REPLAY_FILTER), []);

  const clearSearch = useCallback(() => {
    setQuery('');
    setMatchCursor(0);
  }, []);

  // 快捷键（契约 8.7 ⑥）：焦点不在输入框/文本域/contentEditable 时生效。
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.metaKey || event.ctrlKey || event.altKey) {
        return;
      }
      const action = hotkeyAction(event.key, {
        inInput:
          isEditableTarget(event.target) ||
          // 空格落在按钮/链接上时让位给原生激活（见 isActivationTarget）。
          (event.key === ' ' && isActivationTarget(event.target)),
        shift: event.shiftKey,
        hasSearch: query.trim() !== '',
        hasFilter: !isFilterEmpty(filter),
      });
      if (action === null) {
        return;
      }
      event.preventDefault();
      const handlers: Record<ReplayHotkeyAction, () => void> = {
        'toggle-play': handlePlay,
        'next-step': () => handleStep(1),
        'prev-step': () => handleStep(-1),
        'next-match': () => jumpToMatch(1),
        'prev-match': () => jumpToMatch(-1),
        'first-step': handleFirst,
        'last-step': handleLast,
        'focus-search': () => searchRef.current?.focus(),
        'focus-filter': () => kindFilterRef.current?.focus(),
        clear: () => {
          clearSearch();
          clearFilter();
        },
      };
      handlers[action]();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [
    clearFilter,
    clearSearch,
    filter,
    handleFirst,
    handleLast,
    handlePlay,
    handleStep,
    jumpToMatch,
    query,
  ]);

  const errorInfo = replayQuery.error ? toApiErrorInfo(replayQuery.error) : null;
  const currentStep = visibleSteps[safeIndex];
  const truncated = replayQuery.data?.truncated ?? false;
  // 截断文案四选一（契约 8.7 逐字写死；`''` 不显示横幅）。
  const banner = truncated
    ? truncatedBanner(replayQuery.data?.truncated_reason ?? '', rawTotal)
    : null;

  const filterActive = !isFilterEmpty(filter);
  const counterLabel = total > 0 ? `第 ${safeIndex + 1} / ${total} 步` : '0 步';

  const handleJump = () => {
    const target = parseStepJump(jumpText, total);
    if (target === null) {
      return;
    }
    setPlaying(false);
    setIndex(target);
    setJumpText('');
  };

  if (!enabled) {
    return (
      <div className="opsmind-replay-player opsmind-replay-player--center">
        <EmptyState description="从左侧选择一个会话查看执行回放" />
      </div>
    );
  }

  return (
    <div className="opsmind-replay-player">
      {banner !== null ? (
        <Alert className="replay-banner" type="warning" showIcon message={banner} />
      ) : null}

      <div className="replay-toolbar">
        <Space size={8} wrap>
          <Input
            ref={searchRef}
            allowClear
            value={query}
            onChange={(event) => handleQueryChange(event.target.value)}
            onPressEnter={() => jumpToMatch(1)}
            placeholder="搜索步骤（内容 / 命令 / 输出 / 摘要 / 参数）"
            aria-label="搜索步骤"
            prefix={<span className="replay-hotkey-hint">/</span>}
            style={{ width: 300 }}
          />
          {/* 会话 ID（契约 8.7 ⑩ 复制覆盖）：完整值可复制，显示用等宽小字。 */}
          <Space size={4} className="replay-conversation">
            <Typography.Text type="secondary" className="replay-inline-label">
              会话
            </Typography.Text>
            <span className="replay-conversation-id" title={conversationId}>
              {conversationId}
            </span>
            <CopyButton text={conversationId ?? ''} label="复制会话 ID" />
          </Space>
          {matchCount > 0 ? (
            <Space size={4} className="replay-match-nav">
              <span className="replay-match-label">
                {matchLabel(cursor, matchCount)}
                {cursor === 0 && matchCount > 1 ? '（回车到下一个）' : ''}
              </span>
              <Button size="small" onClick={() => jumpToMatch(-1)} aria-label="上一个命中">
                上一个命中
              </Button>
              <Button size="small" onClick={() => jumpToMatch(1)} aria-label="下一个命中">
                下一个命中
              </Button>
            </Space>
          ) : query.trim() !== '' ? (
            <Typography.Text type="secondary">没有匹配的步骤</Typography.Text>
          ) : null}
        </Space>

        <Space size={8} wrap>
          <Input
            value={jumpText}
            onChange={(event) => setJumpText(event.target.value)}
            onPressEnter={handleJump}
            placeholder="第 N 步"
            aria-label="跳转到步骤"
            disabled={total === 0}
            style={{ width: 108 }}
          />
          <Select
            ref={kindFilterRef}
            mode="multiple"
            allowClear
            value={[...filter.kinds]}
            onChange={(values: ReplayStep['kind'][]) => {
              setFilter((current) => ({ ...current, kinds: values }));
              setPlaying(false);
            }}
            placeholder="步骤类型"
            aria-label="按步骤类型筛选"
            options={REPLAY_KINDS.map((kind) => ({
              value: kind,
              label: REPLAY_KIND_LABELS[kind],
            }))}
            style={{ minWidth: 140 }}
            maxTagCount="responsive"
            // 选项数固定（≤18），关掉 antd 的下拉虚拟列表：既让全部选项常驻
            // DOM（键盘与读屏可达），也避免短列表滚动的空窗。
            virtual={false}
          />
          <Select
            mode="multiple"
            allowClear
            value={[...filter.tools]}
            onChange={(values: string[]) => {
              setFilter((current) => ({ ...current, tools: values }));
              setPlaying(false);
            }}
            placeholder="工具"
            aria-label="按工具筛选"
            options={TOOL_FILTER_OPTIONS}
            style={{ minWidth: 140 }}
            maxTagCount="responsive"
            // 选项数固定（≤18），关掉 antd 的下拉虚拟列表：既让全部选项常驻
            // DOM（键盘与读屏可达），也避免短列表滚动的空窗。
            virtual={false}
          />
          <Select
            mode="multiple"
            allowClear
            value={[...filter.risks]}
            onChange={(values: ReplayRiskFilter[]) => {
              setFilter((current) => ({ ...current, risks: values }));
              setPlaying(false);
            }}
            placeholder="风险级别"
            aria-label="按风险级别筛选"
            options={REPLAY_RISK_LEVELS.map((level) => ({
              value: level,
              label: REPLAY_RISK_LABELS[level],
            }))}
            style={{ minWidth: 130 }}
            maxTagCount="responsive"
            virtual={false}
          />
          {filterActive ? (
            <>
              <Tag color="blue" className="replay-filter-label">
                {mergeFilterLabel(
                  total,
                  rawTotal,
                  matchCount > 0 ? { current: cursor, count: matchCount } : undefined,
                )}
              </Tag>
              <Button size="small" onClick={clearFilter}>
                清除筛选
              </Button>
            </>
          ) : null}
        </Space>
      </div>

      {replayQuery.isPending ? (
        <div className="replay-state">
          <Spin />
        </div>
      ) : errorInfo ? (
        <div className="replay-state replay-state--error">
          <ErrorBanner
            code={errorInfo.code}
            message={describeError(errorInfo.code, errorInfo.message)}
            requestId={errorInfo.requestId}
            onRetry={() => void replayQuery.refetch()}
          />
        </div>
      ) : total === 0 ? (
        <div className="replay-state">
          {rawTotal === 0 ? (
            <EmptyState description="该会话暂无可回放的步骤" />
          ) : (
            <EmptyState description="没有符合筛选条件的步骤">
              <Button size="small" onClick={clearFilter}>
                清除筛选
              </Button>
            </EmptyState>
          )}
        </div>
      ) : (
        <div ref={scrollRef} className="replay-steps" data-testid="replay-steps">
          <div className="replay-steps-inner" style={{ height: virtualizer.getTotalSize() }}>
            {virtualizer.getVirtualItems().map((row) => {
              const step = visibleSteps[row.index];
              if (!step) {
                return null;
              }
              return (
                <div
                  key={row.key}
                  data-index={row.index}
                  ref={virtualizer.measureElement}
                  className="replay-virtual-row"
                  style={{ transform: `translateY(${row.start}px)` }}
                >
                  <StepCard
                    step={step}
                    active={row.index === safeIndex}
                    hasApproval={
                      typeof step.tool_call_id === 'string' && approvedIds.has(step.tool_call_id)
                    }
                    query={query}
                  />
                </div>
              );
            })}
          </div>
        </div>
      )}

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
          <span className="replay-counter">{counterLabel}</span>
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
          tooltip={{ formatter: () => (total > 0 ? counterLabel : '') }}
          onChange={handleProgress}
          aria-label="回放进度"
        />
      </div>
    </div>
  );
}
