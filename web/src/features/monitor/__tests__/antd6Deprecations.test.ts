// antd 6 弃用属性门禁（走查第 4 轮 low）。
//
// antd 的 `warning.deprecated` 只在开发构建（process.env.NODE_ENV !==
// 'production'）里执行；E2E 跑的是 vite preview 的生产产物，浏览器断言
// 永远看不到这些告警。这里改用源码静态扫描兜底：新增/回退到下列弃用写法
// 时单测直接红，保证 /monitor（以及其它页面）的 DevTools console 能作为
// 干净基线。
//
// 覆盖走查列出的五类迁移：
//   Statistic.valueStyle → styles.content
//   Drawer.width/height  → size
//   Space.direction      → orientation
//   Alert.message        → title
//   Spin.tip             → description

import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import ts from 'typescript';
import { describe, expect, it } from 'vitest';

const SRC_ROOT = fileURLToPath(new URL('../../..', import.meta.url));
// 防 OOM：源码树远小于该上限；超过即说明遍历跑飞，直接截断。
const MAX_FILES = 5_000;

const DEPRECATED_PROPS: Readonly<Record<string, readonly string[]>> = {
  Statistic: ['valueStyle'],
  Drawer: ['width', 'height'],
  Space: ['direction'],
  Alert: ['message'],
  Spin: ['tip'],
};

interface Violation {
  file: string;
  line: number;
  tag: string;
  attribute: string;
}

function collectTsxFiles(dir: string): string[] {
  const files: string[] = [];
  const pending = [dir];
  while (pending.length > 0 && files.length < MAX_FILES) {
    const current = pending.pop();
    if (!current) {
      break;
    }
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      if (files.length >= MAX_FILES) {
        break;
      }
      const full = path.join(current, entry.name);
      if (entry.isDirectory()) {
        if (!entry.name.startsWith('.') && entry.name !== 'node_modules') {
          pending.push(full);
        }
      } else if (entry.name.endsWith('.tsx')) {
        files.push(full);
      }
    }
  }
  return files;
}

function findViolations(file: string): Violation[] {
  const text = readFileSync(file, 'utf8');
  if (!text.includes('antd')) {
    return [];
  }
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const violations: Violation[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isJsxElement(node) || ts.isJsxSelfClosingElement(node)) {
      const opening = ts.isJsxElement(node) ? node.openingElement : node;
      const tag = opening.tagName.getText(source);
      const deprecated = DEPRECATED_PROPS[tag];
      if (deprecated) {
        for (const attribute of opening.attributes.properties) {
          if (!ts.isJsxAttribute(attribute)) {
            continue;
          }
          const name = attribute.name.getText(source);
          if (deprecated.includes(name)) {
            violations.push({
              file: path.relative(SRC_ROOT, file),
              line: source.getLineAndCharacterOfPosition(attribute.getStart(source)).line + 1,
              tag,
              attribute: name,
            });
          }
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return violations;
}

describe('antd 6 弃用属性', () => {
  it('源码不再使用 Statistic.valueStyle / Drawer.width / Space.direction / Alert.message / Spin.tip', () => {
    const files = collectTsxFiles(SRC_ROOT);
    expect(files.length).toBeGreaterThan(0);
    expect(files.length).toBeLessThan(MAX_FILES);
    const violations = files.flatMap(findViolations);
    expect(
      violations,
      violations
        .map((item) => `${item.file}:${item.line} <${item.tag} ${item.attribute}>`)
        .join('\n'),
    ).toEqual([]);
  });
});
