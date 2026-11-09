// F1 单测：asciicast v2 NDJSON 流式解析（契约 9.4：只测纯逻辑）。
import { describe, expect, it } from 'vitest';

import {
  CastParser,
  loadCastFromResponse,
  parseResizeData,
  type CastParseResult,
} from '../castParser';

function parseAll(text: string): CastParseResult {
  const parser = new CastParser();
  parser.feed(text);
  return parser.end();
}

describe('parseResizeData', () => {
  it('解析 COLSxROWS', () => {
    expect(parseResizeData('120x40')).toEqual({ cols: 120, rows: 40 });
    expect(parseResizeData(' 80x24 ')).toEqual({ cols: 80, rows: 24 });
  });

  it('拒绝非法值', () => {
    expect(parseResizeData('x40')).toBeNull();
    expect(parseResizeData('0x40')).toBeNull();
    expect(parseResizeData('120x')).toBeNull();
    expect(parseResizeData('120x40x2')).toBeNull();
    expect(parseResizeData('-3x40')).toBeNull();
  });
});

describe('CastParser', () => {
  it('解析 header 与 o/r 事件，maxTime 取最大时间戳', () => {
    const result = parseAll(
      [
        '{"version":2,"width":100,"height":30}',
        '[0.012, "o", "hello\\r\\n"]',
        '[1.5, "r", "120x40"]',
        '[2.25, "o", "world"]',
      ].join('\n'),
    );
    expect(result.header).toEqual({ version: 2, width: 100, height: 30 });
    expect(result.events).toEqual([
      { kind: 'o', time: 0.012, data: 'hello\r\n' },
      { kind: 'r', time: 1.5, cols: 120, rows: 40 },
      { kind: 'o', time: 2.25, data: 'world' },
    ]);
    expect(result.maxTime).toBe(2.25);
    expect(result.badLines).toBe(0);
  });

  it('跨 chunk 切分不丢半行（含 CRLF 行尾）', () => {
    const parser = new CastParser();
    parser.feed('{"version":2,"width":80,"height":24}\r\n[0.5, "o", "abc');
    parser.feed('def"]\r\n[1.0, "o", "ghi"]');
    const result = parser.end();
    expect(result.events).toEqual([
      { kind: 'o', time: 0.5, data: 'abcdef' },
      { kind: 'o', time: 1.0, data: 'ghi' },
    ]);
  });

  it('坏行容错跳过并计数，i/m 事件忽略', () => {
    const result = parseAll(
      [
        '{"version":2,"width":80,"height":24}',
        'not json',
        '[0.1, "i", "secret"]',
        '[0.2, "o", "ok"]',
        '[bad, "o", "x"]',
        '[0.3, "r", "nonsense"]',
        '[0.4, "o", "still ok"]',
      ].join('\n'),
    );
    expect(result.events.map((event) => event.kind)).toEqual(['o', 'o']);
    // not json / [bad...] / [0.3,"r","nonsense"] 三行坏行；i 行不计坏行。
    expect(result.badLines).toBe(3);
  });

  it('version!=2 的 header 计为坏行且后续行按事件尝试', () => {
    const result = parseAll(['{"version":1,"width":80,"height":24}', '[0.1, "o", "x"]'].join('\n'));
    expect(result.header).toBeNull();
    expect(result.badLines).toBe(2);
  });

  it('end 后 feed 不再生效（幂等收尾）', () => {
    const parser = new CastParser();
    parser.feed('{"version":2,"width":80,"height":24}\n[0.1, "o", "x"]\n');
    const first = parser.end();
    parser.feed('[9, "o", "late"]');
    const second = parser.end();
    expect(second).toEqual(first);
    expect(second.events).toHaveLength(1);
  });

  it('空输入与空文件', () => {
    const result = parseAll('');
    expect(result.header).toBeNull();
    expect(result.events).toEqual([]);
    expect(result.maxTime).toBe(0);
  });
});

describe('loadCastFromResponse（流式，不整块载入）', () => {
  function streamResponse(chunks: string[]): Response {
    const encoder = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        for (const chunk of chunks) {
          controller.enqueue(encoder.encode(chunk));
        }
        controller.close();
      },
    });
    return new Response(stream, { status: 200 });
  }

  it('逐块读取并解析完整结果', async () => {
    const response = streamResponse([
      '{"version":2,"width":120,"height":40}\n[0.1, "o", "a"]\n[0.2',
      ', "o", "b"]\n[3, "r", "200x50"]\n',
    ]);
    const result = await loadCastFromResponse(response);
    expect(result.header?.width).toBe(120);
    expect(result.events).toHaveLength(3);
    expect(result.maxTime).toBe(3);
  });

  it('非 2xx 响应报错', async () => {
    const response = new Response('nope', { status: 404 });
    await expect(loadCastFromResponse(response)).rejects.toThrow('HTTP 404');
  });

  it('signal 中止时抛 AbortError', async () => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('{"version":2,"width":1,"height":1}\n'));
        // 永不 close：触发中止路径。
      },
    });
    const controller = new AbortController();
    controller.abort();
    await expect(
      loadCastFromResponse(new Response(stream, { status: 200 }), {
        signal: controller.signal,
      }),
    ).rejects.toThrow();
  });
});
