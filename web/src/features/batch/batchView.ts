// M4 五期：批量页纯逻辑（状态中文、轮询终态判定、取消可用性、失败行
// 重试集合）。node 直测（__tests__/batchView.test.ts）。

import type { components } from '../../api/schema';

export type BatchJob = components['schemas']['BatchJob'];
export type BatchResult = components['schemas']['BatchResult'];
export type BatchResultStatus = BatchResult['status'];

export const BATCH_STATUS_LABELS: Record<string, string> = {
  pending: '待分派',
  running: '执行中',
  partial: '部分完成',
  done: '已完成',
  failed: '编排失败',
  cancelled: '已取消',
};

export const RESULT_STATUS_LABELS: Record<string, string> = {
  pending: '待处理',
  awaiting_approval: '待审批',
  executed: '已执行',
  failed: '执行失败',
  denied: '已拒绝',
  expired: '审批过期',
  skipped: '已跳过',
  cancelled: '已取消',
};

/** job 仍需轮询（8.4：非终态 2s 轮询，终态停止）。 */
export function isBatchJobActive(status: string): boolean {
  return status === 'pending' || status === 'running';
}

/** 单台结果是否终态（进度灯计“已完成”）。 */
export function isResultFinal(status: BatchResultStatus): boolean {
  return (
    status === 'executed' ||
    status === 'failed' ||
    status === 'denied' ||
    status === 'expired' ||
    status === 'skipped' ||
    status === 'cancelled'
  );
}

/** 「取消」仅 pending/awaiting_approval 存在时可用（契约 8.7 批量页）。 */
export function canCancelBatch(results: readonly BatchResult[]): boolean {
  return results.some(
    (result) => result.status === 'pending' || result.status === 'awaiting_approval',
  );
}

/** 失败行集合（重试入口用）：failed / denied / expired / skipped——
 * denied/expired 的审批已终结，重试是唯一推进路径（五期评审发现实现
 * 曾漏掉这两态，被拒绝/过期的服务器无法经「重试失败项」重发）。 */
export function failedServerIds(results: readonly BatchResult[]): string[] {
  return results
    .filter(
      (result) =>
        result.status === 'failed' ||
        result.status === 'denied' ||
        result.status === 'expired' ||
        result.status === 'skipped',
    )
    .map((result) => result.server_id);
}

/** 进度摘要：done/total 与状态灯计数。 */
export function progressSummary(results: readonly BatchResult[]): {
  total: number;
  done: number;
  awaiting: number;
  failed: number;
} {
  let done = 0;
  let awaiting = 0;
  let failed = 0;
  for (const result of results) {
    if (isResultFinal(result.status)) {
      done++;
    }
    if (result.status === 'awaiting_approval' || result.status === 'pending') {
      awaiting++;
    }
    if (result.status === 'failed' || result.status === 'denied' || result.status === 'skipped') {
      failed++;
    }
  }
  return { total: results.length, done, awaiting, failed };
}

/** 状态灯颜色（逐台状态灯：待审批黄 / 执行中蓝 / 成功绿 / 失败红 / 其他灰）。 */
export function resultStatusColor(status: BatchResultStatus): string {
  switch (status) {
    case 'pending':
      return 'var(--color-text-tertiary)';
    case 'awaiting_approval':
      return 'var(--color-warning)';
    case 'executed':
      return 'var(--color-success)';
    case 'failed':
    case 'denied':
      return 'var(--color-danger)';
    default:
      return 'var(--color-text-tertiary)';
  }
}

/**
 * 单台结果「实际执行命令」的展示口径（六期打磨 sprint 5 走查 medium）。
 *
 * 背景：用户在批量审批行把命令改成 uptime 并批准后，执行的是 uptime，但
 * 详情抽屉此前只显示 job 的原文命令（`rm -rf …`），全无改写痕迹——同刻
 * API 的 results[0].output 是 uptime 的输出、approvals.edited_command 也是
 * uptime，页面与事实背离。
 *
 * 返回 `command`（实际执行的命令文本：有改写取改写，否则 job 原文）与
 * `edited`（是否发生用户改写，UI 据此加「用户改写」Tag）。edited_command
 * 为空串表示未改写或无审批（safe 直执行），命令回落 job 原文。
 */
export function executedCommandOf(
  jobCommand: string,
  result: Pick<BatchResult, 'edited_command'>,
): { command: string; edited: boolean } {
  const editedCommand = result.edited_command ?? '';
  if (editedCommand !== '') {
    return { command: editedCommand, edited: true };
  }
  return { command: jobCommand, edited: false };
}

/**
 * 单台「命令」列文案：改写行前缀「实际执行」，未改写行用调用方给的
 * 前缀（默认「命令」）。
 */
export function commandCellOf(
  jobCommand: string,
  result: Pick<BatchResult, 'edited_command'>,
  label = '命令',
): { command: string; edited: boolean; prefix: string } {
  const executed = executedCommandOf(jobCommand, result);
  return {
    ...executed,
    prefix: executed.edited ? '实际执行' : label,
  };
}
