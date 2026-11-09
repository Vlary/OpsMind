// M4 五期：告警规则表单值与 API payload 的纯转换逻辑（node 直测，
// __tests__/ruleForm.test.ts）。阈值范围随指标切换（百分比 0..100、
// load/网络 ≥0），与后端 VALIDATION_FAILED 口径一致（S22②）。

import type { components } from '../../api/schema';

export type AlertMetricName = components['schemas']['AlertMetricName'];
export type AlertSeverity = components['schemas']['AlertSeverity'];
export type AlertComparator = 'gt' | 'lt';

export interface RuleFormValues {
  name: string;
  metric: AlertMetricName;
  comparator: AlertComparator;
  threshold: number;
  duration_windows: number;
  cooldown_seconds: number;
  severity: AlertSeverity;
  silent_until?: string | null;
  clear_silent_until?: boolean;
  notify_channel_id?: string | null;
  clear_notify_channel?: boolean;
  enabled: boolean;
}

const PCT_METRICS: ReadonlySet<AlertMetricName> = new Set(['cpu_pct', 'mem_pct', 'disk_pct']);

export function isPercentMetric(metric: AlertMetricName): boolean {
  return PCT_METRICS.has(metric);
}

/** 指标可读名（表格/表单共用）。 */
export const METRIC_LABELS: Record<AlertMetricName, string> = {
  cpu_pct: 'CPU 使用率',
  mem_pct: '内存使用率',
  disk_pct: '磁盘使用率',
  load_1: '负载（1m）',
  net_in_bps: '网络入速率',
  net_out_bps: '网络出速率',
};

export const SEVERITY_LABELS: Record<AlertSeverity, string> = {
  info: '信息',
  warning: '警告',
  critical: '严重',
};

/**
 * 系统哨兵规则的展示名（六期打磨 sprint 4 走查 low）。
 *
 * 「采集不可达」规则由 monitor collector 自动创建（`internal/monitor/
 * collector.go` 的 `suspendRuleName`，enabled=false、不参与评估，只承载
 * 连续采集失败后的暂停事件）。它的 name 是数据库键（唯一名 + 前端按名
 * 识别 + 用户不可改名），必须保持英文原样；UI 上按名映射中文显示，
 * 原始名放进 Tooltip 供排查。
 */
export const SUSPEND_RULE_NAME = 'monitor.collector.unreachable';

const RULE_DISPLAY_NAMES: Record<string, string> = {
  [SUSPEND_RULE_NAME]: '采集不可达（系统）',
};

/** 规则名 → UI 展示名；未知规则名原样返回（不吞用户自定义名）。 */
export function ruleDisplayName(name: string): string {
  return RULE_DISPLAY_NAMES[name] ?? name;
}

export function metricThresholdBounds(metric: AlertMetricName): {
  min: number;
  max: number | null;
} {
  if (isPercentMetric(metric)) {
    return { min: 0, max: 100 };
  }
  return { min: 0, max: null };
}

/** 表单值 → 创建 payload（silent_until 空串→null）。 */
export function toCreatePayload(values: RuleFormValues): components['schemas']['AlertRuleCreate'] {
  const payload: components['schemas']['AlertRuleCreate'] = {
    name: values.name.trim(),
    metric: values.metric,
    comparator: values.comparator,
    threshold: values.threshold,
    duration_windows: values.duration_windows,
    cooldown_seconds: values.cooldown_seconds,
    severity: values.severity,
    enabled: values.enabled,
  };
  if (values.notify_channel_id) {
    payload.notify_channel_id = values.notify_channel_id;
  }
  if (values.silent_until) {
    payload.silent_until = values.silent_until;
  }
  return payload;
}

/**
 * 表单值 → 更新 payload：静默/渠道的清空语义走显式布尔（AlertRuleUpdate
 * 的 clear_silent_until / clear_notify_channel），避免 null 与「不修改」
 * 的歧义（契约 3.3 AlertRuleUpdate）。
 */
export function toUpdatePayload(values: RuleFormValues): components['schemas']['AlertRuleUpdate'] {
  const payload: components['schemas']['AlertRuleUpdate'] = {
    name: values.name.trim(),
    metric: values.metric,
    comparator: values.comparator,
    threshold: values.threshold,
    duration_windows: values.duration_windows,
    cooldown_seconds: values.cooldown_seconds,
    severity: values.severity,
    enabled: values.enabled,
  };
  if (values.clear_notify_channel) {
    payload.clear_notify_channel = true;
  } else if (values.notify_channel_id) {
    payload.notify_channel_id = values.notify_channel_id;
  }
  if (values.clear_silent_until) {
    payload.clear_silent_until = true;
  } else if (values.silent_until) {
    payload.silent_until = values.silent_until;
  }
  return payload;
}

/** 阈值随指标切换越界时钳回合法域（表单联动辅助）。 */
export function clampThreshold(metric: AlertMetricName, threshold: number): number {
  const bounds = metricThresholdBounds(metric);
  if (Number.isNaN(threshold)) {
    return bounds.min;
  }
  if (threshold < bounds.min) {
    return bounds.min;
  }
  if (bounds.max !== null && threshold > bounds.max) {
    return bounds.max;
  }
  return threshold;
}
