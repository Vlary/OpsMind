import { describe, expect, it } from 'vitest';

import {
  containerStateColor,
  isManagedRuntime,
  runtimeBadgeOf,
  runtimeBadgeTip,
} from '../runtimeBadge';

describe('runtimeBadgeOf（契约 8.7 容器页图例）', () => {
  it('五种运行时各有徽标：空串与 none 灰、docker/podman 品牌色、containerd 独立色', () => {
    expect(runtimeBadgeOf('')).toEqual({ label: '未探测', color: 'default' });
    expect(runtimeBadgeOf('none')).toEqual({ label: '无容器', color: 'default' });
    expect(runtimeBadgeOf('docker').label).toBe('Docker');
    expect(runtimeBadgeOf('podman').label).toBe('Podman');
    expect(runtimeBadgeOf('containerd').label).toBe('containerd');
    // docker/podman 用品牌色 Tag，不是灰默认。
    expect(runtimeBadgeOf('docker').color).not.toBe('default');
    expect(runtimeBadgeOf('podman').color).not.toBe('default');
  });

  it('未知值防御回落「未探测」', () => {
    expect(runtimeBadgeOf('crio').label).toBe('未探测');
  });
});

describe('isManagedRuntime（契约 2.9 约定 2：管理面仅 docker/podman）', () => {
  it('docker/podman 可管理；空串/none/containerd 不可', () => {
    expect(isManagedRuntime('docker')).toBe(true);
    expect(isManagedRuntime('podman')).toBe(true);
    expect(isManagedRuntime('')).toBe(false);
    expect(isManagedRuntime('none')).toBe(false);
    expect(isManagedRuntime('containerd')).toBe(false);
  });
});

describe('runtimeBadgeTip', () => {
  it('空串与 containerd 说明探测与仅展示语义', () => {
    expect(runtimeBadgeTip('')).toContain('尚未探测');
    expect(runtimeBadgeTip('containerd')).toContain('不支持容器管理面');
    expect(runtimeBadgeTip('docker')).toContain('docker');
  });
});

describe('containerStateColor（契约 8.7：running 绿、exited 灰、paused 黄、其他默认）', () => {
  it('running 绿、exited 灰、paused 黄', () => {
    expect(containerStateColor('running')).toBe('green');
    expect(containerStateColor('exited')).toBe('default');
    expect(containerStateColor('paused')).toBe('gold');
  });

  it('created/restarting 等运行时原值默认色', () => {
    expect(containerStateColor('created')).toBe('default');
    expect(containerStateColor('restarting')).toBe('default');
  });
});
