import { describe, expect, it } from 'vitest';

import hljs from 'highlight.js/lib/core';
import { isValidElement } from 'react';

import { loadLanguage, renderHighlight } from './codeBlockHighlight';

// CodeBlock 高亮管线的补充矩阵：受支持语言全量注册、别名表逐项、
// renderHighlight 的嵌套/兄弟/实体解码/多余闭合等结构分支。
// 与 codeBlock.test.ts（加载失败降级与基础解析）互补。
// 注意：go 语言在 codeBlock.test.ts 中被 vi.mock 为加载失败，本文件
// 不 mock，验证真实 go 语言模块可加载注册。

describe('loadLanguage 受支持语言全量', () => {
  it.each(['bash', 'json', 'sql', 'go', 'typescript', 'yaml', 'nginx'] as const)(
    '语言 %s 可加载并注册到 hljs',
    async (language) => {
      expect(await loadLanguage(language)).toBe(language);
      expect(hljs.getLanguage(language)).toBeTruthy();
    },
  );

  it('重复加载同一语言幂等（已注册直接返回）', async () => {
    const first = await loadLanguage('sql');
    const second = await loadLanguage('sql');
    expect(first).toBe('sql');
    expect(second).toBe('sql');
  });
});

describe('loadLanguage 别名矩阵', () => {
  it.each([
    ['sh', 'bash'],
    ['shell', 'bash'],
    ['zsh', 'bash'],
    ['console', 'bash'],
    ['ts', 'typescript'],
    ['yml', 'yaml'],
  ] as const)('别名 %s 归一化为 %s', async (alias, canonical) => {
    expect(await loadLanguage(alias)).toBe(canonical);
  });

  it.each(['text', 'plaintext', 'txt'] as const)('纯文本别名 %s 明确降级为 null', async (alias) => {
    expect(await loadLanguage(alias)).toBeNull();
  });

  it('语言名大小写与首尾空白被归一', async () => {
    expect(await loadLanguage('BASH')).toBe('bash');
    expect(await loadLanguage('  sql  ')).toBe('sql');
  });

  it('受支持语言名的一部分（如 types）不命中', async () => {
    expect(await loadLanguage('types')).toBeNull();
    expect(await loadLanguage('python')).toBeNull();
  });
});

describe('renderHighlight 结构矩阵', () => {
  it('嵌套 span 解析为嵌套 React 元素', () => {
    const nodes = renderHighlight('<span class="a"><span class="b">inner</span></span>');
    expect(nodes).not.toBeNull();
    expect(nodes).toHaveLength(1);
    const outer = nodes?.[0];
    expect(isValidElement(outer)).toBe(true);
    if (isValidElement<{ className: string; children: unknown[] }>(outer)) {
      expect(outer.props.className).toBe('a');
      expect(outer.props.children).toHaveLength(1);
    }
  });

  it('兄弟 span 与中间文本混合输出', () => {
    const nodes = renderHighlight('<span class="a">x</span> mid <span class="b">y</span>');
    expect(nodes).not.toBeNull();
    expect(nodes).toHaveLength(3);
    expect(nodes?.[1]).toBe(' mid ');
  });

  it('纯文本（无 span）输出为单一字符串', () => {
    expect(renderHighlight('just text')).toEqual(['just text']);
  });

  it('span 前后的文本都保留', () => {
    const nodes = renderHighlight('pre<span class="k">mid</span>post');
    expect(nodes).toEqual(['pre', expect.anything(), 'post']);
  });

  it('多余闭合 </span> 被忽略（栈空时 pop 不产出）', () => {
    const nodes = renderHighlight('<span class="a">x</span></span>');
    // 整体长度匹配且最终栈空：多余闭合被丢弃，仍返回节点。
    expect(nodes).not.toBeNull();
    expect(nodes).toHaveLength(1);
  });

  it('class 值为空字符串仍视为合法开标签', () => {
    const nodes = renderHighlight('<span class="">x</span>');
    expect(nodes).not.toBeNull();
    expect(nodes).toHaveLength(1);
  });

  it('其他标签形式（<div>）不匹配 → 整体 null', () => {
    expect(renderHighlight('<div>x</div>')).toBeNull();
    expect(renderHighlight("<span class='a'>x</span>")).toBeNull();
  });

  it('跨行文本内容原样保留（children 为单元素数组）', () => {
    const nodes = renderHighlight('<span class="k">line1\nline2</span>');
    expect(nodes).not.toBeNull();
    const element = nodes?.[0];
    if (isValidElement<{ children: unknown[] }>(element)) {
      expect(element.props.children).toEqual(['line1\nline2']);
    }
  });
});

describe('renderHighlight HTML 实体解码矩阵', () => {
  it('命名实体：amp / lt / gt / quot / #39', () => {
    expect(renderHighlight('&amp;')).toEqual(['&']);
    expect(renderHighlight('&lt;&gt;')).toEqual(['<>']);
    expect(renderHighlight('&quot;')).toEqual(['"']);
    expect(renderHighlight('&#39;')).toEqual(["'"]);
  });

  it('十进制数字实体', () => {
    expect(renderHighlight('&#65;')).toEqual(['A']);
    expect(renderHighlight('&#20013;')).toEqual(['中']);
  });

  it('十六进制数字实体仅支持小写 #x 前缀（大写 #X 不识别，原样保留）', () => {
    expect(renderHighlight('&#x27;')).toEqual(["'"]);
    expect(renderHighlight('&#x41;')).toEqual(['A']);
    expect(renderHighlight('&#X41;')).toEqual(['&#X41;']);
  });

  it('span 内文本的实体同样解码', () => {
    const nodes = renderHighlight('<span class="k">&lt;tag&gt;</span>');
    const element = nodes?.[0];
    if (isValidElement<{ children: unknown[] }>(element)) {
      expect(element.props.children).toEqual(['<tag>']);
    }
  });

  it('未知实体原样保留', () => {
    expect(renderHighlight('&nbsp;')).toEqual(['&nbsp;']);
    expect(renderHighlight('&random;')).toEqual(['&random;']);
  });

  it('实体与普通文本混排', () => {
    expect(renderHighlight('a &amp; b')).toEqual(['a & b']);
  });
});
