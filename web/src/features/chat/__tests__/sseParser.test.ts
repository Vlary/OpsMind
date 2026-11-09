import { describe, expect, it } from 'vitest';

import { SseParser } from '../../../api/sse';

describe('SseParser', () => {
  it('解析 LF 分隔的事件帧', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\ndata: {"delta":"a"}\n\n')).toEqual([
      { event: 'token', data: '{"delta":"a"}' },
    ]);
  });

  it('解析 CRLF 并保留跨 chunk 的半帧', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\r\ndata: {"delta":"a"}\r')).toEqual([]);
    expect(parser.pushText('\n\r\n')).toEqual([{ event: 'token', data: '{"delta":"a"}' }]);
  });

  it('跨 chunk 的多字节 UTF-8 不乱码', () => {
    const encoder = new TextEncoder();
    const bytes = encoder.encode('event: token\ndata: {"delta":"你好"}\n\n');
    const cut = bytes.indexOf(0xe4) + 1;
    expect(cut).toBeGreaterThan(0);

    const parser = new SseParser();
    const frames = [...parser.push(bytes.slice(0, cut)), ...parser.push(bytes.slice(cut))];

    expect(frames).toHaveLength(1);
    expect(frames[0]).toEqual({ event: 'token', data: '{"delta":"你好"}' });
  });

  it('多行 data 以换行拼接', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\ndata: line1\ndata: line2\n\n')).toEqual([
      { event: 'token', data: 'line1\nline2' },
    ]);
  });

  it('忽略注释心跳', () => {
    const parser = new SseParser();
    expect(parser.pushText(': ping\n\n')).toEqual([]);
  });

  it('done 与 error 依次作为终止事件', () => {
    const parser = new SseParser();
    const frames = parser.pushText(
      'event: done\ndata: {"message_id":"m1"}\n\nevent: error\ndata: {"code":"STREAM_TIMEOUT"}\n\n',
    );
    expect(frames.map((frame) => frame.event)).toEqual(['done', 'error']);
  });

  it('半帧保留到下一次输入', () => {
    const parser = new SseParser();
    expect(parser.pushText('event: token\ndata: hello')).toEqual([]);
    expect(parser.pushText('\n\n')).toEqual([{ event: 'token', data: 'hello' }]);
  });

  it('end 冲刷未以空行结束的帧', () => {
    const parser = new SseParser();
    parser.pushText('event: token\ndata: tail');
    expect(parser.end()).toEqual([{ event: 'token', data: 'tail' }]);
  });
});
