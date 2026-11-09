import { describe, expect, it } from 'vitest';

import {
  MarkdownBlockCache,
  blockCacheKey,
  isHighlightable,
  splitMarkdownBlocks,
  stripContextPrefix,
} from '../markdownBlocks';

// 增量 Markdown 切块（CONTRACT.md 8.6.4）的流式语义与缓存行为，
// 与既有 markdownBlocks.test.ts 的基础切块互补。

describe('splitMarkdownBlocks 流式语义', () => {
  it('streaming=true 时最后一个 markdown 块 closed=false（仍可能增长）', () => {
    const blocks = splitMarkdownBlocks('第一段\n\n第二段正在生成', true);
    expect(blocks).toHaveLength(2);
    expect(blocks[0]!.closed).toBe(true);
    expect(blocks[1]!.closed).toBe(false);
    expect(blocks[1]!.content).toBe('第二段正在生成');
  });

  it('streaming=false 时即使缺尾空行也视为全部闭合（历史消息整段渲染）', () => {
    const blocks = splitMarkdownBlocks('结论文本未换行收尾', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.closed).toBe(true);
  });

  it('streaming=true 时已闭合的代码块 closed=true，可缓存可高亮', () => {
    const blocks = splitMarkdownBlocks('```bash\nls -l\n```\n\n仍在写的文字', true);
    expect(blocks[0]!.kind).toBe('code');
    expect(blocks[0]!.closed).toBe(true);
    expect(blocks[1]!.kind).toBe('markdown');
    expect(blocks[1]!.closed).toBe(false);
  });

  it('未闭合围栏产出 streaming-code：内容是围栏内部文本而非整行', () => {
    const blocks = splitMarkdownBlocks('前文\n\n```sql\nSELECT 1', true);
    expect(blocks[1]).toMatchObject({
      kind: 'streaming-code',
      language: 'sql',
      content: 'SELECT 1',
      closed: false,
    });
  });

  it('streaming=false 下未闭合围栏产出 streaming-code 且整体强制 closed（历史整段渲染）', () => {
    const blocks = splitMarkdownBlocks('```bash\nuptime', false);
    expect(blocks[0]!.kind).toBe('streaming-code');
    // 非流式（历史消息）不复解析：全部视为已完整。
    expect(blocks[0]!.closed).toBe(true);
  });
});

describe('splitMarkdownBlocks 围栏变体', () => {
  it('~~~ 波浪线围栏开合并按字符匹配', () => {
    const blocks = splitMarkdownBlocks('~~~text\n纯文本\n~~~\n');
    expect(blocks[0]).toMatchObject({ kind: 'code', language: 'text', closed: true });
  });

  it('更长的围栏（4 个反引号）不能被 3 个反引号提前闭合', () => {
    const source = '````markdown\n内含 ``` 代码\n````\n';
    const blocks = splitMarkdownBlocks(source);
    // 内部 ``` 不闭合外层围栏；外层 ```` 闭合。
    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.kind).toBe('code');
    expect(blocks[0]!.content).toContain('````markdown');
  });

  it('语言 info 取首词并转小写（含版本号等附加参数）', () => {
    const blocks = splitMarkdownBlocks('```Bash title="x"\necho hi\n```');
    expect(blocks[0]!.language).toBe('bash');
  });

  it('行首 3 个空格内的围栏仍识别（Markdown 规范允许）', () => {
    const blocks = splitMarkdownBlocks('   ```go\nfmt.Println(1)\n   ```');
    expect(blocks[0]!.kind).toBe('code');
    expect(blocks[0]!.language).toBe('go');
  });

  it('围栏闭合行允许尾随空白', () => {
    const blocks = splitMarkdownBlocks('```js\ncode\n```   \n');
    expect(blocks[0]!.kind).toBe('code');
    expect(blocks[0]!.closed).toBe(true);
  });

  it('空字符串与纯空白文本返回空数组（无空块）', () => {
    expect(splitMarkdownBlocks('')).toEqual([]);
    expect(splitMarkdownBlocks('\n\n  \n')).toEqual([]);
  });
});

describe('块缓存与高亮判定', () => {
  it('同内容切块 hash 稳定，内容变化后 hash 变化', () => {
    const a = splitMarkdownBlocks('同样的段落');
    const b = splitMarkdownBlocks('同样的段落');
    const c = splitMarkdownBlocks('变化后的段落');
    expect(a[0]!.hash).toBe(b[0]!.hash);
    expect(a[0]!.hash).not.toBe(c[0]!.hash);
  });

  it('不同 kind 的相同正文 hash 不同（缓存键不串）', () => {
    const text = 'SELECT 1';
    const code = splitMarkdownBlocks(`\`\`\`sql\n${text}\n\`\`\``);
    const streaming = splitMarkdownBlocks(`\`\`\`sql\n${text}`, true);
    expect(code[0]!.hash).not.toBe(streaming[0]!.hash);
  });

  it('blockCacheKey 是 index 与 hash 的组合', () => {
    const first = splitMarkdownBlocks('任意')[0];
    expect(first).toBeDefined();
    expect(blockCacheKey(first!)).toBe(`0:${first!.hash}`);
  });

  it('isHighlightable 仅对已闭合 code 块为 true', () => {
    const blocks = splitMarkdownBlocks('正文\n\n```bash\ncmd\n```\n\n```py\n未完', true);
    const flags = blocks.map((block) => isHighlightable(block));
    // markdown / streaming-code 不可高亮，闭合 code 可高亮。
    expect(flags).toEqual([false, true, false]);
  });

  it('MarkdownBlockCache 命中时不重复 create，miss 时写入并返回', () => {
    const cache = new MarkdownBlockCache<number>();
    let created = 0;
    const first = cache.get('k', () => {
      created += 1;
      return 1;
    });
    const second = cache.get('k', () => {
      created += 1;
      return 2;
    });
    expect(first).toBe(1);
    expect(second).toBe(1);
    expect(created).toBe(1);
    expect(cache.has('k')).toBe(true);
  });

  it('MarkdownBlockCache 超容量按插入序淘汰最旧（LRU 兜底）', () => {
    const cache = new MarkdownBlockCache<number>(2);
    cache.get('a', () => 1);
    cache.get('b', () => 2);
    cache.get('c', () => 3);
    expect(cache.has('a')).toBe(false);
    expect(cache.has('b')).toBe(true);
    expect(cache.has('c')).toBe(true);
  });

  it('MarkdownBlockCache.clear 清空全部条目', () => {
    const cache = new MarkdownBlockCache<number>();
    cache.get('a', () => 1);
    cache.clear();
    expect(cache.has('a')).toBe(false);
  });
});

describe('stripContextPrefix 展示剥离', () => {
  it('剥离标准前缀，正文原样保留', () => {
    const content = '<!--opsmind-context\nservers: web-1 (id=s1)\n-->\n检查磁盘';
    expect(stripContextPrefix(content)).toBe('检查磁盘');
  });

  it('无前缀时原样返回', () => {
    expect(stripContextPrefix('普通消息')).toBe('普通消息');
  });

  it('前缀内部包含 --> 样式文本时以首个闭合为准', () => {
    const content = '<!--opsmind-context\ndatabases: db (id=d1)\n-->\nSELECT 1 --> 结果';
    expect(stripContextPrefix(content)).toBe('SELECT 1 --> 结果');
  });

  it('正文里再次出现前缀样式的注释不会被剥离（只剥开头）', () => {
    const content = '正文\n<!--opsmind-context\nfake\n-->';
    expect(stripContextPrefix(content)).toBe(content);
  });

  it('空字符串安全', () => {
    expect(stripContextPrefix('')).toBe('');
  });
});
