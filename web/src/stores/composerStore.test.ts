import { afterEach, describe, expect, it } from 'vitest';

import { useComposerStore } from './composerStore';

// 输入草稿 store（CONTRACT.md 8.5：只存未发送文本，不持久化）。
// 直接驱动 zustand store 的状态机；每个用例后复位，保持隔离。

afterEach(() => {
  useComposerStore.setState({ drafts: {} });
});

describe('useComposerStore 草稿状态机', () => {
  it('初始状态无任何草稿', () => {
    expect(useComposerStore.getState().drafts).toEqual({});
  });

  it('setDraft 写入指定会话的草稿', () => {
    useComposerStore.getState().setDraft('c1', '检查磁盘');
    expect(useComposerStore.getState().drafts['c1']).toBe('检查磁盘');
  });

  it('同一会话再次 setDraft 覆盖旧草稿', () => {
    const store = useComposerStore.getState();
    store.setDraft('c1', '第一版');
    store.setDraft('c1', '第二版');
    expect(useComposerStore.getState().drafts['c1']).toBe('第二版');
  });

  it('不同会话草稿互相隔离', () => {
    const store = useComposerStore.getState();
    store.setDraft('c1', '会话一的草稿');
    store.setDraft('c2', '会话二的草稿');
    const drafts = useComposerStore.getState().drafts;
    expect(drafts['c1']).toBe('会话一的草稿');
    expect(drafts['c2']).toBe('会话二的草稿');
  });

  it('clearDraft 删除指定会话的草稿', () => {
    const store = useComposerStore.getState();
    store.setDraft('c1', '将被清除');
    store.setDraft('c2', '保留');
    store.clearDraft('c1');
    const drafts = useComposerStore.getState().drafts;
    expect('c1' in drafts).toBe(false);
    expect(drafts['c2']).toBe('保留');
  });

  it('clearDraft 不存在的会话无副作用', () => {
    useComposerStore.getState().setDraft('c1', 'x');
    useComposerStore.getState().clearDraft('not-exist');
    expect(useComposerStore.getState().drafts['c1']).toBe('x');
  });

  it('空字符串草稿照常存储（与“无草稿”区分）', () => {
    useComposerStore.getState().setDraft('c1', '');
    expect('c1' in useComposerStore.getState().drafts).toBe(true);
  });

  it('草稿保留多行与超长内容（不截断）', () => {
    const long = `第一行\n第二行\n${'长'.repeat(500)}`;
    useComposerStore.getState().setDraft('c1', long);
    expect(useComposerStore.getState().drafts['c1']).toBe(long);
  });
});
