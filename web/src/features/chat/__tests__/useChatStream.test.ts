import { describe, expect, it } from 'vitest';

import { describeError, matchesLiveErrorTurn } from '../useChatStream';

// CONTRACT.md 5.8 第 7 步：失败时服务端落库“已收到的部分内容”，
// 可能包含前端 abort/断流后未收到的 token；识别同一条消息必须用前缀匹配，
// 否则 live 回合与持久化条目并存导致同一轮对话渲染两份。
describe('matchesLiveErrorTurn', () => {
  it('完全相等时匹配', () => {
    expect(matchesLiveErrorTurn('磁盘正常', '磁盘正常')).toBe(true);
  });

  it('落库内容多出前端未收到的 token 时仍匹配', () => {
    expect(matchesLiveErrorTurn('磁盘正常，但 inode', '磁盘正常')).toBe(true);
  });

  it('内容不一致时不匹配', () => {
    expect(matchesLiveErrorTurn('另一轮的回答', '磁盘正常')).toBe(false);
  });

  it('live.text 为空串时不再匹配非空内容：startsWith("") 恒为 true 会把历史 error 消息误判为本轮；只有落库内容同样为空（未收到任何 token 的断流回合）才匹配', () => {
    expect(matchesLiveErrorTurn('', '')).toBe(true);
    expect(matchesLiveErrorTurn('部分内容', '')).toBe(false);
    expect(matchesLiveErrorTurn('历史轮次的非空 error 内容', '')).toBe(false);
  });
});

// 第 6 轮打磨：工具路径错误码（ToolCallCard 复用 describeError 渲染）
// 必须有中文说明，审批通过后执行失败不再上屏裸英文码。
describe('describeError 工具路径错误码（第 6 轮）', () => {
  it('SSH 执行/超时/主机密钥错误码映射中文', () => {
    expect(describeError('SSH_EXEC_FAILED')).toBe('远程命令执行失败。');
    expect(describeError('SSH_TIMEOUT')).toBe('SSH 命令执行超时。');
    expect(describeError('SSH_HOST_KEY_CHANGED')).toBe('服务器主机密钥已变化，连接被阻止。');
  });

  it('资产不存在与 SQL 工具错误码映射中文', () => {
    expect(describeError('SERVER_NOT_FOUND')).toBe('服务器不存在或已被删除。');
    expect(describeError('DATABASE_NOT_FOUND')).toBe('数据库不存在或已被删除。');
    expect(describeError('SQL_QUERY_FAILED')).toBe('SQL 查询执行失败。');
    expect(describeError('SQL_TIMEOUT')).toBe('SQL 查询执行超时。');
  });

  it('参数与命令错误码映射中文', () => {
    expect(describeError('TOOL_ARGS_INVALID')).toBe('工具调用参数不合法。');
    expect(describeError('TOOL_NOT_FOUND')).toBe('调用了不支持的工具。');
    expect(describeError('COMMAND_INVALID')).toBe('命令不合法，未执行。');
  });

  it('既有兜底行为不受影响：HTTP_ 前缀、fallback、未知码', () => {
    expect(describeError('HTTP_502')).toBe('请求失败（HTTP 502）。');
    expect(describeError('SOME_NEW_CODE', '自定义兜底')).toBe('自定义兜底');
    expect(describeError('SOME_NEW_CODE')).toBe('SOME_NEW_CODE');
  });
});
