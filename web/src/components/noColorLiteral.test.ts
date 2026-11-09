import { readFileSync, readdirSync, statSync } from 'node:fs';
import { extname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// CONTRACT.md 8.3.14 F07：扫描 src/**/*.{ts,tsx,css} 与 index.html，除白名单
// 三文件外零颜色字面量。规则与 scripts/check-color-literals.mjs 完全一致
// （脚本管 CI，本用例管本地 `vitest run`）——两处同规则，避免「本地绿 CI 红」。
//
// 不用 ?raw：实测 vitest 下返回空串，断言会假通过。

const webDir = resolve(fileURLToPath(import.meta.url), '..', '..', '..');
const srcDir = resolve(webDir, 'src');
const indexHtml = resolve(webDir, 'index.html');

const ALLOWED = new Set(
  ['src/styles/tokens.css', 'src/styles/theme.ts', 'src/styles/palette.ts'].map((p) =>
    resolve(webDir, p),
  ),
);

const LITERAL_RE = /#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/;
const TEST_FILE_RE = /\.(test|spec)\.(ts|tsx)$/;
const SOURCE_EXT = new Set(['.ts', '.tsx', '.css']);

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      walk(full, out);
    } else {
      out.push(full);
    }
  }
  return out;
}

function scan(): string[] {
  const files = walk(srcDir).filter((f) => SOURCE_EXT.has(extname(f)) && !TEST_FILE_RE.test(f));
  files.push(indexHtml);
  const hits: string[] = [];
  for (const file of files) {
    if (ALLOWED.has(file)) {
      continue;
    }
    readFileSync(file, 'utf8')
      .split('\n')
      .forEach((line, i) => {
        if (LITERAL_RE.test(line)) {
          hits.push(`${relative(webDir, file).split(sep).join('/')}:${i + 1}: ${line.trim()}`);
        }
      });
  }
  return hits;
}

describe('颜色字面量门禁（8.3.1 第 1 条 / F07）', () => {
  it('白名单三文件之外的 src 与 index.html 零色值字面量', () => {
    expect(scan()).toEqual([]);
  });

  it('index.html 的 meta color-scheme 为 light dark（8.3.11 第 6 条）', () => {
    const html = readFileSync(indexHtml, 'utf8');
    expect(html).toMatch(/<meta\s+name="color-scheme"\s+content="light dark"\s*\/?>/);
    // 写死 dark 会让浏览器给浅色页面画深色滚动条与表单控件。
    expect(html).not.toMatch(/name="color-scheme"\s+content="dark"/);
  });

  it('index.html 在首绘前同步写入 data-theme（8.3.12 防 FOUC）', () => {
    const html = readFileSync(indexHtml, 'utf8');
    // 内联 script 必须位于 <body> 之前，且写 data-theme 与 data-theme-preference。
    const headEnd = html.indexOf('</head>');
    const scriptPos = html.indexOf("setAttribute('data-theme'");
    expect(scriptPos).toBeGreaterThan(0);
    expect(scriptPos).toBeLessThan(headEnd);
    expect(html).toContain("'opsmind.theme'");
    expect(html).toContain("setAttribute('data-theme-preference'");
    // 兜底：localStorage 抛异常时按 light（隐私模式）。
    expect(html).toMatch(/catch\s*\([^)]*\)\s*\{[^}]*pref\s*=\s*'light'/s);
  });

  it('index.html 不含 async/defer 的取主题脚本（不能等异步）', () => {
    const html = readFileSync(indexHtml, 'utf8');
    const head = html.slice(0, html.indexOf('</head>'));
    expect(head).not.toMatch(/<script[^>]*\basync\b/);
    expect(head).not.toMatch(/<script[^>]*\bdefer\b/);
  });
});
