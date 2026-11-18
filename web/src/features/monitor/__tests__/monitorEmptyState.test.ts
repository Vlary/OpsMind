import { describe, expect, it } from 'vitest';

import { PERMS, type Permission } from '../../../lib/rbacPermissions';
// monitorEmptyDescription 为 MonitorPage 的导出纯函数（仅作单测钩子）：
// 无 asset:write / monitor:manage 时不得把用户引向无法执行的「添加资产并开启采集」。
import { monitorEmptyDescription } from '../MonitorPage';

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
