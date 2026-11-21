import { describe, expect, it } from 'vitest';

import { PERMS, type Permission } from '../../../lib/rbacPermissions';
// monitorEmptyDescription 为 MonitorPage 的导出纯函数（仅作单测钩子）：
// 无 asset:write / monitor:manage 时不得把用户引向无法执行的「添加资产并开启采集」。
import {
  collectorSummaryStatus,
  metricCardText,
  metricCardValueLabel,
  metricPlatform,
  metricValueLabel,
  monitorAssetEmptyVisible,
  monitorEmptyDescription,
  resolveCollectorConfig,
} from '../MonitorPage';

const OPERATOR_PERMS: Permission[] = [
  PERMS.chat,
  PERMS.assetsRead,
  PERMS.connectionTest,
  PERMS.execRead,
  PERMS.execWrite,
  PERMS.metricsRead,
  PERMS.inspections,
  PERMS.terminal,
];

const ADMIN_PERMS = Object.values(PERMS) as Permission[];

describe('monitorEmptyDescription（监控空态文案）', () => {
  it('运维工程师（只有 asset:read/monitor:read）改为联系管理员', () => {
    const text = monitorEmptyDescription(OPERATOR_PERMS);
    expect(text).toBe('暂无服务器指标：请联系管理员配置资产与采集');
    expect(text).not.toContain('添加资产并开启采集');
  });

  it('审计员/审批者空集合同样不引导去添加资产', () => {
    for (const permissions of [[PERMS.auditRead], [PERMS.assetsRead]]) {
      expect(monitorEmptyDescription(permissions)).toContain('请联系管理员');
    }
  });

  it('管理员（asset:write + monitor:manage）保留原 CTA 文案', () => {
    expect(monitorEmptyDescription(ADMIN_PERMS)).toBe(
      '暂无服务器指标：请先在「服务器」页添加资产并开启采集',
    );
  });

  it('仅有 asset:write 或仅有 monitor:manage 时不再谎称两项都能做', () => {
    expect(monitorEmptyDescription([PERMS.assetsWrite])).toBe(
      '暂无服务器指标：请联系管理员配置资产与采集',
    );
    expect(monitorEmptyDescription([PERMS.alertsWrite])).toBe(
      '暂无服务器指标：请联系管理员配置资产与采集',
    );
  });

  it('权限未加载完成（空集合）按无权限处理', () => {
    expect(monitorEmptyDescription([])).toBe('暂无服务器指标：请联系管理员配置资产与采集');
  });
});

describe('monitorAssetEmptyVisible（资产空态渲染条件）', () => {
  const idle = { isLoading: false, isError: false };

  it('无选中服务器且 overview 正常返回空列表时渲染资产空态', () => {
    expect(monitorAssetEmptyVisible(null, idle)).toBe(true);
  });

  it('overview 加载中不渲染，避免闪一帧「请先添加资产」', () => {
    expect(monitorAssetEmptyVisible(null, { isLoading: true, isError: false })).toBe(false);
  });

  it('overview 失败时不渲染资产空态（只保留错误横幅）', () => {
    // 走查第 5 轮 medium：接口 500 时 servers 为空，旧条件会再叠加一条
    // 「请先在服务器页添加资产并开启采集」，把故障误报成未配置。
    expect(monitorAssetEmptyVisible(null, { isLoading: false, isError: true })).toBe(false);
  });

  it('已有选中服务器时不渲染资产空态', () => {
    expect(monitorAssetEmptyVisible('srv-1', idle)).toBe(false);
  });
});

// 走查第 11 轮 medium：collector 接口慢/失败时概要卡曾用 `?? true` 显示
// 「运行中」；实际配置已暂停也会被误报，值班人员据此判断采集正常。
describe('collectorSummaryStatus（采集器概要卡状态）', () => {
  it('有真实配置时只看 enabled：暂停就是已暂停', () => {
    expect(collectorSummaryStatus({ enabled: false }, false, false)).toBe('paused');
    expect(collectorSummaryStatus({ enabled: true }, false, false)).toBe('running');
    // 即使查询本身报错，只要 overview 兜底给了配置，也必须显示真实状态。
    expect(collectorSummaryStatus({ enabled: false }, false, true)).toBe('paused');
  });

  it('配置未就绪且仍在加载：显示加载态而不是运行中', () => {
    expect(collectorSummaryStatus(null, true, false)).toBe('loading');
    expect(collectorSummaryStatus(undefined, true, false)).toBe('loading');
  });

  it('collector 与 overview 都结束且无配置：显示状态未知', () => {
    expect(collectorSummaryStatus(null, false, true)).toBe('unknown');
    expect(collectorSummaryStatus(null, true, true)).toBe('unknown');
    expect(collectorSummaryStatus(undefined, false, false)).toBe('unknown');
  });
});

// 走查第 13 轮 medium：跨会话暂停后，本会话的 collector 接口仍是首屏
// 快照「运行中」；overview 每 30s 刷新，必须优先。
describe('resolveCollectorConfig（采集配置取值顺序）', () => {
  it('overview 的新鲜配置优先于首屏 collector 快照', () => {
    expect(
      resolveCollectorConfig(
        { enabled: false, interval_seconds: 300 },
        { enabled: true, interval_seconds: 60 },
      ),
    ).toEqual({ enabled: false, interval_seconds: 300 });
    expect(
      resolveCollectorConfig(
        { enabled: true, interval_seconds: 600 },
        { enabled: false, interval_seconds: 60 },
      ),
    ).toEqual({ enabled: true, interval_seconds: 600 });
  });

  it('overview 尚未返回 / 无 collector 时回落到 collector 兜底', () => {
    expect(resolveCollectorConfig(undefined, { enabled: true, interval_seconds: 60 })).toEqual({
      enabled: true,
      interval_seconds: 60,
    });
    expect(resolveCollectorConfig(null, { enabled: false, interval_seconds: 30 })).toEqual({
      enabled: false,
      interval_seconds: 30,
    });
  });

  it('两者都没有时返回 null，由调用方显示「—」/「状态未知」', () => {
    expect(resolveCollectorConfig(undefined, undefined)).toBeNull();
    expect(resolveCollectorConfig(null, null)).toBeNull();
  });
});

// 走查第 15 轮 medium：Windows 资产在 /monitor 上曾按 Linux 口径标注
// （/proc/stat、/proc/meminfo、loadavg 1 分钟），而实际采集源是
// Win32_Processor LoadPercentage / Win32_OperatingSystem /
// ProcessorQueueLength。副文案必须随资产平台切换。
describe('metricCardText（指标卡平台口径）', () => {
  it('linux 保留 /proc 与 loadavg 口径', () => {
    expect(metricPlatform('linux')).toBe('linux');
    expect(metricCardText('cpu_pct', 'linux')).toEqual({
      title: 'CPU 使用率',
      subtitle: '/proc/stat 使用率（两次采样差分计算）',
    });
    expect(metricCardText('mem_pct', 'linux').subtitle).toBe('/proc/meminfo');
    expect(metricCardText('load_1', 'linux')).toEqual({
      title: '负载（1m）',
      subtitle: 'loadavg 1 分钟',
    });
  });

  it('windows 换成真实采集源，且不出现 /proc 与 loadavg', () => {
    expect(metricPlatform('windows')).toBe('windows');
    const cpu = metricCardText('cpu_pct', 'windows');
    expect(cpu.subtitle).toContain('Win32_Processor');
    expect(cpu.subtitle).toContain('LoadPercentage');
    const mem = metricCardText('mem_pct', 'windows');
    expect(mem.subtitle).toContain('Win32_OperatingSystem');
    const load = metricCardText('load_1', 'windows');
    expect(load.title).toBe('处理器队列长度');
    expect(load.subtitle).toContain('ProcessorQueueLength');
    expect(load.subtitle).toContain('不是 1 分钟负载均值');
    for (const key of ['cpu_pct', 'mem_pct', 'load_1'] as const) {
      const text = metricCardText(key, 'windows');
      expect(`${text.title} ${text.subtitle}`).not.toMatch(/\/proc|loadavg/);
    }
  });

  it('os 缺失时用平台中性文案，不冒充 /proc 口径', () => {
    expect(metricPlatform(undefined)).toBe('unknown');
    expect(metricPlatform(null)).toBe('unknown');
    expect(metricPlatform('network')).toBe('unknown');
    for (const key of ['cpu_pct', 'mem_pct', 'load_1'] as const) {
      const text = metricCardText(key, undefined);
      expect(`${text.title} ${text.subtitle}`).not.toMatch(/\/proc|loadavg|Win32/);
    }
    expect(metricCardText('load_1', undefined).title).toBe('负载 / 处理器队列');
  });
});

// 走查第 15 轮 medium：停采 12 小时但 24h 窗口内仍有历史点时，卡头
// 旧实现仍写「当前 42.0%」，与同页「已超过过期阈值 5 分钟」矛盾。
describe('metricValueLabel（卡头当前/最新）', () => {
  it('样本新鲜时仍写「当前」', () => {
    expect(metricValueLabel(42, false, '10 秒前')).toBe('当前');
  });

  it('样本过期时保留窗口最后一点但改称「最新（12 小时前）」', () => {
    expect(metricValueLabel(42, true, '12 小时前')).toBe('最新（12 小时前）');
    expect(metricValueLabel(42, true, null)).toBe('当前（已过期）');
  });

  it('窗口内无点时沿用「最新 + 年龄」，不再写「当前」', () => {
    expect(metricValueLabel(null, false, '72 小时前')).toBe('最新（72 小时前）');
    expect(metricValueLabel(null, true, null)).toBe('最新');
  });
});

// 走查第 17 轮 medium：latest 时钟超前（selectedSampleStale 被强制 false）
// 且窗口内还有 12 小时前的旧点时，卡头必须按旧点自己的年龄标「最新」，
// 不能复用 latest 的状态写成「当前」。
describe('metricCardValueLabel（窗口点优先于 overview.latest）', () => {
  it('窗口旧点 12 小时前：即使 latest 时钟超前也写「最新（12 小时前）」', () => {
    expect(metricCardValueLabel(42, true, '12 小时前', false, '时间超前服务端时钟 9 分钟')).toBe(
      '最新（12 小时前）',
    );
  });

  it('窗口点新鲜且未超前时仍写「当前」', () => {
    expect(metricCardValueLabel(42, false, '10 秒前', true, '12 小时前')).toBe('当前');
  });

  it('窗口内无点时才回落到 overview.latest 的超前摘要', () => {
    expect(metricCardValueLabel(null, false, null, false, '时间超前服务端时钟 9 分钟')).toBe(
      '最新（时间超前服务端时钟 9 分钟）',
    );
  });
});
