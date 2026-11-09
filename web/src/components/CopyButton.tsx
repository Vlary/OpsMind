import { CheckOutlined, CopyOutlined } from '@ant-design/icons';
import { Button, Input, Popover, Tooltip, Typography } from 'antd';
import type { InputRef } from 'antd';
import { useEffect, useRef, useState, type ReactNode } from 'react';

import { copyViaClipboard, copyViaExecCommand } from '../lib/clipboard';
import { message } from './antdMessage';
import { resolveCopyOutcome, selectableFallbackText, withinCooldown } from './copyButtonLogic';

// FE-P2（六期）：全站统一复制按钮——契约 8.2/8.7 复制清单的唯一实现，
// 禁止各页手写复制。
//
// 行为链：
// 1. 默认复制图标 + tooltip（label）；
// 2. 点击 → navigator.clipboard.writeText，失败（非安全上下文/权限拒绝）
//    降级 document.execCommand('copy')；
// 3. 成功：图标切 √、主题化 message（AntdAppRoot 桥接 App.useApp()）
//    「已复制」，1.5s 冷却内不可重复触发；
// 4. 两级全失败：toast 报错 + 弹出可选中文本面板（自动全选，用户手动
//    Ctrl+C 兜底）。

export interface CopyButtonProps {
  /**
   * 要写入剪贴板的完整文本。传函数时在**点击时**求值（终端「当前命令」
   * 这类随输入实时变化、又不能每键重渲染的值）。
   */
  text: string | (() => string);
  /** tooltip 与 aria-label 文案（如「复制主机」）。 */
  label?: string;
  /** 图标尺寸（跟随宿主单元格密度）。 */
  size?: 'small' | 'middle' | 'large';
  className?: string;
}

export default function CopyButton({
  text,
  label = '复制',
  size = 'small',
  className,
}: CopyButtonProps) {
  // copiedAt 非 null 表示处于成功反馈（√ 图标）+ 冷却窗口。
  const [copiedAt, setCopiedAt] = useState<number | null>(null);
  const [fallback, setFallback] = useState<string | null>(null);
  const fallbackInputRef = useRef<InputRef>(null);
  const resolveText = (): string => (typeof text === 'function' ? text() : text);
  const empty = typeof text === 'string' && text.length === 0;

  useEffect(() => {
    if (copiedAt === null) {
      return undefined;
    }
    const timer = window.setTimeout(() => setCopiedAt(null), 1500);
    return () => window.clearTimeout(timer);
  }, [copiedAt]);

  // 降级面板打开时自动聚焦并全选：用户只补一个 Ctrl+C。
  useEffect(() => {
    if (fallback !== null) {
      fallbackInputRef.current?.focus();
      fallbackInputRef.current?.input?.select();
    }
  }, [fallback]);

  const handleCopy = async (): Promise<void> => {
    if (withinCooldown(copiedAt, Date.now())) {
      return;
    }
    const value = resolveText();
    if (value.length === 0) {
      // 延迟求值形态（如终端当前行）点击时才发现为空：给出可行动提示。
      message.warning('没有可复制的内容');
      return;
    }
    const clipboardOk = await copyViaClipboard(value);
    const outcome = clipboardOk
      ? { ok: true as const }
      : resolveCopyOutcome(false, copyViaExecCommand(value));
    if (outcome.ok) {
      message.success('已复制');
      setCopiedAt(Date.now());
      return;
    }
    message.error('复制失败，请在弹出面板中选中文本手动复制');
    setFallback(selectableFallbackText(value));
  };

  const button = (
    <Button
      type="text"
      size={size}
      {...(className ? { className } : {})}
      aria-label={label}
      disabled={empty}
      icon={
        copiedAt !== null ? (
          <CheckOutlined style={{ color: 'var(--color-success)' }} />
        ) : (
          <CopyOutlined />
        )
      }
      onClick={() => void handleCopy()}
    />
  );

  const tooltipTitle: ReactNode = copiedAt !== null ? '已复制' : label;

  const content = (
    <Tooltip title={tooltipTitle}>
      <span className="opsmind-copy-button">{button}</span>
    </Tooltip>
  );

  if (fallback === null) {
    return content;
  }

  // 两级剪贴板都不可用：可选中文本降级面板（点击面板外关闭并复位）。
  return (
    <Popover
      open
      trigger="click"
      onOpenChange={(open) => {
        if (!open) {
          setFallback(null);
        }
      }}
      content={
        <div style={{ width: 320 }}>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
            剪贴板不可用，请选中文本后按 Ctrl+C / Cmd+C 手动复制：
          </Typography.Paragraph>
          <Input
            ref={fallbackInputRef}
            readOnly
            value={fallback}
            aria-label={`${label}（可选中复制）`}
          />
        </div>
      }
    >
      {content}
    </Popover>
  );
}
