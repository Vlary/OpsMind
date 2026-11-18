// 审批 decide 响应的执行结果摘要（第 8 轮走查 medium）。
//
// 背景：source=console/batch（容器、批量、诊断修复）的审批由服务端在
// approve 时同步逐字节执行，execution 只在 decide 响应里出现一次，随后
// 审批从 pending 队列移除、详情接口不再回传。ApprovalCard 此前丢弃了
// result.execution，批准后固定显示「已批准」——审批者看不到
// exit_code/error_code，会误判命令尚未执行或不知道是否失败。
//
// 这里把「人话文案 + 原始明细」收敛成一个纯函数，供审批卡与 /approvals
// 的决策反馈共用；文案复用容器页的 describeDecisionOutcome（同样的
// exit_code/duration/-1 哨兵口径），避免两处对同一 execution 说法不一致。

import type { components } from '../../api/schema';
import { describeDecisionOutcome, type DecisionOutcome } from '../containers/containerFailures';

type Approval = components['schemas']['Approval'];
type ApprovalExecution = components['schemas']['ApprovalExecution'];

export interface ApprovalOutcome extends DecisionOutcome {
  /** 原始明细：exit_code 恒展示，error_code 有值时附上。 */
  detail: string;
}

/**
 * 已决策审批的即时结果。execution 为 null（chat/terminal/policy 审批与
 * 拒绝）时仍返回审批状态文案（describeDecisionOutcome 的既有口径），
 * detail 为 null，调用方据此决定是否展示执行明细。
 */
export function approvalOutcome(
  approval: Approval,
  execution: ApprovalExecution | null,
): ApprovalOutcome {
  const outcome = describeDecisionOutcome(approval, execution);
  if (!execution) {
    return { ...outcome, detail: '' };
  }
  const parts = [`exit_code=${execution.exit_code}`];
  if (execution.error_code) {
    parts.push(`error_code=${execution.error_code}`);
  }
  return { ...outcome, detail: parts.join(' · ') };
}
