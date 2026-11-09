import { describe, expect, it } from 'vitest';

import { readSseStream } from './sse';

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

// readSseStream：fetch 响应体 → 帧序列的异步生成器（chat 流式渲染入口）。
describe('readSseStream', () => {
  it('单个 chunk 内的多帧全部产出', async () => {
    const frames = await collect([
      'event: token\ndata: {"delta":"你"}\n\nevent: token\ndata: {"delta":"好"}\n\n',
    ]);
    expect(frames).toEqual([
      { event: 'token', data: '{"delta":"你"}' },
      { event: 'token', data: '{"delta":"好"}' },
    ]);
  });

  it('一帧跨多个 chunk 拼接后完整产出', async () => {
    const frames = await collect(['event: token\nda', 'ta: {"del', 'ta":"磁盘"}\n', '\n']);
    expect(frames).toEqual([{ event: 'token', data: '{"delta":"磁盘"}' }]);
  });

  it('流结束时未以空行收尾的尾帧被 end() 冲刷', async () => {
    const frames = await collect(['event: done\ndata: {"message_id":"m1"}']);
    expect(frames).toEqual([{ event: 'done', data: '{"message_id":"m1"}' }]);
  });

  it('空流产出空序列', async () => {
    expect(await collect([])).toEqual([]);
  });

  it('done 事件先于尾帧冲刷时保持输入顺序', async () => {
    const frames = await collect([
      'event: token\ndata: {"delta":"a"}\n\n',
      'event: done\ndata: {"message_id":"m2"}',
    ]);
    expect(frames.map((frame) => frame.event)).toEqual(['token', 'done']);
  });
});
