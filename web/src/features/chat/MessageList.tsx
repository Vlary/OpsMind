import { RobotOutlined, UserOutlined } from '@ant-design/icons';
import { useVirtualizer } from '@tanstack/react-virtual';
import { Avatar, Button, Spin, Tag, Typography } from 'antd';
import { useCallback, useLayoutEffect, useMemo, useRef, useState } from 'react';

import EmptyState from '../../components/EmptyState';
import LazyMarkdownView from '../../components/LazyMarkdownView';
import ReasoningBlock from '../../components/ReasoningBlock';
import type { components } from '../../api/schema';
import MessageItem from './MessageItem';
import ToolCallCard from './ToolCallCard';
import { stripContextPrefix } from './markdownBlocks';
import {
  liveReasoningDurationMs,
  liveToolsFallbackTurnId,
  toLiveToolView,
  toToolCallView,
  type LiveTurn,
  type ToolCallView,
} from './useChatStream';

type Message = components['schemas']['Message'];
type ToolCall = components['schemas']['ToolCall'];
type Approval = components['schemas']['Approval'];

type Entry =
  | { key: string; kind: 'message'; message: Message }
  | {
      key: string;
      kind: 'tools';
      turnId: string;
      tools: ToolCallView[];
      /** 排序时间戳：持久化分组取 tools[0].started_at，回退组取所在消息。 */
      sortTime: number;
    }
  | { key: string; kind: 'live'; live: LiveTurn };

function parseTime(value: string | undefined): number {
  if (!value) {
    return 0;
  }
  const time = Date.parse(value);
  return Number.isNaN(time) ? 0 : time;
}

function entryTime(entry: Entry): number {
  if (entry.kind === 'message') {
    return parseTime(entry.message.created_at);
  }
  if (entry.kind === 'tools') {
    return entry.sortTime;
  }
  return Number.POSITIVE_INFINITY;
}

/**
 * 最后一条消息是 user（本轮没有任何落库的 assistant 回复）时返回其 id，
 * 为其提供「重新生成」入口。后端崩溃经反向代理等场景下 user 消息已落库、
 * 回复永远不会来（走查 A2：刷新后 user 消息悬挂且无任何恢复手段）。
 */
// export 仅作单测钩子（__tests__/pendingRegenerate.test.ts）：纯函数，无行为变化。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function pendingRegenerateUserId(messages: Message[]): string | undefined {
  const last = messages[messages.length - 1];
  return last !== undefined && last.role === 'user' ? last.id : undefined;
}

/**
 * 工具组审批快照合并（mergedApprovalsByTool 本体）：持久化列表打底，
 * live 同 id 覆盖——但落库终态（approved/denied/expired）不被 live 的
 * pending 压回（第 4 轮打磨）。
 *
 * 终态优先的动机：live 快照只在本地 SSE 帧到达时更新。多窗口场景下
 * 另一窗口停止回答，服务端已把审批取消为 expired（stream_cancelled），
 * 本窗口 SSE 已断、live 里仍是 pending；若允许 pending 覆盖落库终态，
 * 卡片会在 TTL 内死锁显示「待处理」，批准按钮可点但必返回 409。
 * live 终态（本窗口决策的乐观回填 / approval_resolved 帧）仍覆盖落库
 * 任意状态，保证决策后的即时反馈不被迟到的 refetch 旧值压回。
 */
// export 仅作单测钩子（__tests__/stopApprovalDeadlock.test.ts）：纯函数。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function mergeApprovalsByTool(
  persistedByTool: ReadonlyMap<string, Approval[]>,
  liveByTool: ReadonlyMap<string, Approval[]>,
): Map<string, Approval[]> {
  const map = new Map<string, Approval[]>(persistedByTool);
  for (const [toolId, list] of liveByTool) {
    const byId = new Map((map.get(toolId) ?? []).map((approval) => [approval.id, approval]));
    for (const approval of list) {
      const persisted = byId.get(approval.id);
      if (persisted && persisted.status !== 'pending' && approval.status === 'pending') {
        continue;
      }
      byId.set(approval.id, approval);
    }
    map.set(toolId, [...byId.values()]);
  }
  return map;
}

export interface MessageListProps {
  conversationId: string;
  messages: Message[];
  toolCalls: ToolCall[];
  approvals: Approval[];
  live: LiveTurn | null;
  liveVisible: boolean;
  streaming: boolean;
  loading: boolean;
  hasOlder: boolean;
  loadingOlder: boolean;
  onLoadOlder: () => void;
  onApprovalUpdated: (approval: Approval) => void;
  /** 历史失败回合的重新生成入口；messageId 是 error assistant 或悬挂 user 消息 id。 */
  onRegenerate?: (messageId: string) => void;
}

function LiveEntry({
  live,
  conversationId,
  streaming,
  liveApprovalsByTool,
  onApprovalUpdated,
}: {
  live: LiveTurn;
  conversationId: string;
  streaming: boolean;
  liveApprovalsByTool: Map<string, Approval[]>;
  onApprovalUpdated: (approval: Approval) => void;
}) {
  // 深度思考流式语义（六期 8.6 第 13 条）：reasoning 正在流入（本轮尚无
  // 正文 token）时 ReasoningBlock 强制展开显示「思考中…」；首个 token
  // delta 落地（live.text 非空）即回折叠。done/停止后 streaming=false 同样折叠。
  const reasoningStreaming = streaming && live.reasoning !== '' && live.text === '';
  const waiting = !live.text && live.tools.length === 0 && !live.error && !live.stopped;

  return (
    <>
      <div className="chat-message chat-message--user">
        <div className="chat-message-avatar">
          <Avatar size={28} icon={<UserOutlined />} />
        </div>
        <div className="chat-bubble chat-bubble--user">
          <div className="chat-user-text">{stripContextPrefix(live.userContent)}</div>
        </div>
      </div>

      <div className="chat-message chat-message--assistant">
        <div className="chat-message-avatar">
          <Avatar
            size={28}
            icon={<RobotOutlined />}
            style={{ background: 'var(--color-accent)' }}
          />
        </div>
        <div className="chat-bubble chat-bubble--assistant">
          <div className="chat-message-meta">
            <Typography.Text strong>OpsMind</Typography.Text>
            {live.stopped ? <Tag>已停止</Tag> : null}
            {streaming ? <Typography.Text type="secondary">正在生成…</Typography.Text> : null}
          </div>

          {live.reasoning ? (
            <ReasoningBlock
              reasoning={live.reasoning}
              streaming={reasoningStreaming}
              durationMs={liveReasoningDurationMs(live)}
            />
          ) : null}

          {waiting ? (
            <div className="chat-waiting">
              <Spin size="small" /> <Typography.Text type="secondary">正在思考…</Typography.Text>
            </div>
          ) : null}

          {/* 与持久化视图一致：同一 turn 的工具卡片在助手正文之前，
              done 后内容不会因重排而跳动。 */}
          {live.tools.map((tool) => {
            const view: ToolCallView = toLiveToolView(tool);
            return (
              <ToolCallCard
                key={view.id}
                toolCall={view}
                approvals={liveApprovalsByTool.get(view.id) ?? []}
                conversationId={conversationId}
                onApprovalUpdated={onApprovalUpdated}
              />
            );
          })}

          {live.text ? <LazyMarkdownView content={live.text} streaming={streaming} /> : null}
        </div>
      </div>
    </>
  );
}

export default function MessageList({
  conversationId,
  messages,
  toolCalls,
  approvals,
  live,
  liveVisible,
  streaming,
  loading,
  hasOlder,
  loadingOlder,
  onLoadOlder,
  onApprovalUpdated,
  onRegenerate,
}: MessageListProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const atBottomRef = useRef(true);
  const anchorRef = useRef<string | null>(null);
  const [showJump, setShowJump] = useState(false);

  const entries = useMemo<Entry[]>(() => {
    const toolsByTurn = new Map<string, ToolCall[]>();
    for (const tool of toolCalls) {
      const list = toolsByTurn.get(tool.turn_id);
      if (list) {
        list.push(tool);
      } else {
        toolsByTurn.set(tool.turn_id, [tool]);
      }
    }

    // done 轮工具卡回退：live 条目已按 messages 落地撤除，但该 turn 的
    // toolCalls 分组尚未落地（done 的 messages/toolCalls invalidate 是
    // 并发 refetch，存在落地间隙）时，用仍在内存中的 live.tools 补渲染，
    // 卡片不消失；分组落地后回退自动失效（见 useChatStream.ts 注释）。
    const fallbackTurnId = liveToolsFallbackTurnId(live, messages, new Set(toolsByTurn.keys()));

    const emitted = new Set<string>();
    const result: Entry[] = [];
    for (const message of messages) {
      if (message.role === 'user') {
        result.push({ key: `m:${message.id}`, kind: 'message', message });
        continue;
      }
      if (!emitted.has(message.turn_id)) {
        const persisted = toolsByTurn.get(message.turn_id);
        const fallback =
          !persisted && message.turn_id === fallbackTurnId
            ? (live?.tools ?? []).map(toLiveToolView)
            : undefined;
        const tools = persisted?.map(toToolCallView) ?? fallback;
        if (tools) {
          result.push({
            key: `t:${message.turn_id}`,
            kind: 'tools',
            turnId: message.turn_id,
            tools,
            // 回退组挂在该消息位置（toolCalls 未落地，无 started_at 可用）。
            sortTime: persisted
              ? parseTime(persisted[0]?.started_at)
              : parseTime(message.created_at),
          });
          emitted.add(message.turn_id);
        }
      }
      result.push({ key: `m:${message.id}`, kind: 'message', message });
    }

    for (const [turnId, tools] of toolsByTurn) {
      if (!emitted.has(turnId)) {
        result.push({
          key: `t:${turnId}`,
          kind: 'tools',
          turnId,
          tools: tools.map(toToolCallView),
          sortTime: parseTime(tools[0]?.started_at),
        });
      }
    }

    if (liveVisible && live) {
      result.push({ key: 'live', kind: 'live', live });
    }
    // 未加载到 assistant 消息的 turn（工具调用可能来自更早的分页），
    // 按发生时间插入正确位置，而不是一律追加到列表末尾。
    result.sort((a, b) => entryTime(a) - entryTime(b));
    return result;
  }, [messages, toolCalls, live, liveVisible]);

  const approvalsByTool = useMemo(() => {
    const map = new Map<string, Approval[]>();
    for (const approval of approvals) {
      // 四期：source=console 的审批 tool_call_id 为 null（不挂在任何
      // chat 工具卡上，其决策入口在容器/控制台页），跳过索引。
      if (approval.tool_call_id === null) {
        continue;
      }
      const list = map.get(approval.tool_call_id);
      if (list) {
        list.push(approval);
      } else {
        map.set(approval.tool_call_id, [approval]);
      }
    }
    return map;
  }, [approvals]);

  // live 轮的审批索引（SSE 实时）：live 条目与 done 轮回退组共用。
  const liveApprovalsByTool = useMemo(() => {
    const map = new Map<string, Approval[]>();
    for (const approval of live?.approvals ?? []) {
      // 同上：live 轮不会出现 console 审批，防御性跳过 null。
      if (approval.tool_call_id === null) {
        continue;
      }
      const list = map.get(approval.tool_call_id);
      if (list) {
        list.push(approval);
      } else {
        map.set(approval.tool_call_id, [approval]);
      }
    }
    return map;
  }, [live]);

  // 工具组的最终审批来源：持久化列表打底，live 同 id 覆盖（回退窗口内
  // approvals refetch 可能同样未落地，live 的 SSE 状态更新）。
  const mergedApprovalsByTool = useMemo(
    () => mergeApprovalsByTool(approvalsByTool, liveApprovalsByTool),
    [approvalsByTool, liveApprovalsByTool],
  );

  // 仅当最后一条 assistant 消息是 error（历史失败回合）时提供重新生成。
  const lastErrorAssistantId = useMemo(() => {
    for (let index = messages.length - 1; index >= 0; index -= 1) {
      const message = messages[index];
      if (!message || message.role !== 'assistant') {
        continue;
      }
      return message.status === 'error' ? message.id : undefined;
    }
    return undefined;
  }, [messages]);

  // 最后一条是 user（无 assistant 回复）时同样提供重新生成（悬挂轮恢复入口）。
  const pendingRegenerateId = useMemo(() => pendingRegenerateUserId(messages), [messages]);

  // eslint-disable-next-line react-hooks/incompatible-library -- TanStack Virtual 官方说明需跳过 React Compiler memo 化
  const virtualizer = useVirtualizer({
    count: entries.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 160,
    overscan: 8,
    getItemKey: (index) => entries[index]?.key ?? index,
  });

  const handleScroll = useCallback(() => {
    const element = scrollRef.current;
    if (!element) {
      return;
    }
    const distance = element.scrollHeight - element.scrollTop - element.clientHeight;
    const atBottom = distance < 80;
    atBottomRef.current = atBottom;
    setShowJump(!atBottom);
  }, []);

  const liveSignature = live
    ? `${live.text.length}:${live.reasoning.length}:${live.tools.length}:${live.approvals
        .map((item) => item.status)
        .join(',')}`
    : '';

  // 流式内容增长时，只有用户位于底部附近才跟随滚动。
  useLayoutEffect(() => {
    if (!atBottomRef.current || entries.length === 0) {
      return;
    }
    const frame = requestAnimationFrame(() => {
      if (atBottomRef.current) {
        virtualizer.scrollToIndex(entries.length - 1, { align: 'end' });
      }
    });
    return () => cancelAnimationFrame(frame);
  }, [entries.length, liveSignature, virtualizer]);

  // 切换会话或首次加载：滚到底部。
  const initialScrollRef = useRef<string>('');
  useLayoutEffect(() => {
    if (loading || entries.length === 0 || !conversationId) {
      return;
    }
    if (initialScrollRef.current === conversationId) {
      return;
    }
    initialScrollRef.current = conversationId;
    atBottomRef.current = true;
    setShowJump(false);
    virtualizer.scrollToIndex(entries.length - 1, { align: 'end' });
  }, [conversationId, loading, entries.length, virtualizer]);

  // 加载更早消息后把锚点保持在原位置。
  useLayoutEffect(() => {
    const anchor = anchorRef.current;
    if (!anchor) {
      return;
    }
    const index = entries.findIndex((entry) => entry.key === anchor);
    if (index >= 0) {
      anchorRef.current = null;
      virtualizer.scrollToIndex(index, { align: 'start' });
    }
  }, [entries, virtualizer]);

  const handleLoadOlder = useCallback(() => {
    anchorRef.current = entries[0]?.key ?? null;
    onLoadOlder();
  }, [entries, onLoadOlder]);

  if (loading) {
    return (
      <div className="chat-messages chat-messages--center">
        <Spin />
      </div>
    );
  }

  if (entries.length === 0) {
    return (
      <div className="chat-messages chat-messages--center">
        <EmptyState description="还没有消息，发送第一条指令开始对话" />
      </div>
    );
  }

  return (
    <div className="chat-messages" ref={scrollRef} onScroll={handleScroll}>
      {hasOlder ? (
        <div className="chat-load-older">
          <Button size="small" loading={loadingOlder} onClick={handleLoadOlder}>
            加载更早消息
          </Button>
        </div>
      ) : null}

      <div className="chat-messages-inner" style={{ height: virtualizer.getTotalSize() }}>
        {virtualizer.getVirtualItems().map((virtualItem) => {
          const entry = entries[virtualItem.index];
          if (!entry) {
            return null;
          }
          return (
            <div
              key={virtualItem.key}
              data-index={virtualItem.index}
              ref={virtualizer.measureElement}
              className="chat-virtual-row"
              style={{ transform: `translateY(${virtualItem.start}px)` }}
            >
              {entry.kind === 'message' ? (
                <MessageItem
                  message={entry.message}
                  lastErrorAssistantId={lastErrorAssistantId}
                  pendingRegenerateId={pendingRegenerateId}
                  onRegenerate={onRegenerate}
                  regenerateDisabled={streaming || liveVisible}
                />
              ) : entry.kind === 'tools' ? (
                <div className="chat-toolgroup">
                  {entry.tools.map((tool) => (
                    <ToolCallCard
                      key={tool.id}
                      toolCall={tool}
                      approvals={mergedApprovalsByTool.get(tool.id) ?? []}
                      conversationId={conversationId}
                      onApprovalUpdated={onApprovalUpdated}
                    />
                  ))}
                </div>
              ) : (
                <LiveEntry
                  live={entry.live}
                  conversationId={conversationId}
                  streaming={streaming}
                  liveApprovalsByTool={liveApprovalsByTool}
                  onApprovalUpdated={onApprovalUpdated}
                />
              )}
            </div>
          );
        })}
      </div>

      {showJump ? (
        <Button
          className="chat-jump-bottom"
          onClick={() => virtualizer.scrollToIndex(entries.length - 1, { align: 'end' })}
        >
          回到底部
        </Button>
      ) : null}
    </div>
  );
}
