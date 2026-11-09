import { describe, expect, it } from 'vitest';

import {
  MarkdownBlockCache,
  blockCacheKey,
  isHighlightable,
  splitMarkdownBlocks,
  stripContextPrefix,
} from '../markdownBlocks';

describe('splitMarkdownBlocks', () => {
  it('按空行切块', () => {
    const blocks = splitMarkdownBlocks('第一段\n\n第二段\n\n- a\n- b', false);
    expect(blocks.map((block) => block.kind)).toEqual(['markdown', 'markdown', 'markdown']);
    expect(blocks[0]?.content).toBe('第一段');
    expect(blocks[2]?.content).toBe('- a\n- b');
  });

  it('闭合围栏是 code 块并带语言', () => {
    const blocks = splitMarkdownBlocks('```bash\nls -la\n```', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('code');
    expect(blocks[0]?.language).toBe('bash');
    expect(blocks[0]?.closed).toBe(true);
    expect(isHighlightable(blocks[0]!)).toBe(true);
  });

  it('未闭合围栏按纯文本处理且不可高亮', () => {
    const blocks = splitMarkdownBlocks('说明\n\n```bash\nls -la', true);
    expect(blocks[1]?.kind).toBe('streaming-code');
    expect(blocks[1]?.content).toBe('ls -la');
    expect(blocks[1]?.closed).toBe(false);
    expect(isHighlightable(blocks[1]!)).toBe(false);
  });

  it('流式中的最后一个普通块不进入缓存', () => {
    const blocks = splitMarkdownBlocks('正在输出的段落', true);
    expect(blocks[0]?.closed).toBe(false);
  });

  it('块 hash 随内容变化', () => {
    const a = splitMarkdownBlocks('alpha', false)[0];
    const b = splitMarkdownBlocks('beta', false)[0];
    expect(a?.hash).not.toBe(b?.hash);
  });
});

describe('MarkdownBlockCache', () => {
  it('同一 index + hash 只创建一次（闭合块缓存命中）', () => {
    const cache = new MarkdownBlockCache<number>();
    const block = splitMarkdownBlocks('```json\n{"a":1}\n```', false)[0]!;
    const key = blockCacheKey(block);

    let created = 0;
    const first = cache.get(key, () => {
      created += 1;
      return created;
    });
    const second = cache.get(key, () => {
      created += 1;
      return created;
    });

    expect(first).toBe(1);
    expect(second).toBe(1);
    expect(created).toBe(1);
  });
});

describe('stripContextPrefix', () => {
  it('仅去掉开头的上下文标记', () => {
    const content = '<!--opsmind-context\nservers: web-1 (id=abc)\n-->\n检查磁盘\n<!-- 保留 -->';
    expect(stripContextPrefix(content)).toBe('检查磁盘\n<!-- 保留 -->');
  });

  it('普通文本不受影响', () => {
    expect(stripContextPrefix('普通消息')).toBe('普通消息');
  });
});
