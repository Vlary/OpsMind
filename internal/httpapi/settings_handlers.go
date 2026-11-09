// settings_handlers.go 实现六期 BE-P 的公开系统设置端点（契约 2.5/2.11
// 约定 1/4.11/9.7）：GET/PUT /api/settings 只暴露公开键（v1 仅
// approval_mode——全局审批门限）；内部键（bootstrap.*、monitor.*）不经
// 该端点返回。写路径落 settings['approval.mode'] 并审计 settings.update。
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/store"
)

// settingsUpdateRequest 是 PUT /api/settings 请求体（SettingsUpdate：
// v1 仅 approval_mode，必填）。
type settingsUpdateRequest struct {
	ApprovalMode string `json:"approval_mode"`
}

// RegisterSettingsRoutes 注册六期 BE-P 设置路由（契约 2.5）。
func (d *Deps) RegisterSettingsRoutes(api *gin.RouterGroup) {
	api.GET("/settings", d.handleGetSettings)
	api.PUT("/settings", d.handleUpdateSettings)
}

// handleGetSettings 返回公开设置（键缺失按 standard 兜底——契约 4.11）。
func (d *Deps) handleGetSettings(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	mode, err := approvals.GlobalMode(c.Request.Context(), d.Store)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{"approval_mode": mode})
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
	raw, err := json.Marshal(string(mode))
	if err != nil {
		d.logError(c, "encode approval mode failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		return
	}
	if d.Clock == nil {
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "clock unavailable")
		return
	}
	if _, err := d.Store.Settings().Set(c.Request.Context(), "approval.mode", raw, d.Clock()); err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	user, _ := userFromContext(c)
	d.auditEnqueue(c, audit.Event{
		ActorType:    "user",
		ActorID:      user.ID,
		Action:       "settings.update",
		ResourceType: "setting",
		ResourceID:   "approval.mode",
		Detail:       map[string]any{"key": "approval.mode", "value": string(mode)},
	})
	c.JSON(http.StatusOK, gin.H{"approval_mode": mode})
}
