// degraded.go 实现单管理员降级模式（契约 12.3 第 11 条 / 12.7 / 12.8）。
//
// 判定事实源是 CountEnabledApprovers（role IN ('admin','approver') 且
// enabled）：可承担「第二人」职责的启用账号 <= 1 时，第二人批准在制度上
// 不成立，系统必须 fail-closed —— 不能批准审批、不能申请/开终端/RDP、
// 不能放宽 policy。恢复路径只有主机侧 aiops break-glass / aiops admin
// reset，或补一个启用 admin/approver。
//
// 口径必须与启动告警（cmd/aiops/main.go 的 bootstrapSecondApprover）、
// README「创建第二个账号（admin 或 approver）」和 CONTRACT 12.3 第 11/25
// 条一致：只数管理员会让「1 个 admin + 1 个 approver」的合规部署永远
// 批不了任何业务审批（走查 high）。
package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/authz"
)

// singleAdminDegraded 报告系统是否处于单管理员降级模式。
//
// 判定口径按契约 12.3 第 11/25 条：可承担第二人职责的启用账号
// （admin + approver）<= 1 时进入降级模式——唯一管理员不能借「他人批准」
// 扩大自己的执行面（needs_approval 执行、终端/RDP grant 与会话、policy
// 放宽）。解除方式：补一个启用 admin/approver，或主机侧 aiops
// break-glass / aiops admin reset。
//
// 存储不可用时返回 error，调用方必须 fail-closed（按降级处理）。
func (d *Deps) singleAdminDegraded(c *gin.Context) (bool, error) {
	if d.Store == nil || d.Store.Users() == nil {
		return true, nil
	}
	n, err := d.Store.Users().CountEnabledApprovers(c.Request.Context())
	if err != nil {
		return true, err
	}
	return n <= 1, nil
}

// requireSecondApprover 在降级模式下拒绝会扩大执行面的动作（approve 审批、
// 开终端/RDP、policy 放宽）。返回 false 时已写响应。
//
// perm 是该动作本应具备的权限（approval:decide / terminal:use /
// policy:request）：降级拒绝写标准 authz.denied（detail 含
// permission/method/path/ip + action/reason），并复用 actor+permission
// 限速——第 4 轮走查 low：此前 detail 只有 {action,reason} 且未限速，
// 运维反复点「批准」会按请求数刷审计。
//
// 存储故障同样拒绝（fail-closed），并记录错误日志。
func (d *Deps) requireSecondApprover(c *gin.Context, perm authz.Permission, action string) bool {
	degraded, err := d.singleAdminDegraded(c)
	if err != nil {
		d.logHandlerError(c, "check single-admin degraded mode failed", err)
		writeErrorStatus(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"cannot verify approver availability")
		return false
	}
	if !degraded {
		return true
	}
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return false
	}
	d.authzDeniedDetail(c, user, perm, map[string]any{
		"action": action,
		"reason": "single_admin_degraded",
	})
	writeErrorStatus(c, http.StatusConflict, "NO_SECOND_APPROVER",
		"system is in single-admin degraded mode; create a second admin or approver first")
	return false
}
