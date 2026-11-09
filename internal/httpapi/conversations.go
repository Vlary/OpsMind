package httpapi

import (
	"github.com/gin-gonic/gin"
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
	api.GET("/providers/presets", d.handleProviderPresets)
	api.GET("/providers", d.handleListProviders)
	api.POST("/providers", d.handleCreateProvider)
	api.GET("/providers/:id", d.handleGetProvider)
	api.PATCH("/providers/:id", d.handleUpdateProvider)
	api.DELETE("/providers/:id", d.handleDeleteProvider)
	api.POST("/providers/:id/test", d.handleTestProvider)
	api.POST("/providers/:id/models/refresh", d.handleRefreshProviderModels)

	api.POST("/chat", d.handleChat)
}
