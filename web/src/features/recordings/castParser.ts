// F1：asciicast v2（NDJSON）流式解析（CONTRACT.md 8.0：自研，不引 asciinema-player）。
//
// 格式（契约 5.13 录制器写死）：
//   首行 header {"version":2,"width":cols,"height":rows}
//   随后事件行 [t, "o", "<输出文本>"] / [t, "r", "COLSxROWS"]（t 为 float 秒）
//   不含 i/m 事件；遇到时忽略。
//
// 大文件不整块载入：loadCastFromResponse 用 body.getReader() 逐块解码，
// CastParser 维护跨 chunk 的行缓冲，避免一次性 response.text() 的峰值内存
// 与巨型字符串；解析出的事件对象数组保留在内存（回放 seek 需要随机访问）。

export interface CastHeader {
  version: number;
  width: number;
  height: number;
}

export interface CastOutputEvent {
  kind: 'o';
  time: number;
  data: string;
}

export interface CastResizeEvent {
  kind: 'r';
  time: number;
  cols: number;
  rows: number;
}

export type CastEvent = CastOutputEvent | CastResizeEvent;

export interface CastParseResult {
  header: CastHeader | null;
  /** 文件序（t 单调不减，按录制器写入顺序）。 */
  events: CastEvent[];
  /** 最大事件时间戳（秒）；无事件时为 0。进度条基于该值（契约 8.7）。 */
  maxTime: number;
  /** 无法解析的事件行数（容错跳过，不中断）。 */
  badLines: number;
}

/** "COLSxROWS" → {cols, rows}；不合法返回 null。 */
export function parseResizeData(data: string): { cols: number; rows: number } | null {
  const match = /^(\d+)x(\d+)$/.exec(data.trim());
  if (!match) {
    return null;
  }
  const cols = Number(match[1]);
  const rows = Number(match[2]);
  if (!Number.isInteger(cols) || !Number.isInteger(rows) || cols <= 0 || rows <= 0) {
    return null;
  }
  return { cols, rows };
}

function parseHeaderLine(line: string): CastHeader | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return null;
  }
  const value = parsed as Record<string, unknown>;
  if (value.version !== 2) {
    return null;
  }
  const width = Number(value.width);
  const height = Number(value.height);
  if (!Number.isInteger(width) || !Number.isInteger(height) || width <= 0 || height <= 0) {
    return null;
  }
  return { version: 2, width, height };
}

/** 哨兵：行可识别但事件类型被忽略（i/m）。 */
const IGNORED_EVENT = Symbol('cast-ignored-event');

function parseEventLine(line: string): CastEvent | typeof IGNORED_EVENT | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  if (!Array.isArray(parsed) || parsed.length < 3) {
    return null;
  }
  const time = Number(parsed[0]);
  const kind = parsed[1];
  const data = parsed[2];
  if (!Number.isFinite(time) || time < 0 || typeof data !== 'string') {
    return null;
  }
  if (kind === 'o') {
    return { kind: 'o', time, data };
  }
  if (kind === 'r') {
    const size = parseResizeData(data);
    if (!size) {
      return null;
    }
    return { kind: 'r', time, cols: size.cols, rows: size.rows };
  }
  if (kind === 'i' || kind === 'm') {
    return IGNORED_EVENT; // 契约录制器不写 i/m：外部文件中的输入/标记跳过不计坏行
  }
  return null;
}

/**
 * 增量 NDJSON 解析器：feed 任意切块文本（可跨行/跨多字节 UTF-8 由调用方
 * 的 stream decoder 保证），end 收尾（冲刷最后半行）。
 */
export class CastParser {
  private buffer = '';
  private header: CastHeader | null = null;
  private readonly events: CastEvent[] = [];
  private badLines = 0;
  private finished = false;

  feed(chunk: string): void {
    if (this.finished) {
      return;
    }
    this.buffer += chunk;
    let newlineIndex = this.buffer.indexOf('\n');
    while (newlineIndex >= 0) {
      const line = this.buffer.slice(0, newlineIndex).replace(/\r$/, '');
      this.buffer = this.buffer.slice(newlineIndex + 1);
      this.consumeLine(line);
      newlineIndex = this.buffer.indexOf('\n');
    }
  }

  end(): CastParseResult {
    if (!this.finished) {
      const rest = this.buffer.replace(/\r$/, '');
      this.buffer = '';
      if (rest.trim() !== '') {
        this.consumeLine(rest);
      }
      this.finished = true;
    }
    let maxTime = 0;
    for (const event of this.events) {
      if (event.time > maxTime) {
        maxTime = event.time;
      }
    }
    return {
      header: this.header,
      events: this.events.slice(),
      maxTime,
      badLines: this.badLines,
    };
  }

  private consumeLine(line: string): void {
    if (line.trim() === '') {
      return; // 空行（尾行/分隔）忽略，不计坏行
    }
    if (this.header === null) {
      const header = parseHeaderLine(line);
      if (header) {
        this.header = header;
        return;
      }
      this.badLines += 1;
      return;
    }
    const event = parseEventLine(line);
    if (event && event !== IGNORED_EVENT) {
      this.events.push(event);
      return;
    }
    if (event === IGNORED_EVENT) {
      return;
    }
    // header 之前若混入事件行（无 header 的文件）：首行按 header 尝试失败
    // 已计入坏行；这里保持简单容错计数。
    this.badLines += 1;
  }
}

/**
 * 流式加载 .cast：逐块读取响应体（不整块载入）；AbortSignal 供组件卸载
 * 中止下载。body 不可读时（异常环境）返回错误，不退化为整块读取。
 */
export async function loadCastFromResponse(
  response: Response,
  options?: { signal?: AbortSignal },
): Promise<CastParseResult> {
  if (!response.ok) {
    throw new Error(`recording fetch failed: HTTP ${response.status}`);
  }
  const body = response.body;
  if (!body) {
    throw new Error('recording response has no streamable body');
  }
  const parser = new CastParser();
  const reader = body.getReader();
  const decoder = new TextDecoder('utf-8');
  try {
    for (;;) {
      if (options?.signal?.aborted) {
        throw new DOMException('aborted', 'AbortError');
      }
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      parser.feed(decoder.decode(value, { stream: true }));
    }
    parser.feed(decoder.decode());
  } finally {
    reader.releaseLock();
  }
  return parser.end();
}
