import { describe, expect, it } from 'vitest';

import { SseParser } from '../../../api/sse';

// SSE 分帧解析矩阵（WHATWG SSE 规范 + OpsMind 服务端实际发送形态）：
// 覆盖换行变体、字段语义、跨 chunk 边界与 end() 冲刷的全部分支，
// 与 sseParser.test.ts / sseParserEdge.test.ts 的主链路互补。

describe('SseParser 字段语义矩阵', () => {
  it('data 后无空格：值原样（冒号后直接是内容）', () => {
    const parser = new SseParser();
    expect(parser.pushText('data:value\n\n')).toEqual([{ event: 'message', data: 'value' }]);
  });

  it('data 后一个空格被剥离（规范：仅一个前导空格）', () => {
    const parser = new SseParser();
    expect(parser.pushText('data: value\n\n')).toEqual([{ event: 'message', data: 'value' }]);
  });

  it('data 后两个空格只剥一个', () => {
    const parser = new SseParser();
    expect(parser.pushText('data:  value\n\n')).toEqual([{ event: 'message', data: ' value' }]);
  });

  it('裸字段名 data（无冒号）按空值 data 行处理', () => {
    const parser = new SseParser();
    expect(parser.pushText('data\n\n')).toEqual([{ event: 'message', data: '' }]);
  });

  it('data: 冒号结尾也是空值 data 行', () => {
    const parser = new SseParser();
    expect(parser.pushText('data:\n\n')).toEqual([{ event: 'message', data: '' }]);
  });

  it('空值 data 行与有值行混排：空行保留在拼接结果中', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data:\ndata: a\ndata:\n\n');
    expect(frames).toEqual([{ event: 'message', data: '\na\n' }]);
  });

  it('event 字段后跟多个 data 行组成一帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText('event: tool_call\ndata: {"id":1}\ndata: {"id":2}\n\n');
    expect(frames).toEqual([{ event: 'tool_call', data: '{"id":1}\n{"id":2}' }]);
  });

  it('同帧内 event 出现多次：最后一次生效', () => {
    const parser = new SseParser();
    const frames = parser.pushText('event: a\nevent: token\ndata: x\n\n');
    expect(frames).toEqual([{ event: 'token', data: 'x' }]);
  });

  it('event 字段值与 data 同样只剥一个前导空格', () => {
    const parser = new SseParser();
    // 冒号后第一个空格被剥，第二个保留（规范行为与 data 一致）。
    expect(parser.pushText('event:  token\ndata: x\n\n')).toEqual([{ event: ' token', data: 'x' }]);
  });

  it('未知字段（如 x-custom）被忽略且不影响帧产出', () => {
    const parser = new SseParser();
    const frames = parser.pushText('x-custom: anything\ndata: keep\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'keep' }]);
  });

  it('id 字段被忽略：不改变后续帧、不产生输出', () => {
    const parser = new SseParser();
    const frames = parser.pushText('id: 1\ndata: a\n\nid: 2\ndata: b\n\n');
    expect(frames).toEqual([
      { event: 'message', data: 'a' },
      { event: 'message', data: 'b' },
    ]);
  });

  it('retry 字段被忽略', () => {
    const parser = new SseParser();
    expect(parser.pushText('retry: 5000\ndata: ok\n\n')).toEqual([
      { event: 'message', data: 'ok' },
    ]);
  });

  it('注释行可夹在 data 行之间，不影响拼接', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data: a\n: keepalive\ndata: b\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'a\nb' }]);
  });

  it('无字段的整行（无冒号）不是 data：帧在空行处因无 data 被丢弃', () => {
    const parser = new SseParser();
    expect(parser.pushText('eventonly\ndata: real\n\n')).toEqual([
      { event: 'message', data: 'real' },
    ]);
  });

  it('字段名大小写敏感：DATA 不是 data', () => {
    const parser = new SseParser();
    // DATA 大写不进 dataLines → 空行处无 data，帧被丢弃。
    expect(parser.pushText('DATA: x\n\n')).toEqual([]);
  });

  it('仅注释的“帧”不产出（注释不产生 data）', () => {
    const parser = new SseParser();
    expect(parser.pushText(': a\n: b\n\n')).toEqual([]);
  });

  it('空行重置 event 名：未完成的 event 不泄漏到下一帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText('event: token\n\ndata: later\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'later' }]);
  });
});

describe('SseParser OpsMind 事件序列', () => {
  it('完整的 token → done 序列按输入顺序产出', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      [
        'event: token\ndata: {"delta":"检"}\n\n',
        'event: token\ndata: {"delta":"查"}\n\n',
        'event: done\ndata: {"message_id":"m-9"}\n\n',
      ].join(''),
    );
    expect(frames).toEqual([
      { event: 'token', data: '{"delta":"检"}' },
      { event: 'token', data: '{"delta":"查"}' },
      { event: 'done', data: '{"message_id":"m-9"}' },
    ]);
  });

  it('approval_required / approval_resolved 帧可解析', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      [
        'event: approval_required\ndata: {"id":"ap1","status":"pending"}\n\n',
        'event: approval_resolved\ndata: {"approval_id":"ap1","status":"approved"}\n\n',
      ].join(''),
    );
    expect(frames.map((frame) => frame.event)).toEqual(['approval_required', 'approval_resolved']);
    expect(frames[0]?.data).toBe('{"id":"ap1","status":"pending"}');
    expect(frames[1]?.data).toBe('{"approval_id":"ap1","status":"approved"}');
  });

  it('tool_call / tool_result 帧 CRLF 形态与 LF 语义一致', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      'event: tool_call\r\ndata: {"tool_call_id":"t1"}\r\n\r\nevent: tool_result\r\ndata: {"ok":true}\r\n\r\n',
    );
    expect(frames).toEqual([
      { event: 'tool_call', data: '{"tool_call_id":"t1"}' },
      { event: 'tool_result', data: '{"ok":true}' },
    ]);
  });

  it('error 帧是终止事件且可携带 request_id', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      'event: error\ndata: {"code":"LLM_TIMEOUT","request_id":"r1"}\n\n',
    );
    expect(frames).toEqual([{ event: 'error', data: '{"code":"LLM_TIMEOUT","request_id":"r1"}' }]);
  });

  it('reasoning 帧与 token 帧交错产出', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      [
        'event: reasoning\ndata: {"delta":"想"}\n\n',
        'event: token\ndata: {"delta":"答"}\n\n',
        'event: reasoning\ndata: {"delta":"考"}\n\n',
      ].join(''),
    );
    expect(frames.map((frame) => frame.event)).toEqual(['reasoning', 'token', 'reasoning']);
  });

  it('100 帧批量输入全部产出（长回答）', () => {
    const parser = new SseParser();
    const source = Array.from(
      { length: 100 },
      (_, index) => `event: token\ndata: {"delta":"${index}"}\n\n`,
    ).join('');
    const frames = parser.pushText(source);
    expect(frames).toHaveLength(100);
    expect(frames[0]?.data).toBe('{"delta":"0"}');
    expect(frames[99]?.data).toBe('{"delta":"99"}');
  });
});

describe('SseParser 跨 chunk 边界矩阵（push 字节）', () => {
  function bytesOf(text: string): Uint8Array {
    return new TextEncoder().encode(text);
  }

  it('行首 event 与行内 data 分别跨 chunk 到达', () => {
    const parser = new SseParser();
    expect(parser.push(bytesOf('event: tok'))).toEqual([]);
    expect(parser.push(bytesOf('en\ndata: {"del'))).toEqual([]);
    expect(parser.push(bytesOf('ta":"x"}\n\n'))).toEqual([
      { event: 'token', data: '{"delta":"x"}' },
    ]);
  });

  it('4 字节 emoji 跨 chunk 切成 3 段仍拼回', () => {
    const parser = new SseParser();
    const bytes = bytesOf('data: 🔴\n\n');
    // 🔴 是 4 字节（F0 9F 94 B4）：在 3 个不同字节边界切开。
    const cut1 = 7; // data:  之后第 1 字节前
    const cut2 = 9; // emoji 中间
    expect(parser.push(bytes.slice(0, cut1))).toEqual([]);
    expect(parser.push(bytes.slice(cut1, cut2))).toEqual([]);
    expect(parser.push(bytes.slice(cut2))).toEqual([{ event: 'message', data: '🔴' }]);
  });

  it('CRLF 的 CR 与 LF 分属两个 chunk：不提前断行也不丢字符', () => {
    const parser = new SseParser();
    expect(parser.push(bytesOf('data: a'))).toEqual([]);
    expect(parser.push(bytesOf('\r'))).toEqual([]);
    expect(parser.push(bytesOf('\n\r\n'))).toEqual([{ event: 'message', data: 'a' }]);
  });

  it('连续两个 \r\n 换行跨 chunk：帧分隔完整', () => {
    const parser = new SseParser();
    expect(parser.push(bytesOf('data: x\r'))).toEqual([]);
    expect(parser.push(bytesOf('\n\r'))).toEqual([]);
    expect(parser.push(bytesOf('\n'))).toEqual([{ event: 'message', data: 'x' }]);
  });

  it('逐字节喂入整帧仍完整解析（极端分片）', () => {
    const parser = new SseParser();
    const bytes = bytesOf('event: token\ndata: {"delta":"碎"}\n\n');
    const frames: unknown[] = [];
    for (const byte of bytes) {
      frames.push(...parser.push(Uint8Array.from([byte])));
    }
    expect(frames).toEqual([{ event: 'token', data: '{"delta":"碎"}' }]);
  });

  it('多帧混 CRLF 的字节流按任意 8 字节分片：帧数与内容不变', () => {
    const source =
      'event: a\ndata: 1\r\n\r\nevent: b\ndata: 2\n\r\n\r\nevent: c\r\ndata: 3\r\n\r\n';
    const bytes = bytesOf(source);
    const parser = new SseParser();
    const frames: { event: string; data: string }[] = [];
    for (let offset = 0; offset < bytes.length; offset += 8) {
      frames.push(...parser.push(bytes.slice(offset, offset + 8)));
    }
    expect(frames).toEqual([
      { event: 'a', data: '1' },
      { event: 'b', data: '2' },
      { event: 'c', data: '3' },
    ]);
  });

  it('空 chunk（0 字节）不产生帧也不破坏状态', () => {
    const parser = new SseParser();
    parser.push(new Uint8Array(0));
    expect(parser.pushText('data: after-empty-chunk\n\n')).toEqual([
      { event: 'message', data: 'after-empty-chunk' },
    ]);
  });

  it('pushText 与 push 字节可交替使用（同一 decoder 流）', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\ndata: ')).toEqual([]);
    expect(parser.push(bytesOf('{"delta":"混"}\n\n'))).toEqual([
      { event: 'token', data: '{"delta":"混"}' },
    ]);
  });
});

describe('SseParser end() 冲刷矩阵', () => {
  it('残留的 event + data 行在 end() 时合成帧', () => {
    const parser = new SseParser();
    parser.pushText('event: done\ndata: {"message_id":"tail"}');
    expect(parser.end()).toEqual([{ event: 'done', data: '{"message_id":"tail"}' }]);
  });

  it('残留多行 data 在 end() 时按 \\n 拼接', () => {
    const parser = new SseParser();
    parser.pushText('data: l1\ndata: l2');
    expect(parser.end()).toEqual([{ event: 'message', data: 'l1\nl2' }]);
  });

  it('残留以 \\n 结尾（帧已完整、只差空行）：end() 补出该帧', () => {
    const parser = new SseParser();
    parser.pushText('data: complete\n');
    expect(parser.end()).toEqual([{ event: 'message', data: 'complete' }]);
  });

  it('无残留时 end() 产出空数组', () => {
    const parser = new SseParser();
    parser.pushText('data: done\n\n');
    expect(parser.end()).toEqual([]);
  });

  it('残留是注释行：end() 不产出帧', () => {
    const parser = new SseParser();
    parser.pushText(': trailing comment');
    expect(parser.end()).toEqual([]);
  });

  it('残留 event 行无 data：end() 丢弃且不产出', () => {
    const parser = new SseParser();
    parser.pushText('event: orphan');
    expect(parser.end()).toEqual([]);
  });

  it('end() 后 parser 可继续使用（状态清零）', () => {
    const parser = new SseParser();
    parser.pushText('data: first\n\n');
    parser.end();
    expect(parser.pushText('data: second\n\n')).toEqual([{ event: 'message', data: 'second' }]);
  });

  it('end() 冲刷被截断的多字节序列：decoder 产出替换字符（不静默丢弃）', () => {
    const parser = new SseParser();
    const bytes = new TextEncoder().encode('data: 尾');
    // 喂入前 8 字节（“尾”的 3 字节序列被截成 2 字节），end() 冲刷 decoder。
    expect(parser.push(bytes.slice(0, 8))).toEqual([]);
    // 截断的“尾”冲刷为替换字符，且仍走 data 字段值（冒号后）路径。
    expect(parser.end()).toEqual([{ event: 'message', data: '\uFFFD' }]);
  });
});

describe('SseParser 换行变体补充', () => {
  it('单独 CR 分隔的字段行可解析（老 Mac 风格）', () => {
    const parser = new SseParser();
    const frames = parser.pushText('event: token\rdata: x\r\r');
    // 尾部 \r 是悬挂换行：等 end() 或下一 chunk。
    expect(frames).toEqual([]);
    expect(parser.end()).toEqual([{ event: 'token', data: 'x' }]);
  });

  it('LF 与 CR 混用且 CR 靠前：先到先断行', () => {
    const parser = new SseParser();
    // 'data: a\r' 之后紧跟 '\n'：\r\n 视作一个换行（宽度 2）。
    const frames = parser.pushText('data: a\r\n\n');
    expect(frames).toEqual([{ event: 'message', data: 'a' }]);
  });

  it('帧间多个空行（含 CR/LF 混合）不产生空帧', () => {
    const parser = new SseParser();
    const frames = parser.pushText('data: only\n\n\r\n\n\r\r\n');
    expect(frames).toEqual([{ event: 'message', data: 'only' }]);
  });

  it('值为空的 event 行后直接空行：无 data 帧被丢弃', () => {
    const parser = new SseParser();
    expect(parser.pushText('event:\n\n')).toEqual([]);
  });

  it('大 payload 单帧（10KB data）不丢内容', () => {
    const parser = new SseParser();
    const big = 'x'.repeat(10_000);
    const frames = parser.pushText(`data: ${big}\n\n`);
    expect(frames).toHaveLength(1);
    expect(frames[0]?.data).toBe(big);
  });
});
