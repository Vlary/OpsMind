// 采集配置卡的 Form 实例门禁（走查第 5 轮 low）。
//
// antd 的「Instance created by `useForm` is not connected to any Form
// element」只在开发构建里打印，E2E 跑的是 vite preview 生产产物，浏览器
// 断言看不到。这里用源码静态检查兜底：`Form.useForm()` 必须只出现在
// 仅在 canConfigure 时挂载的 CollectorConfigForm 子组件里，只读分支
// 不得创建 form 实例。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import ts from 'typescript';
import { describe, expect, it } from 'vitest';

const FILE = fileURLToPath(new URL('../CollectorCard.tsx', import.meta.url));

function functionBody(source: ts.SourceFile, name: string): string {
  let body = '';
  const visit = (node: ts.Node): void => {
    if (ts.isFunctionDeclaration(node) && node.name?.text === name) {
      body = node.body ? node.body.getText(source) : '';
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return body;
}

/** 收集 `Form.useForm(...)` 调用表达式的源码位置（注释里的文字不算）。 */
function useFormCalls(source: ts.SourceFile): number[] {
  const offsets: number[] = [];
  const visit = (node: ts.Node): void => {
    if (
      ts.isCallExpression(node) &&
      ts.isPropertyAccessExpression(node.expression) &&
      ts.isIdentifier(node.expression.expression) &&
      node.expression.expression.text === 'Form' &&
      node.expression.name.text === 'useForm'
    ) {
      offsets.push(node.getStart(source));
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return offsets;
}

describe('CollectorCard 的 useForm 连接', () => {
  it('只在可写子组件 CollectorConfigForm 内创建 form 实例', () => {
    const text = readFileSync(FILE, 'utf8');
    const source = ts.createSourceFile(FILE, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);

    const cardBody = functionBody(source, 'CollectorCard');
    const formBody = functionBody(source, 'CollectorConfigForm');
    expect(cardBody, 'CollectorCard 函数体应存在').not.toBe('');
    expect(formBody, 'CollectorConfigForm 函数体应存在').not.toBe('');

    // 只读分支也在 CollectorCard 内；一旦把 useForm 移回顶层，这条就红。
    expect(cardBody).not.toContain('Form.useForm');
    expect(formBody).toContain('Form.useForm');
    // 全文件只有一处调用：子组件挂载次数与 form 实例一一对应。
    const calls = useFormCalls(source);
    expect(calls).toHaveLength(1);
    // 该调用必须落在 CollectorConfigForm 的函数体区间内。
    const formStart = text.indexOf('function CollectorConfigForm');
    expect(formStart).toBeGreaterThanOrEqual(0);
    expect(calls[0] ?? -1).toBeGreaterThan(formStart);
  });

  it('可写子组件仅在 canConfigure 分支渲染', () => {
    const text = readFileSync(FILE, 'utf8');
    expect(text).toMatch(/canConfigure \? \(\s*<CollectorConfigForm\b/);
  });
});
