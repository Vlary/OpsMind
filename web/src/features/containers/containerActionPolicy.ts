// 容器操作确认弹窗的「提交后会发生什么」展示判定（六期打磨 sprint 6 走查
// medium）。
//
// 背景：confirm 态文案与按钮此前恒为「操作不直接执行——将创建一条需人工
// 批准的审批」+「创建审批」，但六期统一门限（契约 2.11 约定 2）下
// safe+auto_exec（standard 档）或 none/high 档实际是**直接执行、不产生任何
// 审批**——用户以为在排队审批，破坏性命令（stop/restart）已经真实执行。
//
// 本模块是 `approvals.NeedsApproval`（internal/approvals/policy.go，契约
// 2.11 约定 2）矩阵的前端只读镜像：**仅用于 confirm 态文案预告，不是权限
// 判定**——真正的分流永远由后端 POST action 的响应决定（approval 非 null
// 即走审批，null 即已直执行）。任何矩阵变更必须两边同步（后端有 G38 24 格
// 断言，这里对应有单测逐格覆盖）。
//
// 容器动作的输入是固定的：start=safe；stop/restart=needs_approval，规则
// CONTAINER_CHANGE 不在 7.3E 的 warning 集内（internal/risk/severity.go），
// 因此 severity 恒 high。

import type { components } from '../../api/schema';

type ApprovalMode = components['schemas']['ApprovalMode'];
type ContainerAction = components['schemas']['ContainerActionRequest']['action'];

/**
 * 预测「提交后是否需要人工审批」。
 *
 * 返回 `null` 表示门限未知（settings 尚未加载/请求失败）——调用方必须退到
 * 不承诺任一分支的中性文案，不得默认「需审批」。
 */
export function containerActionNeedsApproval(
  mode: ApprovalMode | string | null | undefined,
  autoExecEnabled: boolean,
  action: ContainerAction,
): boolean | null {
  if (mode !== 'all' && mode !== 'standard' && mode !== 'high' && mode !== 'none') {
    return null;
  }
  const safe = action === 'start';
  // forbidden 不进审批（容器动作集封闭，不存在 forbidden）。
  switch (mode) {
    case 'all':
      // 全部命令审批（含只读）。
      return true;
    case 'standard':
      // 现行行为：safe 且 auto_exec 直执行；其余一律审批。
      return !(safe && autoExecEnabled);
    case 'high':
      // 仅高危审批：safe 恒直执行；needs_approval 为 high 严重度（容器
      // 动作恒如此）→ 审批。
      return !safe;
    case 'none':
      // 免审批：safe 直执行；needs_approval 仍受服务器 auto_exec 硬保护。
      return safe ? false : !autoExecEnabled;
    default:
      return null;
  }
}
