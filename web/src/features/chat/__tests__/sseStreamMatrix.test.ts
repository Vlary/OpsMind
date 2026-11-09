import { describe, expect, it } from 'vitest';

import { readSseStream } from '../../../api/sse';

// readSseStream（fetch body → 帧序列）的补充矩阵：与 api/sseStream.test.ts
// 的基础用例互补，覆盖逐 chunk 帧产出、CRLF 字节流、注释心跳穿插、
// 尾帧冲刷顺序与提前取消（迭代器 return 释放 reader）。

function streamOf(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
      }
      controller.close();
    },
  });
}

async function collect(chunks: string[]) {
  const frames = [];
  for await (const frame of readSseStream(streamOf(chunks))) {
    frames.push(frame);
  }
  return frames;
}

describe('readSseStream 补充矩阵', () => {
  it('每个 chunk 恰好一帧：帧随 chunk 逐个产出', async () => {
    const frames = await collect(['event: token\ndata: 1\n\n', 'event: token\ndata: 2\n\n']);
    expect(frames).toEqual([
      { event: 'token', data: '1' },
      { event: 'token', data: '2' },
    ]);
  });

  it('CRLF 字节流：帧正常产出且无残留字符', async () => {
    const frames = await collect(['event: a\r\ndata: v\r\n\r\n']);
    expect(frames).toEqual([{ event: 'a', data: 'v' }]);
  });

  it('注释心跳穿插在 token 帧之间被丢弃', async () => {
    const frames = await collect([
      'event: token\ndata: {"delta":"a"}\n\n',
      ': ping\n\n',
      'event: token\ndata: {"delta":"b"}\n\n',
    ]);
    expect(frames).toEqual([
      { event: 'token', data: '{"delta":"a"}' },
      { event: 'token', data: '{"delta":"b"}' },
    ]);
  });

  it('多字节中文 token 逐字符跨 chunk 到达', async () => {
    const frames = await collect([
      'event: token\ndata: {"delta":"磁',
      '盘"}\n\nevent: token\ndata: {"delta":"使',
      '用率"}\n\n',
    ]);
    expect(frames).toEqual([
      { event: 'token', data: '{"delta":"磁盘"}' },
      { event: 'token', data: '{"delta":"使用率"}' },
    ]);
  });

  it('尾部 CR 悬挂 + 无后续 chunk：end() 冲刷补帧', async () => {
    const frames = await collect(['event: token\ndata: {"delta":"尾"}\r']);
    expect(frames).toEqual([{ event: 'token', data: '{"delta":"尾"}' }]);
  });

  it('仅注释心跳的流产出空序列', async () => {
    expect(await collect([': ping\n\n: pong\n\n'])).toEqual([]);
  });

  it('id/retry 行穿插不影响帧内容', async () => {
    const frames = await collect(['id: 7\nretry: 100\n\nevent: token\ndata: x\n\n']);
    expect(frames).toEqual([{ event: 'token', data: 'x' }]);
  });

  it('error 帧后仍有 done 帧：两者都产出（终止处理由上层）', async () => {
    const frames = await collect([
      'event: error\ndata: {"code":"INTERNAL"}\n\n',
      'event: done\ndata: {"message_id":"m"}\n\n',
    ]);
    expect(frames.map((frame) => frame.event)).toEqual(['error', 'done']);
  });

  it('大 token 帧跨 16 字节分片：内容完整', async () => {
    const big = 'a'.repeat(2048);
    const bytes = new TextEncoder().encode(`data: ${big}\n\n`);
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        for (let offset = 0; offset < bytes.length; offset += 16) {
          controller.enqueue(bytes.slice(offset, offset + 16));
        }
        controller.close();
      },
    });
    const frames = [];
    for await (const frame of readSseStream(stream)) {
      frames.push(frame);
    }
    expect(frames).toEqual([{ event: 'message', data: big }]);
  });

  it('提前 break（上层 abort 语义）：迭代停止且不再产出', async () => {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        for (let index = 0; index < 10; index += 1) {
          controller.enqueue(new TextEncoder().encode(`data: ${index}\n\n`));
        }
        controller.close();
      },
    });
    const seen: string[] = [];
    for await (const frame of readSseStream(stream)) {
      seen.push(frame.data);
      if (seen.length === 3) {
        break;
      }
    }
    expect(seen).toEqual(['0', '1', '2']);
  });
});
