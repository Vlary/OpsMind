import { beforeEach, describe, expect, it, vi } from 'vitest';

// 模拟语言 chunk 加载失败（发版后旧 hash 404 / 弱网）：go 语言模块的
// import() reject，loadLanguage 必须降级返回 null 而不是抛错（契约 8.6.5）。
vi.mock('highlight.js/lib/languages/go', () => {
  throw new Error('Failed to fetch dynamically imported module');
});

import hljs from 'highlight.js/lib/core';
import { loadLanguage, renderHighlight } from './codeBlockHighlight';

describe('loadLanguage（契约 8.6.5：语言加载失败降级纯文本）', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('空语言与未知语言返回 null（按纯文本渲染）', async () => {
    expect(await loadLanguage(undefined)).toBeNull();
    expect(await loadLanguage('')).toBeNull();
    expect(await loadLanguage('not-a-language')).toBeNull();
  });

  it('别名归一化到受支持语言并成功注册', async () => {
    expect(await loadLanguage('JSON')).toBe('json');
    expect(await loadLanguage('sh')).toBe('bash');
    expect(hljs.getLanguage('bash')).toBeTruthy();
  });

  it('动态 import 失败降级为 null，不抛错', async () => {
    expect(await loadLanguage('go')).toBeNull();
    // 失败结果被缓存：再次请求同样降级且不重复触发 import。
    expect(await loadLanguage('go')).toBeNull();
  });
});

describe('renderHighlight（契约 8.6.5：高亮解析失败降级纯文本）', () => {
  it('合法的嵌套 span 结构解析为节点树', () => {
    const nodes = renderHighlight('<span class="hljs-keyword">if</span> x');
    expect(nodes).not.toBeNull();
    expect(nodes?.length).toBeGreaterThan(0);
  });

  it('未闭合的 span（栈非空）返回 null → 调用方回退纯文本', () => {
    expect(renderHighlight('<span class="hljs-keyword">if')).toBeNull();
  });

  it('含未识别结构的 HTML（匹配长度不完整）返回 null', () => {
    // "<x" 不匹配任何模式（非 span 开标签/闭标签/文本），matchedLength 不等于总长。
    expect(renderHighlight('a<x')).toBeNull();
  });

  it('空字符串返回空节点数组', () => {
    expect(renderHighlight('')).toStrictEqual([]);
  });
});
