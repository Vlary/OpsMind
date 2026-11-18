// authz_guards.go 是 HTTP 路由权限守卫的唯一装配点（docs/CONTRACT.md
// 12.3 第 1 条 / 12.4 第 1 条）。
//
// 契约要求「每条非静态路由恰好有一条 guard」。本文件提供：
//   - routeRules：method+path → 权限的完整清单（含 router.go 中注册、
//     只能在 handler 内联守卫的会话端点）；
//   - d.guard / d.guardAny / d.guardAuth：注册路由并记录已挂 guard，
//     TestRoutePermissions 用它们与 gin.Engine.Routes() 对账——漏挂即红灯；
//   - d.authzDenied：403 审计（authz.denied），按 actor+permission 限速。
//
// 新增端点必须先在本表登记再注册路由；未登记 = 测试失败。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// guardKind 区分路由层守卫与 handler 内联守卫（资源归属端点）。
type guardKind string

const (
	guardPermission    guardKind = "permission"
	guardAnyPermission guardKind = "permission-any"
	guardAuthenticated guardKind = "authenticated"
	guardInHandler     guardKind = "handler"
	// guardInstancePermission 表示「权限 + 实例规则」：路由只要求登录，
	// handler 先按 Perm 放行，再按 owner 例外（如审批发起人）判定；无权限
	// 且非 owner 一律 404，不泄露资源是否存在（契约 12.3 第 3 条语义）。
	guardInstancePermission guardKind = "permission-instance"
	// guardExternal 表示守卫由非本文件的注册方直接挂载（users_handlers.go
	// 由身份工程师维护，使用 authz.RequirePermission）；本表仅登记权限意图。
	guardExternal guardKind = "external"
	// guardPublic 表示公开端点（登录/邀请接受），由 Origin+限流+一次性
	// token 保护，不要求登录。
	guardPublic guardKind = "public"
)

// routeRule 是一条路由的权限声明；Path 为含 /api 前缀的完整 pattern。
type routeRule struct {
	Method  string
	Path    string
	Kind    guardKind
	Perm    authz.Permission   // Kind=guardPermission
	Perms   []authz.Permission // Kind=guardAnyPermission
	Handler string             // Kind=guardInHandler：校验所在函数名（文档用途）
}

// routeRules 是全部 /api 路由的权限矩阵（唯一清单）。
//
// 说明：
//   - 会话/消息/回放端点在 router.go 注册（本次改动不触及该文件），
//     其权限与归属检查实现在 handler 内（见 conversation_handlers.go）；
//   - GET /providers* 对非 provider 管理者返回最小 DTO（12.3 第 17 条），
//     因此路由守卫是 authenticated，最小披露在 handler 内完成；
//   - GET /settings 是登录即可读的公开键；PUT /settings 是审批策略变更，
//     需要 policy:request。
var routeRules = []routeRule{
	// ---- conversations / replay（router.go，handler 内联守卫） ----
	// GET /api/conversations 由 handler 做 AuthorizeAny(chat:use, audit:read)：
	// 非 audit:read 只看自己（SQL 过滤），audit:read 拿全量只读目录
	//（第 4 轮走查 medium，与 GET /api/replays 同源）；Perm 字段仅记录主权限。
	{http.MethodGet, "/api/conversations", guardInHandler, authz.PermChat, nil, "handleListConversations"},
	{http.MethodPost, "/api/conversations", guardInHandler, authz.PermChat, nil, "handleCreateConversation"},
	{http.MethodGet, "/api/conversations/:id", guardInHandler, authz.PermChat, nil, "handleGetConversation"},
	{http.MethodPatch, "/api/conversations/:id", guardInHandler, authz.PermChat, nil, "handleUpdateConversation"},
	{http.MethodDelete, "/api/conversations/:id", guardInHandler, authz.PermChat, nil, "handleDeleteConversation"},
	{http.MethodGet, "/api/conversations/:id/messages", guardInHandler, authz.PermChat, nil, "handleListMessages"},
	{http.MethodGet, "/api/conversations/:id/tool-calls", guardInHandler, authz.PermChat, nil, "handleListToolCalls"},
	{http.MethodGet, "/api/conversations/:id/replay", guardInHandler, authz.PermChat, nil, "handleConversationReplay"},
	{http.MethodGet, "/api/replays", guardInHandler, authz.PermChat, nil, "handleListReplays"},

	// ---- auth ----
	{http.MethodPost, "/api/auth/login", guardPublic, "", nil, "handleLogin"},
	{http.MethodPost, "/api/auth/invite/accept", guardPublic, "", nil, "handleAcceptInvite"},
	{http.MethodPost, "/api/auth/logout", guardAuthenticated, "", nil, ""},
	{http.MethodGet, "/api/auth/me", guardAuthenticated, "", nil, ""},
	{http.MethodPost, "/api/auth/password", guardAuthenticated, "", nil, ""},

	// ---- servers / databases / approvals / audit / risk ----
	{http.MethodGet, "/api/servers", guardPermission, authz.PermAssetsRead, nil, ""},
	{http.MethodPost, "/api/servers", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodGet, "/api/servers/:id", guardPermission, authz.PermAssetsRead, nil, ""},
	{http.MethodPatch, "/api/servers/:id", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodDelete, "/api/servers/:id", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodPost, "/api/servers/:id/test", guardPermission, authz.PermConnectionTest, nil, ""},
	{http.MethodGet, "/api/databases", guardPermission, authz.PermAssetsRead, nil, ""},
	{http.MethodPost, "/api/databases", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodGet, "/api/databases/:id", guardPermission, authz.PermAssetsRead, nil, ""},
	{http.MethodPatch, "/api/databases/:id", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodDelete, "/api/databases/:id", guardPermission, authz.PermAssetsWrite, nil, ""},
	{http.MethodPost, "/api/databases/:id/test", guardPermission, authz.PermConnectionTest, nil, ""},
	{http.MethodPost, "/api/databases/:id/query", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/approvals", guardPermission, authz.PermApprovalRead, nil, ""},
	{http.MethodGet, "/api/approvals/:id", guardInstancePermission, authz.PermApprovalRead, nil, "handleGetApproval"},
	{http.MethodGet, "/api/approvals/:id/context", guardPermission, authz.PermApprovalRead, nil, ""},
	// POST /api/approvals/:id/decide 除 approval:decide 权限外，handler 内
	// 叠加职责分离（契约 12.3 第 6 条）：调用者=持久化 requested_by 时，
	// 先于「发起人执行权限」复检返回 403 APPROVAL_SELF_DECISION（第 6 轮
	// 走查 low——否则无 exec:write 的发起人自批会被复检短路成 FORBIDDEN）。
	// 路由权限矩阵不变，SoD 是权限之上的额外 fail-closed 限制。
	// 第 7 轮走查 medium：批量详情/诊断修复/容器操作三处 UI 复用同一事实
	//（approval:decide + requested_by != 当前用户）收敛决定按钮——客户端
	// 隐藏入口不替代本守卫，服务端仍按此矩阵拒绝。
	// 第 11 轮走查 high：approval:decide 之外还要求审批尚未过期。Service
	// 读到 pending 行后先判 now >= expires_at，过期即收敛 expired 并 409
	//（APPROVAL_NOT_PENDING），不进入 console/batch 执行或 policy 落库；
	// store 的 UPDATE 另有 expires_at > decided_at 兜底。TTL 是审批有效性
	// 的一部分，不是客户端的展示倒计时。
	{http.MethodPost, "/api/approvals/:id/decide", guardPermission, authz.PermApprovalDecide, nil, ""},
	{http.MethodGet, "/api/audit", guardPermission, authz.PermAudit, nil, ""},
	{http.MethodGet, "/api/audit/verify", guardPermission, authz.PermAudit, nil, ""},
	{http.MethodPost, "/api/ops/preview-risk", guardPermission, authz.PermExecuteReadonly, nil, ""},

	// ---- providers / chat ----
	// 第 12 轮走查 medium（前端门禁依据）：GET /providers* 对任何登录用户
	// 返回最小 DTO（12.3 第 17 条），但写操作要求 provider:manage。会话页
	// 的「配置模型/去模型管理」入口因此必须按 provider:manage 收敛
	// （web/src/features/chat/ChatPage.tsx canManageProviders）；无权限角色
	// 只看到「请联系管理员配置模型供应商」，不得指向 /providers（页面门
	// 同样要求 provider:manage）。本表若调整 provider 权限，需同步前端
	// 导航/入口与 rbac_frontend_gates_test.go。
	{http.MethodGet, "/api/providers/presets", guardAuthenticated, "", nil, ""},
	{http.MethodGet, "/api/providers", guardAuthenticated, "", nil, ""},
	{http.MethodPost, "/api/providers", guardPermission, authz.PermProviders, nil, ""},
	{http.MethodGet, "/api/providers/:id", guardAuthenticated, "", nil, ""},
	{http.MethodPatch, "/api/providers/:id", guardPermission, authz.PermProviders, nil, ""},
	{http.MethodDelete, "/api/providers/:id", guardPermission, authz.PermProviders, nil, ""},
	{http.MethodPost, "/api/providers/:id/test", guardPermission, authz.PermProviders, nil, ""},
	{http.MethodPost, "/api/providers/:id/models/refresh", guardPermission, authz.PermProviders, nil, ""},
	{http.MethodPost, "/api/chat", guardInHandler, authz.PermChat, nil, "handleChat"},

	// ---- containers ----
	{http.MethodGet, "/api/servers/:id/containers", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/servers/:id/containers/:cid", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodPost, "/api/servers/:id/containers/:cid/action", guardPermission, authz.PermExecuteWrite, nil, ""},
	{http.MethodGet, "/api/servers/:id/containers/:cid/logs", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/servers/:id/images", guardPermission, authz.PermExecuteReadonly, nil, ""},

	// ---- monitor / alerts ----
	// 第 12 轮走查 medium（前端门禁依据）：监控页空态把「添加资产并开启
	// 采集」改为「请联系管理员配置资产与采集」，因为 POST /api/servers
	// 要求 asset:write、PUT /api/monitor/collector 要求 monitor:manage
	// （PermAlertConfig），而运维工程师只有 asset:read/monitor:read。
	{http.MethodGet, "/api/monitor/overview", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodGet, "/api/monitor/collector", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodPut, "/api/monitor/collector", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodGet, "/api/servers/:id/metrics", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodGet, "/api/alert-rules", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodPost, "/api/alert-rules", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodGet, "/api/alert-rules/:id", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodPatch, "/api/alert-rules/:id", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodDelete, "/api/alert-rules/:id", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodGet, "/api/alert-events", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodGet, "/api/notification-channels", guardPermission, authz.PermMetricsRead, nil, ""},
	{http.MethodPost, "/api/notification-channels", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodPatch, "/api/notification-channels/:id", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodDelete, "/api/notification-channels/:id", guardPermission, authz.PermAlertConfig, nil, ""},
	{http.MethodPost, "/api/notification-channels/:id/test", guardPermission, authz.PermAlertConfig, nil, ""},

	// ---- diagnosis / inspection ----
	// 第 12 轮走查 low（前端门禁依据）：回放页 execution 深链按目标页
	// 权限收敛——/alerts 需要监控只读（本段 PermMetricsRead）、
	// /inspections 需要 inspection:manage、/batch 需要执行权限。审计员
	// 只有 audit:read，深链不渲染；ID 文本与复制按钮保留。
	// 第 11 轮走查 medium：GET list 与 GET :id 必须同源。列表在 SQL 层按
	// created_by 收敛（非 admin 只取自己，created_by IS NULL 的 system 行
	// 仅 admin），detail/fix/complete 在 handler 内按 owner-or-admin 判定、
	// 非 owner 404（契约 12.3 第 16 条 / 12.4 第 2 条）。路由仍要求
	// exec:read；行级归属是权限之上的额外 fail-closed 限制。
	{http.MethodGet, "/api/diagnosis/sessions", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodPost, "/api/diagnosis/sessions", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/diagnosis/sessions/:id", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodPost, "/api/diagnosis/sessions/:id/fix", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodPost, "/api/diagnosis/sessions/:id/complete", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/inspection/jobs", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodPost, "/api/inspection/jobs", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodGet, "/api/inspection/jobs/:id", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodPatch, "/api/inspection/jobs/:id", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodDelete, "/api/inspection/jobs/:id", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodPost, "/api/inspection/jobs/:id/run", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodGet, "/api/inspection/reports", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodGet, "/api/inspection/reports/:id", guardPermission, authz.PermInspection, nil, ""},
	{http.MethodPost, "/api/inspection/run", guardPermission, authz.PermInspection, nil, ""},

	// ---- batch ----
	{http.MethodPost, "/api/batch/jobs", guardPermission, authz.PermExecuteWrite, nil, ""},
	{http.MethodGet, "/api/batch/jobs", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodGet, "/api/batch/jobs/:id", guardPermission, authz.PermExecuteReadonly, nil, ""},
	{http.MethodPost, "/api/batch/jobs/:id/cancel", guardPermission, authz.PermExecuteWrite, nil, ""},

	// ---- settings ----
	{http.MethodGet, "/api/settings", guardAuthenticated, "", nil, ""},
	// PUT /api/settings 的放宽分支叠加单管理员降级门（requireSecondApprover，
	// 与审批批准/终端/RDP 同一事实源 CountEnabledApprovers）：权限仍由
	// policy:request 守卫，降级只缩小可写范围（fail-closed），不改变路由权限。
	{http.MethodPut, "/api/settings", guardPermission, authz.PermPolicyRequest, nil, ""},

	// ---- terminal / rdp ----
	// 以下四类端点除权限守卫外，handler 内还叠加单管理员降级门
	//（requireSecondApprover）：降级时不创建 grant/会话（409），但路由权限
	// 矩阵不变——降级是权限之上的额外 fail-closed 限制。
	{http.MethodPost, "/api/term/grants", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodGet, "/api/term/grants", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodPost, "/api/term/sessions", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodGet, "/api/term/sessions", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/term/recordings", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/term/recordings/:id", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/term/sessions/:id/ws", guardInHandler, authz.PermTerminal, nil, "handleTermSessionWS"},
	{http.MethodPost, "/api/rdp/grants", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodGet, "/api/rdp/grants", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodPost, "/api/rdp/sessions", guardPermission, authz.PermTerminal, nil, ""},
	{http.MethodGet, "/api/rdp/sessions", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/rdp/recordings", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/rdp/recordings/:id", guardAnyPermission, "", []authz.Permission{authz.PermTerminal, authz.PermAudit}, ""},
	{http.MethodGet, "/api/rdp/sessions/:id/ws", guardInHandler, authz.PermTerminal, nil, "handleRdpSessionWS"},

	// ---- users（users_handlers.go 由身份工程师维护，直接挂 authz.RequirePermission） ----
	{http.MethodGet, "/api/users", guardExternal, authz.PermUsersRead, nil, "handleListUsers"},
	{http.MethodPost, "/api/users", guardExternal, authz.PermUsersWrite, nil, "handleCreateUser"},
	{http.MethodGet, "/api/users/:id", guardExternal, authz.PermUsersRead, nil, "handleGetUser"},
	{http.MethodPatch, "/api/users/:id", guardExternal, authz.PermUsersWrite, nil, "handleUpdateUser"},
	{http.MethodPost, "/api/users/:id/password", guardExternal, authz.PermCredentialReset, nil, "handleResetUserPassword"},
	{http.MethodPost, "/api/users/:id/revoke-sessions", guardExternal, authz.PermUsersWrite, nil, "handleRevokeUserSessions"},
}

// ruleIndex 供测试与登记辅助按 method+path 查找。
var ruleIndex = func() map[string]routeRule {
	m := make(map[string]routeRule, len(routeRules))
	for _, r := range routeRules {
		m[r.Method+" "+r.Path] = r
	}
	return m
}()

// appliedGuards 记录实际挂到路由层的 guard（method+path → rule）。
var appliedGuards = struct {
	sync.Mutex
	m map[string]routeRule
}{m: make(map[string]routeRule, len(routeRules))}

func recordApplied(r routeRule) {
	appliedGuards.Lock()
	appliedGuards.m[r.Method+" "+r.Path] = r
	appliedGuards.Unlock()
}

// appliedGuardRules 返回已装配 guard 的副本（测试对账用）。
func appliedGuardRules() map[string]routeRule {
	appliedGuards.Lock()
	defer appliedGuards.Unlock()
	out := make(map[string]routeRule, len(appliedGuards.m))
	for k, v := range appliedGuards.m {
		out[k] = v
	}
	return out
}

// guard 注册一条单权限路由并记录。
func (d *Deps) guard(api *gin.RouterGroup, method, path string, perm authz.Permission, h gin.HandlerFunc) {
	api.Handle(method, path, authz.RequirePermission(perm, d.authzDenied), h)
	recordApplied(routeRule{Method: method, Path: api.BasePath() + path, Kind: guardPermission, Perm: perm})
}

// guardAny 注册一条「任一权限」路由（auditor 跨用户只读与 owner 自读共用）。
func (d *Deps) guardAny(api *gin.RouterGroup, method, path string, perms []authz.Permission, h gin.HandlerFunc) {
	api.Handle(method, path, authz.RequireAny(perms, d.authzDenied), h)
	recordApplied(routeRule{Method: method, Path: api.BasePath() + path, Kind: guardAnyPermission, Perms: perms})
}

// guardInstance 注册一条「登录 + 权限或实例归属」路由；权限与归属的最终
// 判定在 handler 内（本函数只负责认证组内注册与守卫表登记）。
func (d *Deps) guardInstance(api *gin.RouterGroup, method, path string, perm authz.Permission, h gin.HandlerFunc) {
	api.Handle(method, path, h)
	recordApplied(routeRule{Method: method, Path: api.BasePath() + path, Kind: guardInstancePermission, Perm: perm})
}

// guardAuth 注册一条仅要求登录的路由。
func (d *Deps) guardAuth(api *gin.RouterGroup, method, path string, h gin.HandlerFunc) {
	api.Handle(method, path, h)
	recordApplied(routeRule{Method: method, Path: api.BasePath() + path, Kind: guardAuthenticated})
}

// UserAuthState 实现 agent.UserAuthStateLookup：每个 tool call 前从 store
// 复读用户 enabled/role（契约 12.4 第 3 条）。
func (d *Deps) UserAuthState(ctx context.Context, userID string) (store.Role, bool, error) {
	if d.Store == nil {
		return "", false, context.Canceled
	}
	user, err := d.Store.Users().ByID(ctx, userID)
	if err != nil {
		return "", false, err
	}
	return user.Role, user.Enabled, nil
}

// isAdmin 返回当前用户是否为 admin 角色（owner-or-admin 实例规则用）。
func isAdmin(c *gin.Context) bool {
	user, ok := authz.CurrentUser(c)
	return ok && user.Role == store.RoleAdmin
}

// ownerOrAdmin 实现契约 12.4 第 2 条的实例规则：created_by=user OR admin。
// created_by 为 NULL（历史 system 行）时仅 admin 可操作。
func ownerOrAdmin(c *gin.Context, createdBy *string) bool {
	if isAdmin(c) {
		return true
	}
	return createdBy != nil && *createdBy != "" && *createdBy == c.GetString("user_id")
}

// approvalRequesterID 返回审批发起人。持久化 requested_by 是职责分离与
// 执行前复检的唯一事实源（契约 12.3 第 6 条 / 12.4 第 4 条）；仅当存储
// 未装配审批仓储时回落到进程内记录。缺失审批写 404、存储故障写 503、
// 发起人为空写 403——全部 fail-closed。
func (d *Deps) approvalRequesterID(c *gin.Context, approvalID string) (string, bool) {
	if d.Approvals == nil || d.Store == nil {
		writeErrorStatus(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "approval service unavailable")
		return "", false
	}
	if apStore := d.Store.Approvals(); apStore != nil {
		ap, err := apStore.Get(c.Request.Context(), approvalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// 缺失审批 → 404（与 decide 的正常缺失路径一致），
				// 而不是把「不存在」伪装成 403。
				writeAPIError(c, http.StatusNotFound, apierr.ApprovalNotFound, "approval not found")
				return "", false
			}
			if errors.Is(err, store.ErrInvalid) {
				// 非法 UUID（真实 Postgres 的 22P02 → ErrInvalid）是客户端
				// 输入问题，不是存储故障：与 GET /api/approvals/:id 的
				// 400 一致，不能映射成 503 触发服务不可用告警
				//（第 8 轮走查 low）。
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "approval id must be a uuid")
				return "", false
			}
			// 存储故障：fail-closed，不执行命令。
			writeErrorStatus(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "approval store unavailable")
			return "", false
		}
		if ap.RequestedBy != "" {
			return ap.RequestedBy, true
		}
	}
	// 进程内记录只覆盖本进程创建的审批；存储未装配时才能用它兜底。
	uid, ok := d.Approvals.RequestedBy(approvalID)
	if !ok || uid == "" {
		writeErrorStatus(c, http.StatusForbidden, authz.CodeForbidden, "approval requester unavailable")
		return "", false
	}
	return uid, true
}

// recheckRequester 在执行审批命令前从 store 复读发起人并复检执行权限
// （契约 12.4 第 4 条 / 12.3 第 22 条）：停用、降权或查不到的发起人
// 一律拒绝执行，审批状态由决定流程自行保持/收敛。
func (d *Deps) recheckRequester(c *gin.Context, approvalID string) bool {
	uid, ok := d.approvalRequesterID(c, approvalID)
	if !ok {
		return false
	}
	// 契约 12.3 第 6 条：调用者就是发起人时是职责分离问题，必须先于
	// 「发起人执行权限」复检判定。若在这里按 exec:write 拒绝，角色已无
	// 执行权的发起人自批会收到 403 FORBIDDEN（前端统一显示「无权访问」），
	// 客户端无法区分「自审被拒」与「普通无权限」——第 6 轮走查 low。
	// 跳过复检，交由 Service.Decide 统一返回 APPROVAL_SELF_DECISION
	//（审批保持 pending）；命令不会执行，安全性不变。
	if uid == c.GetString("user_id") {
		return true
	}
	user, err := d.Store.Users().ByID(c.Request.Context(), uid)
	if err != nil || !user.Enabled || !authz.Has(user.Role, authz.PermExecuteWrite) {
		reason := "requester_unavailable"
		switch {
		case err != nil:
			reason = "requester_not_found"
		case !user.Enabled:
			reason = "requester_disabled"
		default:
			reason = "requester_permission_revoked"
		}
		// 审计必须以**调用者**（审批人）为 actor：此前复用被拒发起人的
		// 身份写 authz.denied，事后追责会指错人（评审 medium）。
		d.writeRequesterDenied(c, uid, reason)
		return false
	}
	return true
}

// writeRequesterDenied 记录「审批发起人复检失败」并写 403。
//
// 审计 actor 是当前调用者（审批人），detail 带 requested_by 与被拒原因；
// 不复用被停用发起人的身份。动作名 approval.requester_denied 与
// authz.denied 区分：前者描述审批前置条件失效，后者描述调用者无权限。
func (d *Deps) writeRequesterDenied(c *gin.Context, requestedBy, reason string) {
	callerID := c.GetString("user_id")
	if d.Audit != nil {
		ctx, cancel := context.WithTimeout(context.Background(), deniedAuditTimeout)
		defer cancel()
		_ = d.Audit.Enqueue(ctx, audit.Event{
			ActorType:    "user",
			ActorID:      callerID,
			Action:       "approval.requester_denied",
			ResourceType: "approval",
			ResourceID:   c.Param("id"),
			RequestID:    c.GetString("request_id"),
			IP:           d.requestIP(c),
			Detail: map[string]any{
				"approval_id":  c.Param("id"),
				"requested_by": requestedBy,
				"reason":       reason,
				"permission":   string(authz.PermExecuteWrite),
				"method":       c.Request.Method,
				"path":         c.FullPath(),
				"ip":           d.requestIP(c),
			},
		})
	}
	writeErrorStatus(c, http.StatusForbidden, authz.CodeForbidden,
		"approval requester is no longer allowed to execute commands")
}

// writeForbidden 供手工鉴权路径（WS 等）写统一 403 并落 authz.denied。
func (d *Deps) writeForbidden(c *gin.Context, user store.User, perm authz.Permission) {
	d.authzDenied(c, user, perm)
	writeErrorStatus(c, http.StatusForbidden, authz.CodeForbidden, "permission denied")
}

// ---------------------------------------------------------------------------
// 403 审计（authz.denied）与限速
// ---------------------------------------------------------------------------

const (
	// deniedAuditWindow 是同一 actor+permission 的最小审计间隔（契约 12.3
	// 第 20 条「按 actor+permission 限速」）。
	deniedAuditWindow = 30 * time.Second
	// deniedAuditMaxKeys 是限速表上限；超过即整体清空（防被刷时无界增长）。
	deniedAuditMaxKeys = 4096
	// deniedAuditTimeout 是审计入队上限，避免 403 洪泛拖住请求。
	deniedAuditTimeout = 500 * time.Millisecond
)

var deniedLimiter = struct {
	sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time, 64)}

// allowDeniedAudit 实现 actor+permission 的固定窗口限速；有界表。
func allowDeniedAudit(actor, perm string, now time.Time) bool {
	key := actor + "\x00" + perm
	deniedLimiter.Lock()
	defer deniedLimiter.Unlock()
	if last, ok := deniedLimiter.last[key]; ok && now.Sub(last) < deniedAuditWindow {
		return false
	}
	if len(deniedLimiter.last) >= deniedAuditMaxKeys {
		deniedLimiter.last = make(map[string]time.Time, 64)
	}
	deniedLimiter.last[key] = now
	return true
}

// authzDenied 是 RequirePermission/RequireAny 的拒绝回调：写 authz.denied。
func (d *Deps) authzDenied(c *gin.Context, user store.User, perm authz.Permission) {
	d.authzDeniedDetail(c, user, perm, nil)
}

// authzDeniedDetail 是 authzDenied 的扩展形式：额外 detail 键（仅用于
// 降级模式等已知小集合，调用方传常量键，不接受用户输入）。契约 12.3
// 第 20 条要求 detail 至少含 permission/method/path/ip，且按
// actor+permission 限速——降级门的 authz.denied 复用同一实现。
func (d *Deps) authzDeniedDetail(c *gin.Context, user store.User, perm authz.Permission, extra map[string]any) {
	if d == nil || d.Audit == nil {
		return
	}
	if !allowDeniedAudit(user.ID, string(perm), time.Now()) {
		return
	}
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}
	detail := map[string]any{
		"permission": string(perm),
		"method":     c.Request.Method,
		"path":       path,
		"ip":         d.requestIP(c),
	}
	for k, v := range extra {
		detail[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), deniedAuditTimeout)
	defer cancel()
	_ = d.Audit.Enqueue(ctx, audit.Event{
		ActorType:    "user",
		ActorID:      user.ID,
		Action:       "authz.denied",
		ResourceType: "permission",
		ResourceID:   string(perm),
		RequestID:    c.GetString("request_id"),
		IP:           d.requestIP(c),
		Detail:       detail,
	})
}
