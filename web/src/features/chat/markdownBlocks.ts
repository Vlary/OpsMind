/**
 * 增量 Markdown 分块。
 *
 * 规则（CONTRACT.md 8.6.4）：
 * - 按空行与围栏代码块切块；
 * - 已闭合块按 `index + hash` 缓存；
 * - 仅最后一个未闭合块在每次刷新时重新解析；
 * - 围栏未闭合时按纯文本渲染，不能高亮。
 */

export type MarkdownBlockKind = 'markdown' | 'code' | 'streaming-code';

export interface MarkdownBlock {
  index: number;
  kind: MarkdownBlockKind;
  /** 原始 Markdown（markdown/code 块）或围栏内部纯文本（streaming-code）。 */
  content: string;
  /** 代码块语言；非代码块为空字符串。 */
  language: string;
  /** true 表示内容已完整，可缓存、可高亮。 */
  closed: boolean;
  hash: string;
}

const FENCE_OPEN = /^ {0,3}(`{3,}|~{3,})(.*)$/;

function hashText(input: string): string {
  // FNV-1a 32 位，足够做块级缓存键。
  let hash = 0x811c9dc5;
  for (let i = 0; i < input.length; i += 1) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, '0');
}

function isFenceClose(line: string, fenceChar: string, fenceLength: number): boolean {
  const match = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(line);
  if (!match) {
    return false;
  }
  const marker = match[1] ?? '';
  return marker.startsWith(fenceChar) && marker.length >= fenceLength;
}

function languageOf(info: string): string {
  return info.trim().split(/\s+/)[0]?.toLowerCase() ?? '';
}

export function splitMarkdownBlocks(source: string, streaming = false): MarkdownBlock[] {
  if (!source) {
    return [];
  }

  const lines = source.split('\n');
  const blocks: Omit<MarkdownBlock, 'index'>[] = [];

  let current: string[] = [];
  let inFence = false;
  let fenceChar = '`';
  let fenceLength = 3;
  let fenceLanguage = '';

  const pushMarkdown = (): void => {
    if (current.length === 0) {
      return;
    }
    const content = current.join('\n').replace(/\s+$/, '');
    current = [];
    if (!content) {
      return;
    }
    blocks.push({
      kind: 'markdown',
      content,
      language: '',
      closed: true,
      hash: hashText(`markdown\n\n${content}`),
    });
  };

  for (const line of lines) {
    if (inFence) {
      current.push(line);
      if (isFenceClose(line, fenceChar, fenceLength)) {
        inFence = false;
        const content = current.join('\n');
        current = [];
        blocks.push({
          kind: 'code',
          content,
          language: fenceLanguage,
          closed: true,
          hash: hashText(`code\n${fenceLanguage}\n${content}`),
        });
      }
      continue;
    }

    const fence = FENCE_OPEN.exec(line);
    if (fence) {
      pushMarkdown();
      inFence = true;
      const marker = fence[1] ?? '```';
      fenceChar = marker[0] ?? '`';
      fenceLength = marker.length;
      fenceLanguage = languageOf(fence[2] ?? '');
      current = [line];
      continue;
    }

    if (line.trim() === '') {
      pushMarkdown();
      continue;
    }

    current.push(line);
  }

  if (inFence) {
    const body = current.slice(1).join('\n');
    blocks.push({
      kind: 'streaming-code',
      content: body,
      language: fenceLanguage,
      closed: false,
      hash: hashText(`streaming-code\n${fenceLanguage}\n${body}`),
    });
  } else {
    pushMarkdown();
  }

  if (!streaming && blocks.length > 0) {
    for (const block of blocks) {
      block.closed = true;
    }
  } else if (streaming) {
    const last = blocks[blocks.length - 1];
    // 代码围栏的 closed 只由是否闭合决定；普通 Markdown 最后一块在流式中仍可能增长。
    if (last && last.kind === 'markdown') {
      last.closed = false;
    }
  }

  return blocks.map((block, index) => ({ ...block, index }));
}

/** 只有已闭合的代码块才允许高亮。 */
export function isHighlightable(block: MarkdownBlock): boolean {
  return block.kind === 'code' && block.closed;
}

export function blockCacheKey(block: MarkdownBlock): string {
  return `${block.index}:${block.hash}`;
}

export class MarkdownBlockCache<T> {
  private readonly map = new Map<string, T>();
  private readonly maxEntries: number;

  constructor(maxEntries = 2000) {
    this.maxEntries = maxEntries;
  }

  get(key: string, create: () => T): T {
    const hit = this.map.get(key);
    if (hit !== undefined) {
      return hit;
    }
    const value = create();
    if (this.map.size >= this.maxEntries) {
      const oldest = this.map.keys().next();
      if (!oldest.done) {
        this.map.delete(oldest.value);
      }
    }
    this.map.set(key, value);
    return value;
  }

  has(key: string): boolean {
    return this.map.has(key);
  }

  clear(): void {
    this.map.clear();
  }
}

/** 去掉 ChatPage 附加在 user 消息前的上下文标记（仅用于展示）。 */
const CONTEXT_PREFIX = /^<!--\s*opsmind-context[\s\S]*?-->\s*/;

export function stripContextPrefix(content: string): string {
  return content.replace(CONTEXT_PREFIX, '');
}
