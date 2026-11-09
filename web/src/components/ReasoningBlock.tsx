import { BulbOutlined, DownOutlined, LoadingOutlined, UpOutlined } from '@ant-design/icons';
import { Button, Typography } from 'antd';
import { useState } from 'react';

import { reasoningCopyTarget, reasoningSummaryLabel } from './reasoningView';
import CopyButton from './CopyButton';

// 深度思考块（六期 FE-P1，契约 8.6 第 13 条 / 8.2）：
// - 默认收起为摘要行「深度思考 · 耗时 Ns」（streaming 时「思考中…」并实时展开）；
// - 展开：首个 reasoning delta 到达即展开；首个正文 token delta 到达即回折叠
//   （streaming prop 的 true→false 边沿驱动，见 useChatStream/MessageList）；
// - 展开态右上角可复制全文；
// - 折叠不丢内容——收起仅 display:none，不卸载 DOM、不截断字符串（历史回看完整）；
// - reasoning 与正文同为 messages.reasoning 持久化字段，回放（FE-P2
//   ReplayDrawer）的 reasoning 步复用本组件渲染（named export 供其引用）。
// 摘要/折叠/复制的判定纯函数见 reasoningView.ts（react-refresh 拆分）。

export interface ReasoningBlockProps {
  reasoning: string;
  /** reasoning 仍在流入（本轮尚未收到正文 token）：自动展开并显示「思考中…」。 */
  streaming?: boolean | undefined;
  /** 本轮 reasoning 首/尾 delta 的耗时（ms）：流式中取 useChatStream 本地测量，
   *  历史消息取服务端落库的 messages.reasoning_duration_ms；无计时数据为 null。 */
  durationMs?: number | null;
  className?: string;
}

function ReasoningBlock({
  reasoning,
  streaming = false,
  durationMs,
  className,
}: ReasoningBlockProps) {
  const [expanded, setExpanded] = useState(streaming);
  // streaming 边沿驱动展开态（You Might Not Need an Effect 的「prop 变化时
  // 调整 state」官方模式——渲染期条件 setState，React 丢弃中断渲染并重试，
  // 不产生级联 effect）：true 到达（首个 reasoning delta）展开、false 到达
  // （首个正文 token / 流结束）回折叠。streaming 不变（含内容增长）不触发，
  // 用户手动展开/折叠得以保留。
  const [prevStreaming, setPrevStreaming] = useState(streaming);
  if (prevStreaming !== streaming) {
    setPrevStreaming(streaming);
    setExpanded(streaming);
  }

  const label = reasoningSummaryLabel({ streaming, durationMs });
  const copyTarget = reasoningCopyTarget(reasoning, expanded);

  if (reasoning === '') {
    return null;
  }

  return (
    <div className={`chat-reasoning-block${className ? ` ${className}` : ''}`}>
      <div className="chat-reasoning-block-header">
        <Button
          type="text"
          size="small"
          className="chat-reasoning-toggle"
          aria-expanded={expanded}
          onClick={() => setExpanded((value) => !value)}
        >
          {streaming ? <LoadingOutlined /> : <BulbOutlined />}
          <span>{label}</span>
          {expanded ? <UpOutlined /> : <DownOutlined />}
        </Button>
        {/* 展开态右上角复制全文（8.7 清单⑪：统一 CopyButton，三级降级
            含选中文本面板——http 非安全上下文长思考文本亦可复制）。 */}
        {copyTarget !== null ? (
          <CopyButton
            text={reasoning}
            label="复制思考全文"
            size="small"
            className="chat-reasoning-copy"
          />
        ) : null}
      </div>
      {/* 折叠不丢内容（契约 8.6 第 13 条）：display:none 收起，DOM 与字符串
          原样保留，展开即完整回看；虚拟列表卸载本组件时亦由字符串重建。 */}
      <div className="chat-reasoning-block-body" style={{ display: expanded ? 'block' : 'none' }}>
        <div className="chat-reasoning">
          <Typography.Text type="secondary">{reasoning}</Typography.Text>
        </div>
      </div>
    </div>
  );
}

export default ReasoningBlock;
// 具名导出与默认导出同体：FE-P2 回放组件按 named import 复用（任务约定导出）。
export { ReasoningBlock };
