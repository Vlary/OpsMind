import { RedoOutlined, RobotOutlined, UserOutlined } from '@ant-design/icons';
import { Avatar, Button, Space, Tag, Typography } from 'antd';
import { memo, useMemo } from 'react';

import type { components } from '../../api/schema';
import CopyButton from '../../components/CopyButton';
import LazyMarkdownView from '../../components/LazyMarkdownView';
import ReasoningBlock from '../../components/ReasoningBlock';
import { stripContextPrefix } from './markdownBlocks';
import { describeError } from './useChatStream';

type Message = components['schemas']['Message'];

function formatTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return '';
  }
  return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
}

export interface MessageItemProps {
  message: Message;
  /** 最后一条持久化 error assistant 消息的 id；仅它显示「重新生成」。 */
  lastErrorAssistantId?: string | undefined;
  /** 最后一条悬挂 user 消息（本轮无任何 assistant 回复）的 id；为它显示「重新生成」。 */
  pendingRegenerateId?: string | undefined;
  /** 重新生成入口：以该轮 user 原文重新发起一轮 send。 */
  onRegenerate?: ((messageId: string) => void) | undefined;
  /** 流式进行中禁用重新生成，避免并发回合。 */
  regenerateDisabled?: boolean | undefined;
}

function MessageItem({
  message,
  lastErrorAssistantId,
  pendingRegenerateId,
  onRegenerate,
  regenerateDisabled = false,
}: MessageItemProps) {
  const isUser = message.role === 'user';
  const content = useMemo(
    () => (isUser ? stripContextPrefix(message.content) : message.content),
    [isUser, message.content],
  );

  if (isUser) {
    // 悬挂轮（本轮 user 已落库但没有任何 assistant 回复：后端崩溃经代理
    // 等）为这条 user 消息提供重新生成，避免走查 A2 的「刷新后无恢复入口」。
    const canRegeneratePending = Boolean(onRegenerate) && message.id === pendingRegenerateId;
    return (
      <div className="chat-message chat-message--user" data-message-id={message.id}>
        <div className="chat-message-avatar">
          <Avatar size={28} icon={<UserOutlined />} />
        </div>
        <div className="chat-bubble chat-bubble--user">
          <div className="chat-user-text">{content}</div>
          <div className="chat-message-meta">
            <Typography.Text type="secondary">{formatTime(message.created_at)}</Typography.Text>
            {/* 六期打磨 sprint 2：复制正文走全站统一 CopyButton（图标 + 1.5s
                冷却 + 两级剪贴板降级 + 可选中文本面板），不再手写 copyText
                + 局部 toast（契约 8.2/8.7「CopyButton 是唯一实现」）。 */}
            {content ? (
              <CopyButton className="chat-copy-button" text={content} label="复制消息内容" />
            ) : null}
            {canRegeneratePending && onRegenerate ? (
              <Button
                className="chat-regenerate-button"
                size="small"
                icon={<RedoOutlined />}
                disabled={regenerateDisabled}
                onClick={() => onRegenerate(message.id)}
              >
                重新生成
              </Button>
            ) : null}
          </div>
        </div>
      </div>
    );
  }

  const failed = message.status === 'error';
  // 用户主动停止/页面离开导致流断开时，后端同样落库 error 消息，但错误码是
  // CLIENT_DISCONNECTED；这不是“生成失败”，标红会误导用户以为自己点停止
  // 触发了故障。与 live 路径（MessageList LiveEntry 的「已停止」Tag）对齐。
  const stopped = failed && message.error_code === 'CLIENT_DISCONNECTED';
  const canRegenerate = failed && Boolean(onRegenerate) && message.id === lastErrorAssistantId;
  const usage =
    message.prompt_tokens > 0 || message.completion_tokens > 0
      ? `${message.prompt_tokens} → ${message.completion_tokens} tokens`
      : '';

  return (
    <div className="chat-message chat-message--assistant" data-message-id={message.id}>
      <div className="chat-message-avatar">
        <Avatar size={28} icon={<RobotOutlined />} style={{ background: 'var(--color-accent)' }} />
      </div>
      <div className="chat-bubble chat-bubble--assistant">
        <div className="chat-message-meta">
          <Space size={8} wrap>
            <Typography.Text strong>OpsMind</Typography.Text>
            {message.model ? <Tag color="blue">{message.model}</Tag> : null}
            {failed ? stopped ? <Tag>已停止</Tag> : <Tag color="red">生成失败</Tag> : null}
            <Typography.Text type="secondary">{formatTime(message.created_at)}</Typography.Text>
            {usage ? <Typography.Text type="secondary">{usage}</Typography.Text> : null}
          </Space>
          {content ? (
            <CopyButton className="chat-copy-button" text={content} label="复制消息内容" />
          ) : null}
        </div>

        {/* 深度思考（六期 8.6 第 13 条 / 打磨 sprint 2）：耗时优先取服务端落库的
            reasoning_duration_ms（刷新/回看仍显示）；无计时数据时摘要「深度思考 · 耗时 —」。
            默认收起，展开可复制全文，折叠不丢内容。 */}
        {message.reasoning ? (
          <ReasoningBlock
            reasoning={message.reasoning}
            durationMs={message.reasoning_duration_ms}
          />
        ) : null}

        {content ? <LazyMarkdownView content={content} /> : null}

        {failed ? (
          <div className="chat-message-error">
            <Typography.Text type={stopped ? 'secondary' : 'danger'}>
              {describeError(message.error_code, '回答生成失败。')}
              {message.error_code ? `（${message.error_code}）` : ''}
            </Typography.Text>
            {canRegenerate && onRegenerate ? (
              <Button
                className="chat-regenerate-button"
                size="small"
                icon={<RedoOutlined />}
                disabled={regenerateDisabled}
                onClick={() => onRegenerate(message.id)}
              >
                重新生成
              </Button>
            ) : null}
          </div>
        ) : null}
      </div>
    </div>
  );
}

/** 已完成消息不可变：按 id 短路，内容/状态变化仅用于防御性刷新。 */
export default memo(MessageItem, (prev, next) => {
  const a = prev.message;
  const b = next.message;
  return (
    a.id === b.id &&
    a.content === b.content &&
    a.reasoning === b.reasoning &&
    a.status === b.status &&
    a.error_code === b.error_code &&
    prev.lastErrorAssistantId === next.lastErrorAssistantId &&
    prev.pendingRegenerateId === next.pendingRegenerateId &&
    prev.onRegenerate === next.onRegenerate &&
    prev.regenerateDisabled === next.regenerateDisabled
  );
});
