// FE-R（八期，CONTRACT.md 9.4 F08）：回放页筛选 / 搜索 / 命中跳转 / 快捷键的
// 纯函数矩阵。契约要求的六组断言：
//  ① filterSteps 三组条件交集、空条件返回原数组（同一引用）；
//  ② 筛选后 visibleTotal / progress 以可见序列为准，replayProgress(0,1)=0；
//  ③ searchSteps 大小写不敏感，命中字段含 arguments/output/result_summary/
//     command/content；
//  ④ matchIndexes 升序，nextMatch/prevMatch 首尾环绕；
//  ⑤ hotkeyAction 矩阵：input 内全 null、Esc 无搜索无筛选为 null、/ 与 f 聚焦、
//     Shift+Arrow 区别于 Arrow；
//  ⑥ mergeFilterLabel 文案「已筛选：显示 M / N 步」与「第 k / K 个匹配」。

import { describe, expect, it } from 'vitest';

import type { ReplayStep } from '../replayPlayer';
import {
  EMPTY_REPLAY_FILTER,
  filterSteps,
  firstMatchExcerpt,
  hotkeyAction,
  isActivationTarget,
  isEditableTarget,
  isFilterEmpty,
  matchLabel,
  mergeFilterLabel,
  nextMatch,
  parseStepJump,
  prevMatch,
  replayProgress,
  searchSteps,
  stepSearchText,
  truncatedBanner,
  type ReplayFilter,
} from '../replayPlayer';

/** ReplayStep 的构造器：openapi 固定字段全集（未用字段为 null）。 */
function step(overrides: Partial<ReplayStep> & Pick<ReplayStep, 'seq' | 'kind'>): ReplayStep {
  return {
    timestamp: '2026-11-10T10:00:00Z',
    message_id: null,
    tool_call_id: null,
    approval_id: null,
    name: null,
    source: null,
    server_name: null,
    content: null,
    reasoning_duration_ms: null,
    command: null,
    original_command: null,
    arguments: null,
    risk_level: null,
    edited_risk_level: null,
    status: null,
    exit_code: null,
    output: null,
    error_code: null,
    approval_mode: null,
    duration_ms: null,
    result_summary: null,
    result_truncated: null,
    affected_servers: null,
    diagnosis_id: null,
    inspection_report_id: null,
    batch_job_id: null,
    ...overrides,
  };
}

const userStep = step({ seq: 1, kind: 'user_message', content: '检查磁盘水位' });
const reasoningStep = step({ seq: 2, kind: 'reasoning', content: '先采集事实，再执行 df -h' });
const sshToolStep = step({
  seq: 3,
  kind: 'tool_call',
  tool_call_id: 'tc-1',
  name: 'ssh_exec',
  arguments: { server_id: 'srv-1', command: 'df -h' },
  risk_level: 'needs_approval',
  status: 'succeeded',
  approval_mode: 'standard',
  result_summary: 'exit=0 · 128B',
});
const approvalStep = step({
  seq: 4,
  kind: 'approval',
  tool_call_id: 'tc-1',
  approval_id: 'ap-1',
  source: 'chat',
  command: 'df -h',
  risk_level: 'needs_approval',
  status: 'approved',
});
const execStep = step({
  seq: 5,
  kind: 'execution',
  tool_call_id: 'tc-1',
  name: 'ssh_exec',
  command: 'df -h',
  status: 'succeeded',
  exit_code: 0,
  output: 'Filesystem 40G 12G 26G 32%',
  result_summary: 'df 输出 1 行',
  result_truncated: false,
});
const localStep = step({
  seq: 6,
  kind: 'tool_call',
  tool_call_id: 'tc-2',
  name: 'list_servers',
  arguments: {},
  risk_level: 'safe',
  status: 'succeeded',
});
const assistantStep = step({ seq: 7, kind: 'assistant_text', content: '磁盘使用率 32%。' });

const ALL: ReplayStep[] = [
  userStep,
  reasoningStep,
  sshToolStep,
  approvalStep,
  execStep,
  localStep,
  assistantStep,
];

function filterOf(partial: Partial<ReplayFilter>): ReplayFilter {
  return { ...EMPTY_REPLAY_FILTER, ...partial };
}

describe('F08 ① filterSteps：三组条件交集', () => {
  it('空条件返回原数组（同一引用，不复制）', () => {
    expect(filterSteps(ALL, EMPTY_REPLAY_FILTER)).toBe(ALL);
    expect(isFilterEmpty(EMPTY_REPLAY_FILTER)).toBe(true);
  });

  it('kind 单选：只留该 kind', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['approval'] }));
    expect(visible.map((item) => item.seq)).toEqual([4]);
  });

  it('kind 多选：组内 OR', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['user_message', 'assistant_text'] }));
    expect(visible.map((item) => item.seq)).toEqual([1, 7]);
  });

  it('工具筛选只命中 tool_call / execution 步（同名的 approval 步不命中）', () => {
    const visible = filterSteps(ALL, filterOf({ tools: ['ssh_exec'] }));
    expect(visible.map((item) => item.seq)).toEqual([3, 5]);
  });

  it('工具筛选不误伤本地工具与无工具步', () => {
    const visible = filterSteps(ALL, filterOf({ tools: ['list_servers'] }));
    expect(visible.map((item) => item.seq)).toEqual([6]);
  });

  it('风险筛选只命中 tool_call / approval 步', () => {
    const visible = filterSteps(ALL, filterOf({ risks: ['needs_approval'] }));
    expect(visible.map((item) => item.seq)).toEqual([3, 4]);
    // safe 只命中 list_servers（execution 步的 risk_level 恒 null）。
    expect(filterSteps(ALL, filterOf({ risks: ['safe'] })).map((item) => item.seq)).toEqual([6]);
  });

  it('三组条件交集（组间 AND）', () => {
    const visible = filterSteps(
      ALL,
      filterOf({ kinds: ['tool_call'], tools: ['ssh_exec'], risks: ['needs_approval'] }),
    );
    expect(visible.map((item) => item.seq)).toEqual([3]);
  });

  it('交集为空返回空数组（不是原数组）', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['assistant_text'], tools: ['ssh_exec'] }));
    expect(visible).toEqual([]);
  });
});

describe('F08 ② 筛选后进度以可见序列为准', () => {
  it('可见序列长度与下标独立于原序列', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['user_message', 'execution'] }));
    expect(visible.length).toBe(2);
    // 可见序列的两步：首 0% / 末 100%。
    expect(replayProgress(0, visible.length)).toBe(0);
    expect(replayProgress(1, visible.length)).toBeCloseTo(100);
  });

  it('筛选后只剩一步：进度恒 0（不回归 0/0=NaN）', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['approval'] }));
    expect(visible.length).toBe(1);
    const value = replayProgress(0, visible.length);
    expect(Number.isNaN(value)).toBe(false);
    expect(value).toBe(0);
  });

  it('筛选后为空：进度恒 0', () => {
    const visible = filterSteps(ALL, filterOf({ kinds: ['execution'], tools: ['list_servers'] }));
    expect(visible.length).toBe(0);
    expect(replayProgress(0, visible.length)).toBe(0);
  });
});

describe('F08 ③ searchSteps：大小写不敏感，命中五个字段', () => {
  it('命中 content', () => {
    expect(searchSteps(ALL, '磁盘')).toEqual([0, 6]);
  });

  it('命中 command（大小写不敏感；reasoning 正文里的 df -h 也命中）', () => {
    expect(searchSteps(ALL, 'DF -H')).toEqual([1, 2, 3, 4]);
    expect(searchSteps(ALL, 'df -h')).toEqual([1, 2, 3, 4]);
  });

  it('命中 output', () => {
    expect(searchSteps(ALL, 'filesystem')).toEqual([4]);
  });

  it('命中 result_summary', () => {
    expect(searchSteps(ALL, '128b')).toEqual([2]);
  });

  it('命中 arguments（对象序列化后）', () => {
    expect(searchSteps(ALL, 'srv-1')).toEqual([2]);
    expect(stepSearchText(sshToolStep)).toContain('"server_id":"srv-1"');
  });

  it('空查询与纯空白查询返回空数组（不把全部步骤当命中）', () => {
    expect(searchSteps(ALL, '')).toEqual([]);
    expect(searchSteps(ALL, '   ')).toEqual([]);
  });

  it('命中下标升序', () => {
    const hits = searchSteps(ALL, 'e');
    expect(hits).toEqual([...hits].sort((a, b) => a - b));
  });

  it('无命中返回空数组', () => {
    expect(searchSteps(ALL, '不存在的字符串')).toEqual([]);
  });
});

describe('F08 ④ nextMatch / prevMatch 首尾环绕', () => {
  it('next 依次前进，末位回到首位', () => {
    expect(nextMatch(0, 3)).toBe(1);
    expect(nextMatch(1, 3)).toBe(2);
    expect(nextMatch(2, 3)).toBe(0);
  });

  it('prev 依次后退，首位回到末位', () => {
    expect(prevMatch(2, 3)).toBe(1);
    expect(prevMatch(1, 3)).toBe(0);
    expect(prevMatch(0, 3)).toBe(2);
  });

  it('零命中恒 0（调用方不会下标越界）', () => {
    expect(nextMatch(0, 0)).toBe(0);
    expect(prevMatch(5, 0)).toBe(0);
  });
});

describe('F08 ⑤ hotkeyAction 矩阵', () => {
  const base = { inInput: false };

  it('焦点在输入框内：全部返回 null（搜索框里打字不触发快捷键）', () => {
    for (const key of [' ', 'ArrowLeft', 'ArrowRight', 'Home', 'End', '/', 'f', 'Escape']) {
      expect(
        hotkeyAction(key, { inInput: true, shift: true, hasSearch: true, hasFilter: true }),
        key,
      ).toBeNull();
    }
  });

  it('空格 = 播放/暂停，Home/End = 首/末步', () => {
    expect(hotkeyAction(' ', base)).toBe('toggle-play');
    expect(hotkeyAction('Home', base)).toBe('first-step');
    expect(hotkeyAction('End', base)).toBe('last-step');
  });

  it('Arrow 是上下一步，Shift+Arrow 是上/下一个匹配', () => {
    expect(hotkeyAction('ArrowLeft', base)).toBe('prev-step');
    expect(hotkeyAction('ArrowRight', base)).toBe('next-step');
    expect(hotkeyAction('ArrowLeft', { ...base, shift: true })).toBe('prev-match');
    expect(hotkeyAction('ArrowRight', { ...base, shift: true })).toBe('next-match');
  });

  it('/ 与 f 聚焦搜索与筛选', () => {
    expect(hotkeyAction('/', base)).toBe('focus-search');
    expect(hotkeyAction('f', base)).toBe('focus-filter');
    expect(hotkeyAction('F', base)).toBe('focus-filter');
  });

  it('Esc：有搜索或有筛选才清空；纯净态返回 null', () => {
    expect(hotkeyAction('Escape', base)).toBeNull();
    expect(hotkeyAction('Escape', { ...base, hasSearch: true })).toBe('clear');
    expect(hotkeyAction('Escape', { ...base, hasFilter: true })).toBe('clear');
  });

  it('未映射的按键返回 null（不吞浏览器默认行为）', () => {
    expect(hotkeyAction('a', base)).toBeNull();
    expect(hotkeyAction('Tab', base)).toBeNull();
  });

  it('isActivationTarget：按钮/链接让位空格（方向键仍推进）', () => {
    // 模拟 DOM：closest 只在命中按钮/链接时返回节点，普通 div 返回 null。
    const fake = (hit: boolean) => ({ closest: () => (hit ? {} : null) }) as unknown as EventTarget;
    expect(isActivationTarget(fake(true))).toBe(true);
    expect(isActivationTarget(fake(false))).toBe(false);
    expect(isActivationTarget(null)).toBe(false);
  });

  it('isEditableTarget：input/textarea/select/contentEditable 让位', () => {
    const fake = (tagName: string, isContentEditable = false) =>
      ({ tagName, isContentEditable }) as unknown as EventTarget;
    expect(isEditableTarget(fake('INPUT'))).toBe(true);
    expect(isEditableTarget(fake('textarea'))).toBe(true);
    expect(isEditableTarget(fake('SELECT'))).toBe(true);
    expect(isEditableTarget(fake('DIV', true))).toBe(true);
    expect(isEditableTarget(fake('DIV'))).toBe(false);
    expect(isEditableTarget(null)).toBe(false);
  });
});

describe('F08 ⑥ mergeFilterLabel / matchLabel 文案', () => {
  it('筛选文案逐字「已筛选：显示 M / N 步」', () => {
    expect(mergeFilterLabel(3, 10)).toBe('已筛选：显示 3 / 10 步');
  });

  it('叠加命中文案（第 k / K 个匹配）', () => {
    expect(mergeFilterLabel(3, 10, { current: 0, count: 4 })).toBe(
      '已筛选：显示 3 / 10 步 · 第 1 / 4 个匹配',
    );
    expect(mergeFilterLabel(3, 10, { current: 3, count: 4 })).toContain('第 4 / 4 个匹配');
  });

  it('无命中不追加匹配段', () => {
    expect(mergeFilterLabel(3, 10, { current: 0, count: 0 })).toBe('已筛选：显示 3 / 10 步');
    expect(matchLabel(0, 0)).toBe('');
  });
});

describe('八期：搜索命中片段与步骤跳转', () => {
  it('firstMatchExcerpt 返回命中前后上下文并标出原文', () => {
    const excerpt = firstMatchExcerpt(execStep, '40G');
    expect(excerpt).not.toBeNull();
    expect(excerpt?.field).toBe('output');
    expect(excerpt?.hit).toBe('40G');
    expect(excerpt?.after).toContain('12G');
  });

  it('空查询与无命中都返回 null（不渲染空的命中块）', () => {
    expect(firstMatchExcerpt(execStep, '')).toBeNull();
    expect(firstMatchExcerpt(execStep, 'zzz')).toBeNull();
  });

  it('parseStepJump：1 基输入、越界钳制、非数字返回 null', () => {
    expect(parseStepJump('1', 7)).toBe(0);
    expect(parseStepJump('7', 7)).toBe(6);
    expect(parseStepJump('99', 7)).toBe(6);
    expect(parseStepJump('0', 7)).toBe(0);
    expect(parseStepJump('', 7)).toBeNull();
    expect(parseStepJump('abc', 7)).toBeNull();
    expect(parseStepJump('-2', 7)).toBeNull();
  });
});

describe('八期：truncatedBanner 四值文案', () => {
  it('steps / approvals / payload 三套文案互不相同，且都不复用 steps', () => {
    const steps = truncatedBanner('steps', 42);
    const approvals = truncatedBanner('approvals', 42);
    const payload = truncatedBanner('payload', 42);
    expect(steps).toBe('超出 20000 步上限，仅显示最近 42 步');
    expect(approvals).toBe('审批记录过多，仅显示最近 42 步（更早的审批未纳入回放）');
    expect(payload).toBe('会话内容过大，部分参数与输出未下发（仅保留最近 42 步的摘要）');
    expect(approvals).not.toContain('20000');
    expect(payload).not.toContain('20000');
  });

  it('未截断（空串/null/未知值）返回 null（不显示横幅）', () => {
    expect(truncatedBanner('', 10)).toBeNull();
    expect(truncatedBanner(null, 10)).toBeNull();
    expect(truncatedBanner(undefined, 10)).toBeNull();
    expect(truncatedBanner('weird', 10)).toBeNull();
  });
});
