// settings_handlers.go 实现六期 BE-P 的公开系统设置端点（契约 2.5/2.11
// 约定 1/4.11/9.7）：GET/PUT /api/settings 只暴露公开键（v1 仅
// approval_mode——全局审批门限）；内部键（bootstrap.*、monitor.*）不经
// 该端点返回。写路径落 settings['approval.mode'] 并审计 settings.update。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// settingsUpdateRequest 是 PUT /api/settings 请求体（SettingsUpdate：
// v1 仅 approval_mode，必填）。
type settingsUpdateRequest struct {
	ApprovalMode string `json:"approval_mode"`
}

// RegisterSettingsRoutes 注册六期 BE-P 设置路由（契约 2.5）。
func (d *Deps) RegisterSettingsRoutes(api *gin.RouterGroup) {
	d.guardAuth(api, http.MethodGet, "/settings", d.handleGetSettings)
	d.guard(api, http.MethodPut, "/settings", authz.PermPolicyRequest, d.handleUpdateSettings)
}

// handleGetSettings 返回公开设置（键缺失按 standard 兜底——契约 4.11）。
//
// 走查第 3 轮 medium：响应额外携带只读的降级态（single_admin_degraded +
// enabled_approvers），让审批页/审批卡在点「批准」之前就能渲染中文说明
// 并把按钮置灰，而不是让审批人撞一次英文 409。两者都不是敏感信息
// （409 文案本身就公开了该事实），且不参与写入。
func (d *Deps) handleGetSettings(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	mode, err := approvals.GlobalMode(c.Request.Context(), d.Store)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	resp := gin.H{"approval_mode": mode}
	// 降级态查询失败不阻断设置读取（前端按「未知」处理，不会隐藏按钮）：
	// 这里只做提示，权威判定仍在 requireSecondApprover。
	if d.Store.Users() != nil {
		if n, nerr := d.Store.Users().CountEnabledApprovers(c.Request.Context()); nerr == nil {
			resp["enabled_approvers"] = n
			resp["single_admin_degraded"] = n <= 1
		}
	}
	c.JSON(http.StatusOK, resp)
}

// handleUpdateSettings 写 settings['approval.mode']（JSON 字符串枚举）。
// 变更对创建之后的审批即时生效（已 pending 的审批不迁移、按创建时快照
// 展示）。审计 settings.update（resource_type=setting、resource_id=
// approval.mode、detail={key,value}——值为门限枚举，非敏感）。
func (d *Deps) handleUpdateSettings(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req settingsUpdateRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	mode := store.ApprovalMode(req.ApprovalMode)
	if !store.ValidApprovalMode(mode) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "approval_mode must be one of all, standard, high, none")
		return
	}
	if d.Clock == nil {
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "clock unavailable")
		return
	}
	current, err := approvals.GlobalMode(c.Request.Context(), d.Store)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	user, _ := userFromContext(c)
	if approvals.IsWidening(current, mode) {
		// 契约 12.3 第 10/11 条：放宽全局门限不能由单个管理员直接生效。
		// 必须有第二个人（approver）批准；系统里没有第二个可承担审批
		// 职责的启用账号（admin/approver）时直接 409 NO_SECOND_APPROVER，
		// 连审批都不创建。判定与审批批准、终端/RDP 会话走同一事实源
		// （requireSecondApprover → CountEnabledApprovers），避免
		// 「1 admin + 1 approver」的合规部署在此被误判为降级（走查 high）。
		if !d.requireSecondApprover(c, authz.PermPolicyRequest, "policy.widen") {
			return
		}
		if d.Approvals == nil {
			writeErrorStatus(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "approval service unavailable")
			return
		}
		from, to := string(current), string(mode)
		ap, cerr := d.Approvals.Create(c.Request.Context(), approvals.CreateRequest{
			Source:       "policy",
			Command:      "",
			RiskLevel:    "safe",
			Reason:       "global approval mode widening",
			ApprovalMode: string(current),
			RequestID:    c.GetString("request_id"),
			RequestedBy:  user.ID,
			PolicyFrom:   &from,
			PolicyTo:     &to,
		})
		if cerr != nil {
			if errors.Is(cerr, approvals.ErrPolicyNotWidening) {
				writeErrorStatus(c, http.StatusConflict, "POLICY_STALE", "policy change no longer widens current value")
				return
			}
			d.logHandlerError(c, "create policy approval failed", cerr)
			writeError(c, apierr.Internal, "internal server error")
			return
		}
		// 202：不写 settings，等待第二人经 /api/approvals/:id/decide 批准。
		c.JSON(http.StatusAccepted, gin.H{
			"approval_mode":    current,
			"pending_approval": ap,
		})
		return
	}
	raw, err := json.Marshal(string(mode))
	if err != nil {
		d.logError(c, "encode approval mode failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		return
	}
	// settings 写入与 settings.update 审计在同一事务内提交（契约 12.3 第
	// 19 条）：进程在提交与入队之间崩溃不再丢这条审计。
	auditEv := audit.Event{
		ActorType:    "user",
		ActorID:      user.ID,
		Action:       "settings.update",
		ResourceType: "setting",
		ResourceID:   "approval.mode",
		Detail:       map[string]any{"key": "approval.mode", "value": string(mode)},
	}
	outboxUsed := false
	if err := d.Store.WithTx(c.Request.Context(), func(tx store.Store) error {
		if _, err := tx.Settings().Set(c.Request.Context(), "approval.mode", raw, d.Clock()); err != nil {
			return err
		}
		used, err := d.auditInTxCtx(c, tx, auditEv)
		outboxUsed = used
		return err
	}); err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if !outboxUsed {
		d.auditEnqueue(c, auditEv)
	}
	c.JSON(http.StatusOK, gin.H{"approval_mode": mode})
}
