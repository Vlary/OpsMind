import { lazy, Suspense } from 'react';

import type { MarkdownViewProps } from './MarkdownView';

// Markdown 渲染链（react-markdown + remark-gfm + highlight.js core）体积大，
// 仅聊天消息渲染需要：拆为动态 import，不进首屏与登录页。
const MarkdownView = lazy(() => import('./MarkdownView'));

// 该模块被加载即并行预取渲染器 chunk，避免首条消息出现二次等待。
void import('./MarkdownView');

function LazyMarkdownView({ content, streaming = false }: MarkdownViewProps) {
  return (
    // 占位给足预期高度（min-height + 脉冲，见 spinner.css）：零高 fallback
    // 会让流式首条消息在渲染器 chunk 就绪前正文空白，就绪后一次性撑开
    // 引起虚拟行高度突变。
    <Suspense fallback={<div className="opsmind-markdown markdown-loading" />}>
      <MarkdownView content={content} streaming={streaming} />
    </Suspense>
  );
}

export default LazyMarkdownView;
