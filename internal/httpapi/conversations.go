package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/authz"
)

// RegisterRoutes 注册 BE3 拥有的 HTTP 端点。由 SCAFFOLD 的 NewRouter 在受保护组上调用：
//
//	api := r.Group("/api", d.requireAuth())
//	d.RegisterRoutes(api)
//
// 不在本函数中注册会话/消息读取端点（属于 BE1）。
// 注意：发送消息只有 POST /api/chat 一个入口（契约 2.5 与 api/openapi.yaml 的
// 事实源）；不要注册 POST /api/conversations/:id/messages 之类的别名路由。
func (d *Deps) RegisterRoutes(api *gin.RouterGroup) {
	// 工具执行期复读用户状态（停用/降权后立即拒绝执行）。
	if d.Agent != nil {
		d.Agent.SetUserAuthStateLookup(d)
	}
	// GET /providers* 对非 provider 管理者返回最小 DTO（契约 12.3 第 17 条），
	// 因此路由守卫只要求登录；完整详情/写操作需 provider:manage。
	d.guardAuth(api, http.MethodGet, "/providers/presets", d.handleProviderPresets)
	d.guardAuth(api, http.MethodGet, "/providers", d.handleListProviders)
	d.guard(api, http.MethodPost, "/providers", authz.PermProviders, d.handleCreateProvider)
	d.guardAuth(api, http.MethodGet, "/providers/:id", d.handleGetProvider)
	d.guard(api, http.MethodPatch, "/providers/:id", authz.PermProviders, d.handleUpdateProvider)
	d.guard(api, http.MethodDelete, "/providers/:id", authz.PermProviders, d.handleDeleteProvider)
	d.guard(api, http.MethodPost, "/providers/:id/test", authz.PermProviders, d.handleTestProvider)
	d.guard(api, http.MethodPost, "/providers/:id/models/refresh", authz.PermProviders, d.handleRefreshProviderModels)

	// POST /chat 的权限与会话归属在 handler 内校验（会话归属需要先加载会话）。
	api.POST("/chat", d.handleChat)
}
