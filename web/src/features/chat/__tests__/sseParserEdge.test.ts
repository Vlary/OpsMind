import { describe, expect, it } from 'vitest';

import { SseParser } from '../../../api/sse';

// SSE 分帧解析的规范边界（WHATWG SSE）：与既有 sseParser.test.ts 的
// 主链路互补，聚焦换行变体、多行 data、注释、字段值修剪与跨 chunk 半帧。

describe('SseParser 换行与分帧边界', () => {
  it('CRLF 与 LF 混合输入各自成帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText('event: a\ndata: 1\r\n\r\nevent: b\ndata: 2\n\n');
    expect(frames).toEqual([
      { event: 'a', data: '1' },
      { event: 'b', data: '2' },
    ]);
  });

  it('单独 CR 也作为换行（\\r\\n 未跨 chunk 时不吞字符）', () => {
    const parser = new SseParser();
    // 第二个数据行后的 \r 处于缓冲区末尾：可能是 \r\n 前半段，须等 end()。
    expect(parser.pushText('data: hello\r\rdata: world\r\r')).toEqual([
      { event: 'message', data: 'hello' },
    ]);
    expect(parser.end()).toEqual([{ event: 'message', data: 'world' }]);
  });

  it('chunk 以 \\r 结尾时等待下一 chunk 拼出 \\r\\n，不产生半帧', () => {
    const parser = new SseParser();
    expect(parser.pushText('data: a\r')).toEqual([]);
    expect(parser.pushText('\n\r\n')).toEqual([{ event: 'message', data: 'a' }]);
  });

  it('多行 data 按规范以 \\n 拼接', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data: line1\ndata: line2\ndata:\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'line1\nline2\n' }]);
  });

  it(': comment 心跳行被忽略，不影响后续帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText(': ping\ndata: real\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'real' }]);
  });

  it('无字段名的整行（无冒号）被当作空值 field，不产生 data', () => {
    const parser = new SseParser();
    // "dataless" 是非法字段：不进 dataLines，帧因 data 为空在空行处被丢弃。
    const frames = parser.pushText('dataless\n\n');
    expect(frames).toEqual([]);
  });

  it('字段值仅剥离一个前导空格，其余保留', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data:  two spaces before\n\n');
    expect(frames).toEqual([{ event: 'message', data: ' two spaces before' }]);
  });

  it('event 字段后的空行丢弃重置 eventName，不误用到下一帧', () => {
    const parser = new SseParser();
    // event:a 帧没有 data 行 → 空行处丢弃且 eventName 被清空。
    const frames = parser.pushText('event: a\n\ndata: after\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'after' }]);
  });

  it('id / retry 字段被忽略但允许出现在帧内', () => {
    const parser = new SseParser();
    const frames = parser.pushText('id: 42\nretry: 1000\ndata: payload\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'payload' }]);
  });

  it('连续空行只产出一次帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data: once\n\n\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'once' }]);
  });
});

describe('SseParser 字节流边界（push Uint8Array）', () => {
  it('多字节 UTF-8 字符跨 chunk 分割后正确拼回', () => {
    const parser = new SseParser();
    const bytes = new TextEncoder().encode('data: 磁盘使用率 83%\n\n');
    const cut = 9; // 切在多字节字符中间。
    expect(parser.push(bytes.slice(0, cut))).toEqual([]);
    const frames = parser.push(bytes.slice(cut));
    expect(frames).toEqual([{ event: 'message', data: '磁盘使用率 83%' }]);
  });

  it('行内容本身跨 chunk 到达时不提前断帧', () => {
    const parser = new SseParser();
    expect(parser.push(new TextEncoder().encode('data: partial'))).toEqual([]);
    expect(parser.push(new TextEncoder().encode(' token\n\n'))).toEqual([
      { event: 'message', data: 'partial token' },
    ]);
  });

  it('end() 冲刷未以空行结束的最后一帧', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\ndata: {"delta":"尾部"}')).toEqual([]);
    expect(parser.end()).toEqual([{ event: 'token', data: '{"delta":"尾部"}' }]);
  });

  it('end() 处理以 \\r 结尾的残留行（剥离悬挂 CR）', () => {
    const parser = new SseParser();
    expect(parser.pushText('data: tail\r')).toEqual([]);
    expect(parser.end()).toEqual([{ event: 'message', data: 'tail' }]);
  });

  it('end() 可重复调用，不重复产出帧', () => {
    const parser = new SseParser();
    parser.pushText('data: x\n\n');
    expect(parser.end()).toEqual([]);
    expect(parser.end()).toEqual([]);
  });
});
