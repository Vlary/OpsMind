import hljs from 'highlight.js/lib/core';
import { useEffect, useMemo, useState, type ReactNode } from 'react';

import { loadLanguage, renderHighlight } from './codeBlockHighlight';
// 六期（FE-P2，契约 8.2）：代码块复制按钮改用全站统一 CopyButton。
import CopyButton from './CopyButton';

interface CodeBlockProps {
  code: string;
  language?: string;
  /** 只有闭合的代码块才高亮；未闭合时按纯文本渲染。 */
  closed?: boolean;
  /** 流式光标。 */
  cursor?: boolean;
}

export default function CodeBlock({
  code,
  language,
  closed = true,
  cursor = false,
}: CodeBlockProps) {
  const renderKey = `${language ?? ''}\u0000${code}`;
  const [highlighted, setHighlighted] = useState<{
    key: string;
    nodes: ReactNode[];
  } | null>(null);

  useEffect(() => {
    if (!closed) {
      return;
    }
    let cancelled = false;
    void loadLanguage(language).then((resolved) => {
      if (cancelled || !resolved) {
        return;
      }
      try {
        const html = hljs.highlight(code, { language: resolved, ignoreIllegals: true }).value;
        const nodes = renderHighlight(html);
        if (!cancelled && nodes) {
          setHighlighted({ key: renderKey, nodes });
        }
      } catch {
        // 高亮失败降级为纯文本。
      }
    });
    return () => {
      cancelled = true;
    };
  }, [code, language, closed, renderKey]);

  const highlightedNodes = highlighted && highlighted.key === renderKey ? highlighted.nodes : null;

  const label = useMemo(() => (language && language.length > 0 ? language : 'text'), [language]);

  return (
    <div className="opsmind-codeblock-wrap">
      <div className="opsmind-codeblock-header">
        <div className="opsmind-codeblock-label">{label}</div>
        {/* 保留 .opsmind-codeblock-copy 类：既有 e2e（polish.spec /
            polish-round4.spec）按该类定位代码块复制按钮。 */}
        <CopyButton text={code} label="复制代码" className="opsmind-codeblock-copy" />
      </div>
      <pre className="opsmind-codeblock" data-language={label}>
        <code className="hljs">
          {highlightedNodes ?? code}
          {cursor ? <span className="opsmind-cursor">▍</span> : null}
        </code>
      </pre>
    </div>
  );
}
