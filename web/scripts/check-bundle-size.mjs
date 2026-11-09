#!/usr/bin/env node
// 首屏 JS gzip 体积门禁（README 8.7 预算的 CI 化，已知限制 19 的修复）。
//
// 口径与 README 8.7 的人工测量命令完全一致：`web/dist/index.html` 直接以
// <script type="module"> 或 <link rel="modulepreload"> 引用的 JS chunk，
// gzip 后字节总和 ≤ 150KB（150 * 1024 = 153,600 B）。
//
//   cd web/dist && for f in $(grep -oE 'assets/[A-Za-z0-9._-]+\.js' index.html \
//     | sort -u); do gzip -c "$f" | wc -c; done | paste -sd+ | bc
//
// 用 node:zlib（默认 level 6，与 gzip CLI 一致）替代 shell 管道，便于 CI
// 输出逐 chunk 明细。在 `pnpm -C web build` 之后运行。

import { readFileSync } from 'node:fs';
import { relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const webDir = resolve(fileURLToPath(import.meta.url), '..', '..');
const distDir = resolve(webDir, 'dist');
const indexHtml = readFileSync(resolve(distDir, 'index.html'), 'utf8');

// README 8.7：预算 ≤ 150KB。
const BUDGET_BYTES = 150 * 1024;

const chunks = [...new Set(indexHtml.match(/assets\/[A-Za-z0-9._-]+\.js/g) ?? [])];
if (chunks.length === 0) {
  console.error('bundle-size: index.html 中未找到任何 assets/*.js 引用（build 是否成功？）');
  process.exit(1);
}

let total = 0;
const rows = [];
for (const chunk of chunks) {
  const raw = readFileSync(resolve(distDir, chunk));
  const gz = gzipSync(raw).length;
  total += gz;
  rows.push({ chunk, gz, raw: raw.length });
}

rows.sort((a, b) => b.gz - a.gz);
console.log('首屏 JS chunk（gzip / raw）：');
for (const row of rows) {
  console.log(`  ${relative(distDir, resolve(distDir, row.chunk))}  ${row.gz} B / ${row.raw} B`);
}
console.log(
  `合计：${total} B（gzip）≈ ${(total / 1024).toFixed(1)} KiB；预算 ≤ ${BUDGET_BYTES} B（150KB）`,
);

if (total > BUDGET_BYTES) {
  console.error(
    `bundle-size: 超出首屏 JS gzip 预算 ${total - BUDGET_BYTES} B。` +
      '首屏只应包含入口与 react-vendor 等框架层；新增依赖请下沉到懒加载边界' +
      '（参考 src/App.tsx 的 AntdAppRoot 懒加载注释与 vite.config.ts 分包说明）。',
  );
  process.exit(1);
}
console.log('bundle-size: PASS');
