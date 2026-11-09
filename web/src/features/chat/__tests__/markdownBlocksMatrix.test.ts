import { describe, expect, it } from 'vitest';

import {
  MarkdownBlockCache,
  blockCacheKey,
  isHighlightable,
  splitMarkdownBlocks,
  stripContextPrefix,
} from '../markdownBlocks';

// 增量 Markdown 切块的补充矩阵（CONTRACT.md 8.6.4）：围栏识别边界、
// 流式/非流式 closed 语义、hash 缓存命名空间、展示剥离。
// 与 markdownBlocks.test.ts / markdownBlocksMore.test.ts 互补。

describe('splitMarkdownBlocks 围栏识别边界', () => {
  it('行内单/双反引号不构成围栏', () => {
    const blocks = splitMarkdownBlocks('这是 `code` 与 ``code`` 的行', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('markdown');
  });

  it('4 个空格缩进的 ``` 不识别为围栏（Markdown 缩进代码块语义）', () => {
    const blocks = splitMarkdownBlocks('    ```bash\n    ls\n    ```', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('markdown');
  });

  it('波浪线围栏不能被反引号闭合', () => {
    const blocks = splitMarkdownBlocks('~~~text\n内容\n```\n~~~', false);
    // ``` 不闭合 ~~~ 围栏；到 ~~~ 才闭合，整体是一个 code 块。
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('code');
    expect(blocks[0]?.content).toContain('```');
  });

  it('反引号围栏不能被波浪线闭合', () => {
    const blocks = splitMarkdownBlocks('```bash\nls\n~~~\n```', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('code');
    expect(blocks[0]?.closed).toBe(true);
  });

  it('3 个反引号不能闭合 4 个反引号开的围栏', () => {
    const blocks = splitMarkdownBlocks('````\n内层 ```\n````', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('code');
    expect(blocks[0]?.content).toBe('````\n内层 ```\n````');
  });

  it('更长的波浪线围栏同理按长度匹配', () => {
    const blocks = splitMarkdownBlocks('~~~~\n~~~\n~~~~', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('code');
  });

  it('围栏内的空行保留在 code content 中（不切块）', () => {
    const blocks = splitMarkdownBlocks('```text\na\n\nb\n```', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.content).toBe('```text\na\n\nb\n```');
  });

  it('闭合围栏后紧跟正文（无空行）切成 code + markdown 两块', () => {
    const blocks = splitMarkdownBlocks('```bash\nls\n```\n结论：正常', false);
    expect(blocks.map((block) => block.kind)).toEqual(['code', 'markdown']);
    expect(blocks[1]?.content).toBe('结论：正常');
  });

  it('连续两个代码块（空行分隔）各自成块', () => {
    const source = '```sql\nSELECT 1;\n```\n\n```bash\nls\n```';
    const blocks = splitMarkdownBlocks(source, false);
    expect(blocks.map((block) => block.kind)).toEqual(['code', 'code']);
    expect(blocks[0]?.language).toBe('sql');
    expect(blocks[1]?.language).toBe('bash');
  });

  it('流式中第一个代码块未闭合时其后不再解析内容', () => {
    const blocks = splitMarkdownBlocks('```bash\necho 1\n\n后续内容都算代码', true);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('streaming-code');
    expect(blocks[0]?.content).toBe('echo 1\n\n后续内容都算代码');
  });

  it('未闭合围栏在流式中位于首块（无前文）', () => {
    const blocks = splitMarkdownBlocks('```json\n{"partial":', true);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]).toMatchObject({ kind: 'streaming-code', index: 0, language: 'json' });
  });

  it('闭围栏行带缩进（≤3 空格）也识别闭合', () => {
    const blocks = splitMarkdownBlocks('```bash\nls\n  ```', false);
    expect(blocks[0]?.kind).toBe('code');
    expect(blocks[0]?.closed).toBe(true);
  });
});

describe('splitMarkdownBlocks 语言与 info 解析', () => {
  it('空 info：language 为空字符串', () => {
    const blocks = splitMarkdownBlocks('```\nplain\n```', false);
    expect(blocks[0]?.language).toBe('');
  });

  it('info 带前后空白被 trim 后取首词', () => {
    const blocks = splitMarkdownBlocks('```  bash extra  \nls\n```', false);
    expect(blocks[0]?.language).toBe('bash');
  });

  it('info 首词全大写转小写', () => {
    const blocks = splitMarkdownBlocks('```SQL\nSELECT 1\n```', false);
    expect(blocks[0]?.language).toBe('sql');
  });

  it('streaming-code 的 language 同样解析', () => {
    const blocks = splitMarkdownBlocks('```Bash\necho', true);
    expect(blocks[0]?.language).toBe('bash');
  });

  it('波浪线围栏的 info 也参与语言解析', () => {
    const blocks = splitMarkdownBlocks('~~~yaml\nkey: value\n~~~', false);
    expect(blocks[0]?.language).toBe('yaml');
  });
});

describe('splitMarkdownBlocks 结构与 index', () => {
  it('多块输出 index 连续递增（0..n-1）', () => {
    const source = '段一\n\n```bash\nls\n```\n\n段二\n\n```sql\nSELECT 1;\n```';
    const blocks = splitMarkdownBlocks(source, false);
    expect(blocks.map((block) => block.index)).toEqual([0, 1, 2, 3]);
  });

  it('markdown 块的尾部空白（含尾随空行）被剥离', () => {
    const blocks = splitMarkdownBlocks('段落\n\n\n\n', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.content).toBe('段落');
  });

  it('行间空行保留在 markdown 内容中（仅块尾剥离）', () => {
    const blocks = splitMarkdownBlocks('# 标题\n正文一', false);
    expect(blocks[0]?.content).toBe('# 标题\n正文一');
  });

  it('纯空行多个只切一次块（无空块产生）', () => {
    const blocks = splitMarkdownBlocks('a\n\n\n\nb', false);
    expect(blocks).toHaveLength(2);
    expect(blocks.map((block) => block.content)).toEqual(['a', 'b']);
  });

  it('全空白行不产出任何块', () => {
    expect(splitMarkdownBlocks('   ')).toEqual([]);
    expect(splitMarkdownBlocks('\n')).toEqual([]);
  });

  it('列表/引用等语法原样保留在 markdown 块中', () => {
    const source = '- 项目一\n- 项目二\n\n> 引用\n\n| a | b |\n|---|---|';
    const blocks = splitMarkdownBlocks(source, false);
    expect(blocks).toHaveLength(3);
    expect(blocks[0]?.content).toBe('- 项目一\n- 项目二');
    expect(blocks[1]?.content).toBe('> 引用');
    expect(blocks[2]?.content).toContain('| a | b |');
  });

  it('CRLF 输入：\r 保留在内容中（文档化现状，仅 \n 切行）', () => {
    const blocks = splitMarkdownBlocks('第一行\r\n第二行', false);
    expect(blocks[0]?.content).toBe('第一行\r\n第二行');
  });

  it('CRLF 输入：``` 行尾的 CR 让 FENCE_OPEN 不匹配，整体按 markdown 处理（文档化：服务端只发 LF）', () => {
    const blocks = splitMarkdownBlocks('```bash\r\nls\r\n```\r\n', false);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe('markdown');
  });
});

describe('splitMarkdownBlocks closed 语义矩阵', () => {
  it('streaming=false：markdown / code / streaming-code 全部 closed=true', () => {
    const blocks = splitMarkdownBlocks('段落\n\n```bash\nls\n```\n\n```sql\nSELECT 1', false);
    expect(blocks.map((block) => block.closed)).toEqual([true, true, true]);
  });

  it('streaming=true：非末尾 markdown 块 closed=true 可缓存', () => {
    const blocks = splitMarkdownBlocks('第一段\n\n第二段\n\n第三段正在写', true);
    expect(blocks.map((block) => block.closed)).toEqual([true, true, false]);
  });

  it('streaming=true：末块是闭合 code 时保持 closed=true', () => {
    const blocks = splitMarkdownBlocks('说明\n\n```bash\nls\n```', true);
    expect(blocks.map((block) => block.kind)).toEqual(['markdown', 'code']);
    expect(blocks.map((block) => block.closed)).toEqual([true, true]);
  });

  it('streaming=true：末块是 streaming-code 时 closed=false', () => {
    const blocks = splitMarkdownBlocks('说明\n\n```bash\nls', true);
    expect(blocks[1]?.closed).toBe(false);
  });

  it('内容增长重新切块：前序块 hash 不变（缓存命中前提）', () => {
    const early = splitMarkdownBlocks('第一段\n\n正在写', true);
    const later = splitMarkdownBlocks('第一段\n\n正在写，且继续', true);
    expect(early[0]?.hash).toBe(later[0]?.hash);
    expect(early[1]?.hash).not.toBe(later[1]?.hash);
  });

  it('代码块闭合瞬间：streaming-code → code 的 hash 命名空间切换', () => {
    const before = splitMarkdownBlocks('```bash\nls\n', true);
    const after = splitMarkdownBlocks('```bash\nls\n```', true);
    expect(before[0]?.kind).toBe('streaming-code');
    expect(after[0]?.kind).toBe('code');
    expect(before[0]?.hash).not.toBe(after[0]?.hash);
  });
});

describe('块缓存补充矩阵', () => {
  it('markdown 与 code 的 hash 命名空间隔离（同正文不撞 key）', () => {
    const md = splitMarkdownBlocks('SELECT 1', false);
    const code = splitMarkdownBlocks('```\nSELECT 1\n```', false);
    expect(md[0]?.hash).not.toBe(code[0]?.hash);
  });

  it('语言不同的同正文 code 块 hash 不同', () => {
    const sql = splitMarkdownBlocks('```sql\nSELECT 1\n```', false);
    const bash = splitMarkdownBlocks('```bash\nSELECT 1\n```', false);
    expect(sql[0]?.hash).not.toBe(bash[0]?.hash);
  });

  it('blockCacheKey 随 index 变化：同 hash 不同位置的块 key 不同', () => {
    const blocks = splitMarkdownBlocks('a\n\nb\n\nc', false);
    const key0 = blockCacheKey(blocks[0]!);
    const key2 = blockCacheKey(blocks[2]!);
    expect(key0).not.toBe(key2);
  });

  it('isHighlightable：非流式下强制 closed 的 streaming-code 仍不可高亮', () => {
    const blocks = splitMarkdownBlocks('```bash\nuptime', false);
    // kind 是 streaming-code（非 code）：历史消息未闭合围栏不走进高亮。
    expect(blocks[0]?.kind).toBe('streaming-code');
    expect(blocks[0]?.closed).toBe(true);
    expect(isHighlightable(blocks[0]!)).toBe(false);
  });

  it('isHighlightable：markdown 块一律不可高亮（含 closed=true）', () => {
    const blocks = splitMarkdownBlocks('普通段落', false);
    expect(isHighlightable(blocks[0]!)).toBe(false);
  });

  it('MarkdownBlockCache maxEntries=1：新键淘汰旧键', () => {
    const cache = new MarkdownBlockCache<number>(1);
    cache.get('a', () => 1);
    cache.get('b', () => 2);
    expect(cache.has('a')).toBe(false);
    expect(cache.has('b')).toBe(true);
  });

  it('MarkdownBlockCache 返回缓存值而非重新 create', () => {
    const cache = new MarkdownBlockCache<{ tag: string }>();
    const first = cache.get('k', () => ({ tag: 'v1' }));
    const second = cache.get('k', () => ({ tag: 'v2' }));
    expect(second).toBe(first);
    expect(second.tag).toBe('v1');
  });

  it('MarkdownBlockCache 淘汰后再次访问会重新 create', () => {
    const cache = new MarkdownBlockCache<number>(1);
    expect(cache.get('a', () => 1)).toBe(1);
    cache.get('b', () => 2);
    let created = 0;
    expect(cache.get('a', () => (created += 1))).toBe(1);
    expect(created).toBe(1);
  });

  it('默认容量 2000 内不淘汰', () => {
    const cache = new MarkdownBlockCache<number>();
    for (let index = 0; index < 2000; index += 1) {
      cache.get(`k${index}`, () => index);
    }
    expect(cache.has('k0')).toBe(true);
    expect(cache.has('k1999')).toBe(true);
  });
});

describe('stripContextPrefix 矩阵', () => {
  it('servers 与 databases 双行前缀整体剥离', () => {
    const content = '<!--opsmind-context\nservers: a (id=1)\ndatabases: b (id=2)\n-->\n正文';
    expect(stripContextPrefix(content)).toBe('正文');
  });

  it('注释开始标记后允许空格：<!-- opsmind-context', () => {
    const content = '<!-- opsmind-context\nservers: a\n-->\n正文';
    expect(stripContextPrefix(content)).toBe('正文');
  });

  it('前缀闭合后紧跟空行也被一并剥离（\\s* 吃掉换行）', () => {
    const content = '<!--opsmind-context\nservers: a\n-->\n\n\n正文';
    expect(stripContextPrefix(content)).toBe('正文');
  });

  it('只有前缀没有正文：剥离后为空串', () => {
    expect(stripContextPrefix('<!--opsmind-context\nservers: a\n-->')).toBe('');
  });

  it('正文开头就是普通 HTML 注释不受影响', () => {
    expect(stripContextPrefix('<!--普通注释-->正文')).toBe('<!--普通注释-->正文');
  });

  it('未闭合的前缀（无 -->）不剥离', () => {
    const content = '<!--opsmind-context\nservers: a\n正文';
    expect(stripContextPrefix(content)).toBe(content);
  });

  it('前缀后的空白（含正文前导空格）被贪婪吃掉', () => {
    const content = '<!--opsmind-context\n-->\n  缩进正文';
    expect(stripContextPrefix(content)).toBe('缩进正文');
  });

  it('伪前缀出现在正文中间不被剥离（只锚定开头）', () => {
    const content = '正文一\n<!--opsmind-context\nfake\n-->\n正文二';
    expect(stripContextPrefix(content)).toBe(content);
  });
});
