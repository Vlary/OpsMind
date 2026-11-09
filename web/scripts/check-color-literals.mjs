#!/usr/bin/env node
// 颜色字面量门禁（CONTRACT.md 8.3.1 第 1 条 / 8.3.9 末段）。
//
// 白名单只有三个文件可以出现色值字面量：
//   src/styles/tokens.css   —— CSS 唯一来源（浅色 + 深色两套）
//   src/styles/theme.ts     —— antd ThemeConfig 唯一来源
//   src/styles/palette.ts   —— xterm 主题与 ANSI 唯一来源
//
// 扫描 src/**/*.{ts,tsx,css} 与 index.html；注释里的字面量同样计入
// （脚本是正则扫描、不做 AST 判定，这是契约明确的口径）。
// *.test.ts / *.test.tsx 排除，理由见契约：测试需要断言色值本身。
//
// 与 check-bundle-size.mjs 同级：只用 node: 内置模块，无第三方依赖。

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { extname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const webDir = resolve(fileURLToPath(import.meta.url), '..', '..');
const srcDir = resolve(webDir, 'src');
const indexHtml = resolve(webDir, 'index.html');

const ALLOWED = new Set(
  ['src/styles/tokens.css', 'src/styles/theme.ts', 'src/styles/palette.ts'].map((p) =>
    resolve(webDir, p),
  ),
);

// 与契约 8.3.9 一致：#rgb / #rrggbb / #rrggbbaa、rgb()/rgba()/hsl()/hsla()。
const LITERAL_RE = /#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/;
const TEST_FILE_RE = /\.(test|spec)\.(ts|tsx)$/;
const SOURCE_EXT = new Set(['.ts', '.tsx', '.css']);

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) {
      walk(full, out);
    } else {
      out.push(full);
    }
  }
  return out;
}

const files = walk(srcDir).filter((f) => SOURCE_EXT.has(extname(f)) && !TEST_FILE_RE.test(f));
files.push(indexHtml);

const hits = [];
for (const file of files) {
  if (ALLOWED.has(file)) {
    continue;
  }
  const lines = readFileSync(file, 'utf8').split('\n');
  lines.forEach((line, i) => {
    if (LITERAL_RE.test(line)) {
      hits.push(`${relative(webDir, file).split(sep).join('/')}:${i + 1}: ${line.trim()}`);
    }
  });
}

if (hits.length > 0) {
  console.error(
    `color-literals: 发现 ${hits.length} 处硬编码色值（契约 8.3.1 只允许三个白名单文件）：`,
  );
  for (const hit of hits) {
    console.error(`  ${hit}`);
  }
  console.error(
    '改用语义变量（CSS: var(--color-*)，TS: styles/theme.ts 的 SEMANTIC_TOKENS / 图表用的 var(--color-chart-N)）。',
  );
  process.exit(1);
}

console.log(
  `color-literals: PASS（扫描 ${files.length} 个文件，白名单 ${ALLOWED.size} 个，命中 0）`,
);
