// 深度思考块的纯逻辑（六期 FE-P1，契约 8.6 第 13 条 / 8.7 清单⑪）。
// 与 ReasoningBlock 组件分离（react-refresh：组件文件只导出组件），
// 这里承载摘要文案、耗时换算、streaming 边沿折叠与复制入口判定，供组件
// 与单测共用（reasoningBlock.test.ts）。

/** 摘要行的耗时秒数（四舍五入到整秒；<500ms 显示 0s——计时只有提示价值）。 */
export function reasoningDurationSeconds(durationMs: number | null | undefined): number | null {
  if (typeof durationMs !== 'number' || !Number.isFinite(durationMs) || durationMs < 0) {
    return null;
  }
  return Math.round(durationMs / 1000);
}

/**
 * 摘要行文案：streaming → 「思考中…」；有计时 → 「深度思考 · 耗时 Ns」；
 * 无计时数据（历史旧行无 reasoning_duration_ms、无 reasoning）→
 * 「深度思考 · 耗时 —」（契约 8.6 第 13 条，打磨 sprint 2：不静默省略耗时）。
 */
export function reasoningSummaryLabel(options: {
  streaming: boolean;
  durationMs?: number | null | undefined;
}): string {
  if (options.streaming) {
    return '思考中…';
  }
  const seconds = reasoningDurationSeconds(options.durationMs);
  return seconds === null ? '深度思考 · 耗时 —' : `深度思考 · 耗时 ${seconds}s`;
}

/**
 * streaming 边沿驱动的展开态（纯函数）：
 * - streaming=true（首个 reasoning delta）：强制展开（覆盖用户手动折叠）；
 * - streaming=false（首个正文 token / 流结束）：回默认收起；
 * - streaming=undefined（历史消息，无流式语义）：不动（保留用户手动展开）。
 */
export function applyStreamingToExpanded(
  expanded: boolean,
  streaming: boolean | undefined,
): boolean {
  if (streaming === undefined) {
    return expanded;
  }
  return streaming;
}

/**
 * 复制目标：仅展开态提供复制入口，复制内容恒为完整 reasoning 全文（不因
 * 折叠截断）。折叠态返回 null——不渲染复制按钮。
 */
export function reasoningCopyTarget(reasoning: string, expanded: boolean): string | null {
  return expanded && reasoning !== '' ? reasoning : null;
}
