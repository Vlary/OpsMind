// C2 四期：容器错误码中文映射与 console 审批 decide 结果文案（纯函数）。
// CONTAINER_* 无通用中文映射（apiError.ts 归 FE2，契约目录树 2.3），沿用
// ServersPage 对 NETCLI_* 的本地映射先例：已知码在此本地化，未知码回退
// describeFailure 的通用文案（不吞后端语义）。

import { describeFailure, type ApiFailure } from '../../components/admin/apiError';
import { formatDuration } from '../../components/admin/format';
import type { components } from '../../api/schema';

const CONTAINER_CODE_MESSAGES: Record<string, string> = {
  CONTAINER_UNSUPPORTED: '该服务器未探测到受支持的容器运行时（docker/podman）',
  CONTAINER_NOT_FOUND: '容器不存在或已删除',
  CONTAINER_CLI_FAILED: '容器 CLI 执行失败（daemon 未运行或命令执行失败）',
  // p4 sprint 6（走查 D）：SSH/WinRM 通道层失败（拨号/认证/会话，命令未
  // 送达远端）后端映射 SSH_EXEC_FAILED——与 agent 容器工具路径同口径；
  // 曾落 CONTAINER_CLI_FAILED 把「服务器不可达」误导成「daemon 未运行」。
  SSH_EXEC_FAILED: '无法连接服务器执行容器命令（可先在服务器页测试连接）',
};

/**
 * 容器接口失败的中文文案。CONTAINER_UNSUPPORTED 的错误体 detail 携带探测
 * 现值 runtime（契约 9.6 / S21 ①），有值且非空时拼进括号——「未探测到…
 * （当前运行时：containerd）」比裸文案更能解释原因。
 *
 * persistedRuntime 是 servers 查询里的库值 container_runtime：探测通道
 * 错误（SSH 拨号失败/超时）时后端按 none 返回 400 但**不写回**（四期评审
 * 修订：一次瞬态失败不得把服务器钉死为 none），库值仍 ''。此时
 * 「当前运行时：none」与库中状态矛盾，改文案为「本次探测失败，按无容器
 * 处理」；探测确认写回（库值更新为 none）后页面走 unsupportedKnown 空态，
 * 不再渲染本横幅。
 */
export function describeContainerFailure(failure: ApiFailure, persistedRuntime?: string): string {
  const localized = CONTAINER_CODE_MESSAGES[failure.code];
  if (!localized) {
    return describeFailure(failure);
  }
  if (failure.code === 'CONTAINER_UNSUPPORTED' && failure.details) {
    const runtime = failure.details['runtime'];
    if (typeof runtime === 'string' && runtime === 'none' && persistedRuntime === '') {
      return `${localized}——本次探测失败，按无容器处理（服务器记录未修改）`;
    }
    if (typeof runtime === 'string' && runtime !== '') {
      const label = runtime === 'containerd' ? 'containerd（仅探测，不支持管理）' : runtime;
      return `${localized}（当前运行时：${label}）`;
    }
  }
  return localized;
}

type Approval = components['schemas']['Approval'];
type ApprovalExecution = components['schemas']['ApprovalExecution'];

export interface DecisionOutcome {
  kind: 'success' | 'failed' | 'denied';
  message: string;
}

/**
 * console 审批 decide 响应（ApprovalDecisionResult）的回填文案（契约
 * 8.7 容器页）：execution 非 null 时展示 exit_code/duration，失败显示
 * error_code；execution 恒 null 的 denied（未执行）与兜底 approved（chat
 * 审批形状，容器页理论上不出现）按状态描述。
 *
 * 通道失败特判：后端 fail 路径（连接建立失败/命令未执行）回填哨兵
 * exit_code=-1 且不带 duration_ms（containers_handlers.go executeConsoleApproval
 * 初始值），-1 与「—」对用户无意义——改语义文案「连接或命令通道失败」，
 * 不展示哨兵数字与空时长（走查 T4）。
 */
export function describeDecisionOutcome(
  approval: Approval | null,
  execution: ApprovalExecution | null,
): DecisionOutcome {
  // 六期（契约 2.11 约定 2）：action 端点在门限判定直执行时返回
  // approval=null + execution——无审批对象，直接按 execution 判定。
  if (approval?.status === 'denied') {
    return { kind: 'denied', message: '已拒绝，命令未执行' };
  }
  if (approval && approval.status !== 'approved') {
    return { kind: 'denied', message: `审批未生效（${approval.status}）` };
  }
  if (!execution) {
    return { kind: 'success', message: '已批准' };
  }
  if (execution.exit_code === 0) {
    return {
      kind: 'success',
      message: `执行成功（exit 0 · ${formatDuration(execution.duration_ms)}）`,
    };
  }
  const cause = execution.error_code ? ` · ${execution.error_code}` : '';
  if (execution.exit_code === -1 && !execution.duration_ms) {
    return {
      kind: 'failed',
      message: `执行失败（连接或命令通道失败，命令未执行${cause}）`,
    };
  }
  return {
    kind: 'failed',
    message: `执行失败（exit ${execution.exit_code} · ${formatDuration(execution.duration_ms)}${cause}）`,
  };
}

/**
 * 命令区标签按弹窗阶段取文案（p4 sprint 5，walk5）：confirm（审批未创建）
 * 预告「将申请执行的命令」；pending「待执行命令」；result 已决——命令已
 * 真正下发过（批准路径，含 exit 非 0 的执行失败）是「已执行命令」，denied
 * 从未执行是「未执行命令」（与 describeDecisionOutcome 的「已拒绝，命令
 * 未执行」同口径，不得宣称已执行）。
 */
export function commandSectionLabel(
  stage: 'confirm' | 'pending' | 'result',
  approval: Approval | null,
  /** confirm 态的门限预测（containerActionPolicy.containerActionNeedsApproval）；
   *  true=将申请审批、false=将直接执行、null=未知（门限尚未加载）。 */
  confirmNeedsApproval?: boolean | null,
): string {
  if (stage === 'confirm') {
    if (confirmNeedsApproval === false) {
      return '将直接执行的命令';
    }
    // 未知沿用「将申请执行的命令」的中性承诺？——不：未知时两种分流都可能，
    // 用不提审批的「将执行的命令」（六期打磨 sprint 6）。
    return confirmNeedsApproval === true ? '将申请执行的命令' : '将执行的命令';
  }
  if (stage === 'pending') {
    return '待执行命令';
  }
  return approval !== null && approval.status === 'denied' ? '未执行命令' : '已执行命令';
}

/**
 * 确认弹窗阶段的命令预览：与后端固化的审批命令一致（契约 2.9 约定 3：
 * 「按运行时拼好的逐字节命令如 docker stop <cid>」）。展示用；真命令以
 * action 响应返回的 approval.command 为准（逐字节）。
 */
export function previewContainerCommand(
  runtime: string,
  containerId: string,
  action: string,
): string {
  return `${runtime} ${action} ${containerId}`;
}
