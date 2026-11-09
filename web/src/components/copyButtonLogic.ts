// FE-P2（六期）：CopyButton 的纯逻辑内核。
//
// 契约 8.2/8.7：全站统一复制按钮——复制图标（默认）+ 成功反馈（图标切换
// √ + 1.5s 内不可重复触发）；`navigator.clipboard.writeText` 失败（非安全
// 上下文/权限拒绝）降级 `document.execCommand('copy')`；两级都不可用时
// 降级为「可选中文本」面板（用户手动 Ctrl+C），并 toast 报错。
// 本模块只放可在 node 环境单测的纯函数，DOM 副作用在 CopyButton.tsx。

/** 成功反馈期间禁止重复触发（契约 8.2：1.5s）。 */
export const COPY_COOLDOWN_MS = 1500;

/** 复制降级链的三种终态。 */
export type CopyOutcome =
  | { ok: true }
  /** 两级剪贴板都不可用：UI 应展示可选中文本面板。 */
  | { ok: false; fallback: 'selectable' };

/**
 * 冷却判定：now 与上次成功复制时刻（copiedSince）相差不足 1.5s 时
 * 忽略新的点击（成功反馈期间按钮防抖）。
 */
export function withinCooldown(copiedAt: number | null, now: number): boolean {
  if (copiedAt === null) {
    return false;
  }
  return now - copiedAt < COPY_COOLDOWN_MS;
}

/**
 * 由两级尝试结果推导终态：
 * - clipboard API 成功 → 成功；
 * - execCommand 降级成功 → 同样视为成功（用户拿到的是同一份复制）；
 * - 两级全失败 → 可选中文本降级。
 */
export function resolveCopyOutcome(clipboardOk: boolean, execOk: boolean): CopyOutcome {
  return clipboardOk || execOk ? { ok: true } : { ok: false, fallback: 'selectable' };
}

/**
 * 降级面板的可选中文本：空文本无可复制，返回 null 表示不展示面板
 * （空字符串按钮在各页本来就不会渲染，这里再兜一层）。
 */
export function selectableFallbackText(text: string): string | null {
  return text.length > 0 ? text : null;
}
