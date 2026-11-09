/**
 * POST /api/chat 的 SSE 分帧器。
 *
 * 服务端 v1 只发送单行 `data:`，但客户端仍按规范支持：
 * - CRLF / LF / 单独 CR 换行，且换行可跨 chunk；
 * - 跨 chunk 的多字节 UTF-8（TextDecoder stream:true）；
 * - 多行 data（以 \n 拼接）；
 * - `: ping` 注释心跳；
 * - 空行分隔帧；半帧保留到下一个 chunk。
 */

export interface SseFrame {
  event: string;
  data: string;
}

export class SseParser {
  private buffer = '';
  private eventName = '';
  private dataLines: string[] = [];
  private readonly decoder = new TextDecoder('utf-8');

  /** 喂入原始字节；返回本次已完整的帧。 */
  push(chunk: Uint8Array): SseFrame[] {
    return this.consume(this.decoder.decode(chunk, { stream: true }));
  }

  /** 测试与边界场景：直接喂入已解码文本。 */
  pushText(text: string): SseFrame[] {
    return this.consume(text);
  }

  /** 输入结束：冲刷解码器、残留行与最后一个未以空行结束的帧。 */
  end(): SseFrame[] {
    const frames = this.consume(this.decoder.decode());
    let rest = this.buffer;
    this.buffer = '';
    if (rest.endsWith('\r')) {
      rest = rest.slice(0, -1);
    }
    if (rest.length > 0) {
      const frame = this.handleLine(rest);
      if (frame) frames.push(frame);
    }
    const tail = this.handleLine('');
    if (tail) frames.push(tail);
    return frames;
  }

  private consume(text: string): SseFrame[] {
    this.buffer += text;
    const frames: SseFrame[] = [];
    for (;;) {
      const lf = this.buffer.indexOf('\n');
      const cr = this.buffer.indexOf('\r');
      if (lf === -1 && cr === -1) {
        break;
      }

      let index: number;
      let width: number;
      if (cr === -1 || (lf !== -1 && lf < cr)) {
        index = lf;
        width = 1;
      } else if (cr + 1 < this.buffer.length) {
        index = cr;
        width = this.buffer[cr + 1] === '\n' ? 2 : 1;
      } else {
        // chunk 以 \r 结尾：可能是 \r\n 的前半段，等下一个 chunk。
        break;
      }

      const line = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + width);
      const frame = this.handleLine(line);
      if (frame) frames.push(frame);
    }
    return frames;
  }

  private handleLine(line: string): SseFrame | null {
    if (line === '') {
      if (this.dataLines.length === 0) {
        this.eventName = '';
        return null;
      }
      const frame: SseFrame = {
        event: this.eventName || 'message',
        data: this.dataLines.join('\n'),
      };
      this.eventName = '';
      this.dataLines = [];
      return frame;
    }

    if (line.startsWith(':')) {
      // 注释 / 心跳。
      return null;
    }

    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) {
      value = value.slice(1);
    }

    if (field === 'event') {
      this.eventName = value;
    } else if (field === 'data') {
      this.dataLines.push(value);
    }
    // id / retry 在 v1 中不使用，忽略。
    return null;
  }
}

/** 读取 fetch 响应体并逐帧产出。取消由外层 AbortController 负责。 */
export async function* readSseStream(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SseFrame, void, void> {
  const reader = body.getReader();
  const parser = new SseParser();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (value) {
        for (const frame of parser.push(value)) {
          yield frame;
        }
      }
    }
    for (const frame of parser.end()) {
      yield frame;
    }
  } finally {
    reader.releaseLock();
  }
}
