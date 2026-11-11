package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/config"
)

// NewRouter 构建完整路由。
//
// 中间件顺序固定：recovery → requestID → accessLog → securityHeaders →
// originCheck → jsonContentType → bodyLimit。
//
// BE1 注册 auth/conversations；BE2 通过 RegisterBE2Routes 注册
// servers/databases/approvals/audit/ops；BE3 通过 RegisterRoutes 注册
// providers/chat。handler 文件由对应工程师维护。
func NewRouter(d *Deps) *gin.Engine {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Version == "" {
		d.Version = "dev"
	}

	r := gin.New()
	// 只信任本机反向代理。信任 0.0.0.0/0 会让任何直连客户端伪造
	// X-Forwarded-For，从而绕过按 IP 的登录限流并污染审计 IP。
	_ = r.SetTrustedProxies(trustedProxies(d.Config))
	r.Use(
		d.recovery(),
		d.requestID(),
		d.accessLog(),
		d.securityHeaders(),
		d.originCheck(),
		d.jsonContentType(),
		d.bodyLimit(jsonBodyLimit),
	)

	r.GET("/healthz", d.handleHealth)

	api := r.Group("/api")
	api.POST("/auth/login", d.handleLogin)

	protected := api.Group("")
	protected.Use(d.requireAuth())
	protected.POST("/auth/logout", d.handleLogout)
	protected.GET("/auth/me", d.handleMe)
	protected.POST("/auth/password", d.handleChangePassword)
	protected.GET("/conversations", d.handleListConversations)
	protected.POST("/conversations", d.handleCreateConversation)
	protected.GET("/conversations/:id", d.handleGetConversation)
	protected.PATCH("/conversations/:id", d.handleUpdateConversation)
	protected.DELETE("/conversations/:id", d.handleDeleteConversation)
	protected.GET("/conversations/:id/messages", d.handleListMessages)
	protected.GET("/conversations/:id/tool-calls", d.handleListToolCalls)
	// 六期 BE-P（契约 2.11 约定 4）：会话执行回放——持久化表重建。
	protected.GET("/conversations/:id/replay", d.handleConversationReplay)
	// 八期 BE-R（契约 2.12 约定 4 / 5.19.1）：回放目录聚合查询。
	protected.GET("/replays", d.handleListReplays)

	// BE2 的 RegisterBE2Routes 注册 servers/databases/approvals/audit/ops。
	d.RegisterBE2Routes(protected)

	// BE3 的 RegisterRoutes 注册 providers 与 chat（文件由 BE3 创建）。
	d.RegisterRoutes(protected)

	// W3 的 RegisterTermRoutes 注册 term 会话/录制与终端 WS。
	d.RegisterTermRoutes(protected, api)

	// W4 的 RegisterRdpRoutes 注册 rdp 会话/录制与 RDP WS。
	d.RegisterRdpRoutes(protected, api)

	// C1（四期）的 RegisterContainerRoutes 注册容器五路由（契约 2.5/2.9）。
	d.RegisterContainerRoutes(protected)

	// M1（五期）注册监控/告警路由（契约 2.5：monitor/collector/metrics、
	// alert-rules、alert-events、notification-channels）。
	d.RegisterMonitorRoutes(protected)
	d.RegisterAlertRoutes(protected)

	// M2（五期）注册诊断与巡检路由（契约 2.5：diagnosis/sessions*、
	// inspection/jobs*、inspection/reports*、inspection/run）。
	d.RegisterDiagnosisRoutes(protected)
	d.RegisterInspectionRoutes(protected)

	// M3（五期）注册批量任务路由（契约 2.5：/api/batch/jobs*）。
	d.RegisterBatchRoutes(protected)

	// BE-P（六期）注册公开系统设置路由（契约 2.5/2.11 约定 1：
	// GET/PUT /api/settings——v1 仅 approval_mode）。
	d.RegisterSettingsRoutes(protected)

	// 未注册的路由由 NoRoute 返回统一 404 JSON。

	d.mountStatic(r)
	return r
}

// trustedProxies 返回允许提供 X-Forwarded-For 的代理地址。
//
// 只信任环回地址：开启 AIOPS_TRUST_PROXY 通常表示本机有 nginx 之类的反向
// 代理；信任任意来源会让客户端直接伪造 XFF 绕过限流。
func trustedProxies(cfg *config.Config) []string {
	if cfg == nil || !cfg.TrustProxy {
		return nil
	}
	return []string{"127.0.0.1", "::1"}
}

// mountStatic 在生产环境提供 SPA；不对 /api/* 做回退。
func (d *Deps) mountStatic(r *gin.Engine) {
	webDir := ""
	if d.Config != nil {
		webDir = strings.TrimSpace(d.Config.WebDir)
	}
	if webDir != "" {
		assets := filepath.Join(webDir, "assets")
		if info, err := os.Stat(assets); err == nil && info.IsDir() {
			r.Static("/assets", assets)
		}
		index := filepath.Join(webDir, "index.html")
		r.GET("/", func(c *gin.Context) { c.File(index) })
	}

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/") || path == "/healthz" {
			apierr.Write(c, apierr.New(apierr.NotFound, "not found"))
			return
		}
		if webDir != "" && !strings.HasPrefix(path, "/assets/") {
			index := filepath.Join(webDir, "index.html")
			if _, err := os.Stat(index); err == nil {
				c.File(index)
				return
			}
		}
		c.Status(http.StatusNotFound)
	})
}
