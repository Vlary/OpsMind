import { memo, type ReactNode } from 'react';
import Markdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';

import CodeBlock from './CodeBlock';
import {
  MarkdownBlockCache,
  blockCacheKey,
  splitMarkdownBlocks,
  type MarkdownBlock,
} from '../features/chat/markdownBlocks';

export interface MarkdownViewProps {
  content: string;
  streaming?: boolean;
}

const blockCache = new MarkdownBlockCache<ReactNode>(2000);

/**
 * 安全渲染：不启用 rehype-raw，跳过原始 HTML，链接强制带 rel。
 * 代码块交给 CodeBlock，仅在其围栏闭合后高亮。
 */
const components: Components = {
  pre: ({ children }) => <>{children}</>,
  code: ({ className, children }) => {
    const text = String(children ?? '').replace(/\n$/, '');
    const language = /language-([\w-]+)/.exec(className ?? '')?.[1];
    const isBlock = Boolean(className?.includes('language-')) || text.includes('\n');
    if (!isBlock) {
      return <code className="opsmind-inline-code">{children}</code>;
    }
    return <CodeBlock code={text} closed {...(language ? { language } : {})} />;
  },
  a: ({ href, children }) => (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  ),
  table: ({ children }) => (
    <div className="opsmind-table-wrap">
      <table>{children}</table>
    </div>
  ),
};

function MarkdownBody({ block }: { block: MarkdownBlock }) {
  return (
    <Markdown remarkPlugins={[remarkGfm]} skipHtml components={components}>
      {block.content}
    </Markdown>
  );
}

function renderBlock(block: MarkdownBlock, streamingCursor: boolean): ReactNode {
  if (block.kind === 'streaming-code') {
    return (
      <div key={block.index} className="opsmind-md-block">
        <CodeBlock
          code={block.content}
          language={block.language}
          closed={false}
          cursor={streamingCursor}
        />
      </div>
    );
  }

  return (
    <div
      key={block.index}
      className={`opsmind-md-block${streamingCursor ? ' opsmind-stream-cursor' : ''}`}
    >
      <MarkdownBody block={block} />
    </div>
  );
}

function MarkdownView({ content, streaming = false }: MarkdownViewProps) {
  const blocks = splitMarkdownBlocks(content, streaming);

  return (
    <div className="opsmind-markdown">
      {blocks.map((block) => {
        const isLast = block.index === blocks.length - 1;
        const needsCursor = streaming && isLast;
        if (block.closed && !needsCursor) {
          return blockCache.get(blockCacheKey(block), () => renderBlock(block, false));
        }
        return renderBlock(block, needsCursor);
      })}
    </div>
  );
}

export default memo(MarkdownView);
