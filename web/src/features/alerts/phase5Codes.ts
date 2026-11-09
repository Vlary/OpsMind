// M4 五期：五期错误码中文映射（CONTRACT.md 9.6 错误码表的五期新增行）。
// 现有通用映射在 components/admin/apiError.ts 的 CODE_MESSAGES（一期口径，
// 映射内容不改动；sprint 4 起仅导出供本模块回落复用）；本模块在其上叠加
// 五期码，四页共用。
// 未知 code 直接显示 message（契约 9.6 末行），由 describeFailure 兜底。

import { CODE_MESSAGES, describeFailure, type ApiFailure } from '../../components/admin/apiError';

export const PHASE5_CODE_MESSAGES: Record<string, string> = {
  ALERT_RULE_NOT_FOUND: '告警规则不存在或已删除',
  ALERT_EVENT_NOT_FOUND: '告警事件不存在',
  NOTIFICATION_CHANNEL_NOT_FOUND: '通知渠道不存在或已删除',
  NOTIFICATION_CONFIG_INVALID: '通知渠道配置不合法（URL / 收件人等）',
  NOTIFICATION_TEST_FAILED: '通知渠道测试投递失败',
  DIAGNOSIS_SESSION_NOT_FOUND: '诊断会话不存在或已删除',
  DIAGNOSIS_BUSY: '诊断任务并发已满，请稍后再试',
  DIAGNOSIS_REPORT_INVALID: 'AI 诊断报告解析失败',
  // 五期打磨 sprint 2：诊断引擎 mini-loop 的另外两个终态错误码
  // （internal/diagnosis/loop.go 产出）——此前抽屉错误列直显英文原码。
  DIAGNOSIS_MAX_ROUNDS: '诊断轮数耗尽，未能产出报告',
  DIAGNOSIS_TIMEOUT: '诊断会话超时',
  INSPECTION_JOB_NOT_FOUND: '巡检任务不存在或已删除',
  INSPECTION_REPORT_NOT_FOUND: '巡检报告不存在',
  CRON_INVALID: 'cron 表达式不合法（应为五段：分 时 日 月 周，UTC）',
  BATCH_JOB_NOT_FOUND: '批量任务不存在或已删除',
  BATCH_TOO_MANY_TARGETS: '批量目标超过 50 台上限',
  // 五期打磨 sprint 4：批量通道落库的 error_code（internal/batchx/service.go
  // errCode* 常量集）此前在详情抽屉错误列与 decide toast 直显英文原码。
  // SSH_EXEC_FAILED / SERVER_NOT_FOUND / SSH_TIMEOUT / SSH_HOST_KEY_CHANGED /
  // COMMAND_FORBIDDEN 通用表已有一期中文口径，describeResultCode 回落复用
  // （不复制文案，防两套口径漂移）；这里只补通用表没有的两个批量专属码。
  NETCLI_EXEC_FAILED: '网络设备命令执行失败（expect 超时或通道断开）',
  EXEC_INTERRUPTED: '任务已取消，命令执行被中断',
};

/** 五期失败文案：命中五期码用本表，其余回落到通用 describeFailure。 */
export function describePhase5Failure(failure: ApiFailure): string {
  const hit = PHASE5_CODE_MESSAGES[failure.code];
  if (hit) {
    return hit;
  }
  return describeFailure(failure);
}

/**
 * 事件/结果/报告携带的业务 error_code 字段（非 HTTP 错误体）转中文：
 * 命中五期码用本表，未命中回落通用 CODE_MESSAGES（批量通道落库的
 * SSH_EXEC_FAILED / SERVER_NOT_FOUND 等，与全站一期中文口径一致），
 * 仍未知才显原码。
 */
export function describeResultCode(code: string | null | undefined): string {
  if (!code) {
    return '';
  }
  return PHASE5_CODE_MESSAGES[code] ?? CODE_MESSAGES[code] ?? code;
}
