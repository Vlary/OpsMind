// FE-P2（六期，契约 8.7 ⑩）：终端页「当前输入行回显的命令」复制。
//
// xterm 光标行文本提取：buffer.active.cursorY 是视口相对行号，绝对行 =
// cursorY + viewportY；getLine(y)?.translateToString(true) 去尾部填充空格。
// 结构化最小接口（不 import @xterm/xterm 类型，纯函数可在 node 单测）。

/** xterm buffer 的最小结构面（Terminal.buffer.active）。 */
export interface TerminalBufferLike {
  cursorY: number;
  viewportY: number;
  getLine(y: number): { translateToString(trimRight: boolean): string } | undefined;
}

/**
 * 取光标所在行文本（当前命令回显行）；buffer 越界/未就绪返回空串。
 */
export function cursorLineText(buffer: TerminalBufferLike): string {
  const absolute = buffer.cursorY + buffer.viewportY;
  if (absolute < 0) {
    return '';
  }
  const line = buffer.getLine(absolute);
  if (!line) {
    return '';
  }
  try {
    return line.translateToString(true).trim();
  } catch {
    return '';
  }
}
