// FE-P2（六期）：终端当前命令行提取（term/termLine.ts）。
import { describe, expect, it } from 'vitest';

import { cursorLineText, type TerminalBufferLike } from '../termLine';

function bufferOf(lines: string[], cursorY: number, viewportY = 0): TerminalBufferLike {
  return {
    cursorY,
    viewportY,
    getLine: (y: number) => {
      const text = lines[y];
      return text === undefined
        ? undefined
        : { translateToString: (trim: boolean) => (trim ? text.trimEnd() : text) };
    },
  };
}

describe('cursorLineText', () => {
  it('取光标行文本并去尾部填充空格', () => {
    const buffer = bufferOf(['uptime', 'df -h /tmp   '], 1);
    expect(cursorLineText(buffer)).toBe('df -h /tmp');
  });

  it('viewportY 参与绝对行换算（光标行随视口滚动解析到不同缓冲行）', () => {
    const buffer = bufferOf(['old1', 'old2', 'kubectl get pods'], 2, 0);
    expect(cursorLineText(buffer)).toBe('kubectl get pods');
    // 视口滚到顶部、光标落在视口第 2 行：绝对行 = cursorY(1) + viewportY(1)。
    const scrolled = { ...buffer, cursorY: 1, viewportY: 1 };
    expect(cursorLineText(scrolled)).toBe('kubectl get pods');
    // 未滚动时同一 cursorY 解析到另一缓冲行。
    const top = { ...buffer, cursorY: 1, viewportY: 0 };
    expect(cursorLineText(top)).toBe('old2');
  });

  it('行不存在（越界）返回空串', () => {
    expect(cursorLineText(bufferOf([], 0))).toBe('');
    expect(cursorLineText(bufferOf(['only'], 5))).toBe('');
  });

  it('负绝对行返回空串', () => {
    expect(cursorLineText(bufferOf(['x'], -1, 0))).toBe('');
  });

  it('translateToString 抛错时兜底空串', () => {
    const buffer: TerminalBufferLike = {
      cursorY: 0,
      viewportY: 0,
      getLine: () => ({
        translateToString: () => {
          throw new Error('disposed');
        },
      }),
    };
    expect(cursorLineText(buffer)).toBe('');
  });
});
