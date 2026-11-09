// FE-P2（六期）：CopyButton 降级与冷却逻辑（契约 8.2/8.7）。
import { describe, expect, it } from 'vitest';

import {
  COPY_COOLDOWN_MS,
  resolveCopyOutcome,
  selectableFallbackText,
  withinCooldown,
} from './copyButtonLogic';

describe('withinCooldown', () => {
  it('无成功记录时不冷却', () => {
    expect(withinCooldown(null, 1_000_000)).toBe(false);
  });

  it('成功后 1.5s 内点击被冷却', () => {
    expect(withinCooldown(10_000, 10_000 + COPY_COOLDOWN_MS - 1)).toBe(true);
    expect(withinCooldown(10_000, 10_000 + 0)).toBe(true);
  });

  it('超过 1.5s 后解除冷却', () => {
    expect(withinCooldown(10_000, 10_000 + COPY_COOLDOWN_MS)).toBe(false);
    expect(withinCooldown(10_000, 10_000 + 5_000)).toBe(false);
  });
});

describe('resolveCopyOutcome（降级链）', () => {
  it('clipboard API 成功即成功', () => {
    expect(resolveCopyOutcome(true, false)).toEqual({ ok: true });
    expect(resolveCopyOutcome(true, true)).toEqual({ ok: true });
  });

  it('clipboard 失败但 execCommand 降级成功仍算成功', () => {
    expect(resolveCopyOutcome(false, true)).toEqual({ ok: true });
  });

  it('两级都失败降级为可选中文本', () => {
    expect(resolveCopyOutcome(false, false)).toEqual({ ok: false, fallback: 'selectable' });
  });
});

describe('selectableFallbackText', () => {
  it('非空文本原样返回', () => {
    expect(selectableFallbackText('host:22')).toBe('host:22');
  });

  it('空文本不展示降级面板', () => {
    expect(selectableFallbackText('')).toBeNull();
  });
});
