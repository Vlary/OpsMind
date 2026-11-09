import hljs from 'highlight.js/lib/core';
import type { LanguageFn } from 'highlight.js';
import type { ReactNode } from 'react';

// CodeBlock 的语言加载与高亮渲染逻辑，独立成模块：
// 1) 契约 8.6.5 的两条降级路径（语言 chunk 加载失败 → 纯文本、高亮
//    解析失败 → 纯文本）可被单测直接覆盖；
// 2) 组件文件（CodeBlock.tsx）保持仅导出组件，满足 react-refresh 规则。

const SUPPORTED: Record<string, () => Promise<{ default: LanguageFn }>> = {
  bash: () => import('highlight.js/lib/languages/bash'),
  json: () => import('highlight.js/lib/languages/json'),
  sql: () => import('highlight.js/lib/languages/sql'),
  go: () => import('highlight.js/lib/languages/go'),
  typescript: () => import('highlight.js/lib/languages/typescript'),
  yaml: () => import('highlight.js/lib/languages/yaml'),
  nginx: () => import('highlight.js/lib/languages/nginx'),
};

const ALIASES: Record<string, string> = {
  sh: 'bash',
  shell: 'bash',
  zsh: 'bash',
  console: 'bash',
  text: '',
  plaintext: '',
  txt: '',
  ts: 'typescript',
  yml: 'yaml',
};

const languageCache = new Map<string, Promise<string | null>>();

function normalizeLanguage(language: string | undefined): string | null {
  const raw = (language ?? '').trim().toLowerCase();
  if (!raw) {
    return null;
  }
  const key = ALIASES[raw] ?? raw;
  return key && key in SUPPORTED ? key : null;
}

/** 加载受支持语言；未知语言或动态 import 失败一律降级返回 null（纯文本）。 */
export function loadLanguage(language: string | undefined): Promise<string | null> {
  const normalized = normalizeLanguage(language);
  if (!normalized) {
    return Promise.resolve(null);
  }
  if (hljs.getLanguage(normalized)) {
    return Promise.resolve(normalized);
  }
  const cached = languageCache.get(normalized);
  if (cached) {
    return cached;
  }
  const loader = SUPPORTED[normalized];
  if (!loader) {
    return Promise.resolve(null);
  }
  const promise = loader()
    .then((module) => {
      if (!hljs.getLanguage(normalized)) {
        hljs.registerLanguage(normalized, module.default);
      }
      return normalized;
    })
    .catch(() => null);
  languageCache.set(normalized, promise);
  return promise;
}

/**
 * 把 highlight.js 的 HTML 输出解析成 React 节点。
 * 这样既保留高亮，又不需要 dangerouslySetInnerHTML。
 * 解析失败（结构不完整）返回 null，调用方回退纯文本。
 */
export function renderHighlight(html: string): ReactNode[] | null {
  const root: ReactNode[] = [];
  const stack: { className: string; children: ReactNode[] }[] = [];
  let text = '';
  let key = 0;

  const currentChildren = (): ReactNode[] => {
    const top = stack[stack.length - 1];
    return top ? top.children : root;
  };

  const flushText = (): void => {
    if (!text) {
      return;
    }
    currentChildren().push(decodeEntities(text));
    text = '';
  };

  const pattern = /<span class="([^"]*)">|<\/span>|([^<]+)/g;
  let match: RegExpExecArray | null;
  let matchedLength = 0;
  while ((match = pattern.exec(html)) !== null) {
    matchedLength += match[0].length;
    if (match[1] !== undefined) {
      flushText();
      stack.push({ className: match[1], children: [] });
    } else if (match[0] === '</span>') {
      flushText();
      const frame = stack.pop();
      if (frame) {
        key += 1;
        const element = (
          <span key={`h${key}`} className={frame.className}>
            {frame.children}
          </span>
        );
        currentChildren().push(element);
      }
    } else if (match[2] !== undefined) {
      text += match[2];
    }
  }
  flushText();

  if (matchedLength !== html.length || stack.length > 0) {
    return null;
  }
  return root;
}

function decodeEntities(value: string): string {
  return value.replace(/&(#x?[0-9a-fA-F]+|amp|lt|gt|quot|#39);/g, (whole, entity: string) => {
    switch (entity) {
      case 'amp':
        return '&';
      case 'lt':
        return '<';
      case 'gt':
        return '>';
      case 'quot':
        return '"';
      case '#39':
        return "'";
      default: {
        if (entity.startsWith('#x') || entity.startsWith('#X')) {
          const code = Number.parseInt(entity.slice(2), 16);
          return Number.isFinite(code) ? String.fromCodePoint(code) : whole;
        }
        if (entity.startsWith('#')) {
          const code = Number.parseInt(entity.slice(1), 10);
          return Number.isFinite(code) ? String.fromCodePoint(code) : whole;
        }
        return whole;
      }
    }
  });
}
