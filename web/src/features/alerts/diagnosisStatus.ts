// M4 五期：诊断会话状态纯逻辑（独立文件避免 react-refresh/only-export-components）。

export const DIAGNOSIS_STATUS_LABELS: Record<string, string> = {
  pending: '排队中',
  diagnosing: '诊断中',
  report: '生成报告',
  awaiting_fix: '等待修复处理',
  done: '已完成',
  expired: '已过期',
  failed: '失败',
};

export const DIAGNOSIS_STATUS_COLOR: Record<string, string> = {
  pending: 'default',
  diagnosing: 'processing',
  report: 'processing',
  awaiting_fix: 'warning',
  done: 'success',
  expired: 'default',
  failed: 'error',
};

/** 非终态判定（8.4：pending/diagnosing/report 轮询 3s 至终态）。 */
export function isDiagnosisActive(status: string): boolean {
  return status === 'pending' || status === 'diagnosing' || status === 'report';
}
