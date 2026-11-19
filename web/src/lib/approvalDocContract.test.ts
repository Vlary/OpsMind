import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// 第 7 轮走查 low：docs/CONTRACT.md 5.18 与 api/openapi.yaml 仍写「审批被
// 批准且带改写时执行 approval.EditedCommand / edited_command」，与 12.3
// 第 7/8 条的「执行恒用 approval.command、edited_command 仅历史只读」冲突。
// 本用例锁住这几处旧口径不再回潮：文档与 OpenAPI 必须与代码
//（internal/batchx/execute.go、internal/httpapi/approvals.go）一致。
//
// 与 noColorLiteral.test.ts / themeTokens.test.ts 同模式：node:fs 直读原文
// （vitest 下 ?raw 返回空串，断言会假通过）。

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '..', '..', '..');

// docs/CONTRACT.md 是内部设计文档，不随开源仓库发布。缺失时跳过「契约文本」
// 断言，只保留 openapi.yaml 的断言——openapi 是发布物。本地有该文件，断言照常跑，
// 防回潮的保护不减弱。
function readIfExists(path: string): string {
  try {
    return readFileSync(path, 'utf8');
  } catch {
    return '';
  }
}

const contract = readIfExists(resolve(repoRoot, 'docs/CONTRACT.md'));
const openapi = readFileSync(resolve(repoRoot, 'api/openapi.yaml'), 'utf8');
const itContract = contract === '' ? it.skip : it;

describe('审批 edited_command 文档口径（00012 / 第 7 轮走查）', () => {
  itContract('CONTRACT.md 不再声称审批改写命令会被执行', () => {
    // 5.18 逐台分派旧句：OnApprovalDecided 执行 approval.EditedCommand。
    expect(contract).not.toContain('逐字节执行 `approval.EditedCommand` 非空时的改写后命令');
    // 6.2 SSE / decide 旧句：请求体可携带 edited_command、runner 执行改写命令。
    expect(contract).not.toContain('六期起 decide 请求体可携带 `edited_command`');
    expect(contract).not.toContain('runner 执行改写命令');
    // 5.18 工具表旧句：decide 改写按 2.11 约定 3。
    expect(contract).not.toContain('decide 改写按 2.11 约定 3');
    // 8.7 审批卡编辑旧段落：待审批态提供「编辑命令」并提交 edited_command。
    expect(contract).not.toContain('待审批态提供「编辑命令」进入编辑态');
  });

  itContract('CONTRACT.md 保留 12.3 的只读口径', () => {
    expect(contract).toContain('执行层无条件使用 `approvals.command`');
    expect(contract).toContain('执行层恒用 `approval.command`');
    expect(contract).toContain('edited_command` 仅历史只读');
  });

  it('openapi.yaml 的执行/SSE 描述恒用 approval.command', () => {
    expect(openapi).not.toContain('执行命令为 edited_command 非空时的改写后命令');
    expect(openapi).not.toContain('decide 批准时携带改写后命令则非 null（等待方按它执行）');
    expect(openapi).toContain('执行命令恒为 approval.command');
    expect(openapi).toContain('等待方执行恒以 approval.command 为准');
  });
});
