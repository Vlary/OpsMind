// rbac_matrix_test.go 是 RBAC 的**穷举**越权矩阵测试（契约 12.2/12.3/12.4）：
//
//  1. 规则表自检：routeRules 与 gin 实际注册路由双向对账——登记了但没注册
//     （dead rule）、注册了但没登记（漏挂权限）、同一 method+path 重复登记
//     （表内自相矛盾）都直接红灯；
//  2. 矩阵穷举：4 个角色 × 每条非公开路由各发一次真实 HTTP 请求，只断言
//     「403 与否」（有权限→非 403；无权限→403），不依赖业务成功路径；
//  3. fail-closed：无会话（context 无 user）→ 401；未知角色 → 403；
//  4. 职责分离：approver 决定自己 requested_by 的审批 → 拒绝且原因可读。
//
// 全部走 newTestEnv（memStore + 真实中间件链），不启动 postgres。
package httpapi

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/auth"
	"aiops/internal/authz"
	"aiops/internal/config"
	"aiops/internal/store"
	"aiops/internal/termx"
)

// rbacProbeID 是路径参数占位符：必须是合法 UUID，让 handler 走到「资源
// 不存在」的 404 而不是 400，从而把「403 与否」的断言与业务校验解耦。
const rbacProbeID = "11111111-1111-4111-8111-111111111111"

// rbacInlinePermissions 记录 guardInHandler 路由在 handler 内实际使用的
// 权限集合。routeRules 对 guardInHandler 只声明「权限意图」（Perm 是文档
// 字段，不是路由层实际守卫），因此矩阵期望必须以 handler 实现为准：
//   - conversation_handlers.go 的会话目录用 authz.AuthorizeAny(chat, audit)
//     （owner 自读或审计员跨用户只读，第 4 轮走查 medium 起与 /api/replays
//     同源）；会话详情/CRUD 仍只用单权限；
//   - 回放端点（replay_handlers.go:81）同样 AuthorizeAny(chat, audit)；
//   - 其余 handler 内联路由（会话 CRUD/chat/term/rdp WS）只用单权限。
//
// 表大小固定（3 项），不随输入增长。
var rbacInlinePermissions = map[string][]authz.Permission{
	"GET /api/conversations":            {authz.PermChat, authz.PermAudit},
	"GET /api/conversations/:id/replay": {authz.PermChat, authz.PermAudit},
	"GET /api/replays":                  {authz.PermChat, authz.PermAudit},
}

// rbacRuleKey 是 method+path 的唯一键（与 ruleIndex/appliedGuards 同构）。
func rbacRuleKey(method, path string) string { return method + " " + path }

// rbacRulePermissions 返回该规则实际要求的权限集合；guardAuthenticated /
// guardPublic 没有权限要求（返回 nil）。
func rbacRulePermissions(r routeRule) []authz.Permission {
	switch r.Kind {
	case guardPermission, guardInstancePermission, guardExternal:
		if r.Perm == "" {
			return nil
		}
		return []authz.Permission{r.Perm}
	case guardAnyPermission:
		return r.Perms
	case guardInHandler:
		if perms, ok := rbacInlinePermissions[rbacRuleKey(r.Method, r.Path)]; ok {
			return perms
		}
		if r.Perm == "" {
			return nil
		}
		return []authz.Permission{r.Perm}
	default:
		return nil
	}
}

// rbacEqualPermissions 比较两条规则实际要求的权限集合（顺序无关）。
// guardInHandler 不参与（路由层没有 guard 可对账）。
func rbacEqualPermissions(a, b routeRule) bool {
	as := make([]string, 0, 2)
	for _, p := range rbacRulePermissions(a) {
		as = append(as, string(p))
	}
	bs := make([]string, 0, 2)
	for _, p := range rbacRulePermissions(b) {
		bs = append(bs, string(p))
	}
	sort.Strings(as)
	sort.Strings(bs)
	return strings.Join(as, ",") == strings.Join(bs, ",")
}

// rbacRoleAllowed 报告角色是否被允许进入该路由的 handler。
// guardAuthenticated 对任何已认证用户放行（unknown role 也放行——权限
// 判定只在 guardPermission/guardAny 处失败关闭）。
func rbacRoleAllowed(role store.Role, r routeRule) bool {
	if r.Kind == guardAuthenticated {
		return true
	}
	for _, p := range rbacRulePermissions(r) {
		if authz.Has(role, p) {
			return true
		}
	}
	return false
}

// rbacDenyStatus 返回无权限时的期望状态码。
//
// 对 guardInstancePermission（如 GET /approvals/:id）契约要求「无权限且非
// owner 一律 404，不泄露资源是否存在」（12.3 第 3 条 / 12.4 第 2 条），
// 其余守卫一律 403 FORBIDDEN（12.4 第 1 条）。
func rbacDenyStatus(r routeRule) int {
	if r.Kind == guardInstancePermission {
		return http.StatusNotFound
	}
	return http.StatusForbidden
}

// rbacDescribeRule 生成可读的子测试名与失败信息片段。
func rbacDescribeRule(r routeRule) string {
	return r.Method + " " + r.Path
}

// rbacSortedRules 返回按 path+method 排序的规则副本（稳定输出）。
func rbacSortedRules() []routeRule {
	out := append([]routeRule(nil), routeRules...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// ---------------------------------------------------------------------------
// 1. 规则表自检
// ---------------------------------------------------------------------------

// TestRBACMatrixRuleTableMatchesRouter 双向对账 routeRules 与真实路由：
//
//   - 每条规则必须对应一条已注册的真实路由（抓「登记了但路由没注册」）；
//   - 每条 /api 路由必须恰好有一条规则（抓「路由注册了但漏挂权限」与重复登记）；
//   - 需要路由层守卫的规则必须真的装上了守卫（appliedGuards）。
func TestRBACMatrixRuleTableMatchesRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(&Deps{
		Config: &config.Config{AllowedOrigins: []string{testOrigin}},
		Clock:  time.Now,
	})

	registered := make(map[string]int, len(routeRules))
	routeList := router.Routes()
	for _, ri := range routeList {
		if !strings.HasPrefix(ri.Path, "/api/") {
			continue // /healthz 与静态资源不在 RBAC 范围。
		}
		registered[rbacRuleKey(ri.Method, ri.Path)]++
	}

	ruleCounts := make(map[string]int, len(routeRules))
	for _, r := range routeRules {
		key := rbacRuleKey(r.Method, r.Path)
		ruleCounts[key]++

		// 守卫种类必须已知，且权限载荷与种类匹配（防止空权限的假守卫）。
		switch r.Kind {
		case guardPermission, guardInstancePermission, guardExternal:
			if r.Perm == "" {
				t.Errorf("rule %s kind=%s has empty permission", key, r.Kind)
			}
		case guardAnyPermission:
			if len(r.Perms) == 0 {
				t.Errorf("rule %s kind=%s has empty permission list", key, r.Kind)
			}
		case guardAuthenticated, guardInHandler, guardPublic:
			// 无需权限载荷。
		default:
			t.Errorf("rule %s has unknown guard kind %q", key, r.Kind)
		}

		if registered[key] == 0 {
			t.Errorf("rule %s is registered in routeRules but no such route exists in gin.Routes() (规则登记了但路由未注册)", key)
		}
	}

	// 每条路由必须恰好一条规则——0 条=漏挂权限；>1 条=表内自相矛盾
	// （ruleIndex 会静默覆盖，先注册的那条永远不会生效）。
	for key := range registered {
		switch ruleCounts[key] {
		case 0:
			t.Errorf("route %s has no permission rule (路由注册了但漏挂权限)", key)
		case 1:
			// OK
		default:
			t.Errorf("route %s has %d permission rules, want exactly 1", key, ruleCounts[key])
		}
	}

	// 权限载荷对账：规则声明与真正装配到路由上的 guard 必须是同一组权限
	// （防止「表里写 asset:read、实际挂 asset:write」这类错挂）。
	// guardExternal 允许由其他文件注册，故不强制存在；guardAuthenticated
	// 目前直接在 router.go 的 protected 组注册（无 recordApplied），
	// 其认证性由 TestRBACMatrixFailClosed 的全路由未登录扫描兜底。
	applied := appliedGuardRules()
	for _, r := range routeRules {
		key := rbacRuleKey(r.Method, r.Path)
		switch r.Kind {
		case guardPublic, guardInHandler:
			continue
		}
		appliedRule, ok := applied[key]
		if !ok {
			if r.Kind == guardPermission || r.Kind == guardAnyPermission {
				t.Errorf("rule %s (kind=%s) has no applied route guard (权限中间件未装配)", key, r.Kind)
			}
			continue
		}
		if !rbacEqualPermissions(r, appliedRule) {
			t.Errorf("rule %s declares permissions %v but the applied guard enforces %v",
				key, rbacRulePermissions(r), rbacRulePermissions(appliedRule))
		}
	}

	// 装配了守卫但路由不存在（反向泄漏）也失败。
	for key := range applied {
		if registered[key] == 0 {
			t.Errorf("applied guard %s exists but gin has no such route", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 全角色 × 全路由矩阵
// ---------------------------------------------------------------------------

// newRBACMatrixEnv 在 newTestEnv 基础上补齐终端的 Term 依赖并重建路由。
//
// 原因：/api/term/sessions/:id/ws 的 handler 在权限判定**之前**检查
// `d.Term == nil`（term_handlers.go:399）；newTestEnv 未注入 Term，会让
// 无权限角色拿到 500 而不是 403，矩阵就无法验证这条路由的权限守卫。
// 其余依赖（memStore/auth/audit/approvals/streams/...）全部复用
// newTestEnv，不启动 postgres。
func newRBACMatrixEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	term := termx.NewManager(termx.Config{}, env.st, env.sshExec, env.auditW, time.Now, log)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		term.Shutdown(ctx)
	})

	env.router = NewRouter(&Deps{
		Config:    &config.Config{AllowedOrigins: []string{testOrigin}, PublicOrigin: testOrigin, WebDir: ""},
		Log:       log,
		Store:     env.st,
		Cipher:    env.cipher,
		Auth:      env.authSvc,
		Limiter:   auth.NewLoginLimiter(1000, time.Second, time.Now),
		Approvals: env.apprSvc,
		LLM:       env.llmReg,
		SSH:       env.sshExec,
		NetCLI:    env.netExec,
		DB:        env.dbExec,
		Audit:     env.auditW,
		Streams:   env.streams,
		Term:      term,
		Clock:     time.Now,
		Version:   "test",
	})
	return env
}

// rbacActor 是一个矩阵身份：角色 + 会话 + 所属终端会话（WS 归属校验需要）。
type rbacActor struct {
	name          string
	role          store.Role
	username      string
	userID        string
	cookie        string
	termSessionID string
}

// newRBACMatrixActors 建立契约 12.1 的四个角色身份。
//
// 每个角色额外创建一条属于自己的 active 终端会话：/term/sessions/:id/ws
// 在 handler 里先查会话（404）后判权限（403），没有真实会话行时无权限角色
// 会得到 404 而不是 403，无法验证权限守卫。
func newRBACMatrixActors(t *testing.T, env *testEnv) []rbacActor {
	t.Helper()
	ctx := context.Background()

	defs := []struct {
		name     string
		role     store.Role
		username string
	}{
		{"admin", store.RoleAdmin, "admin"},
		{"operator", store.RoleOperator, "rbac-op"},
		{"approver", store.RoleApprover, "rbac-approver"},
		{"auditor", store.RoleAuditor, "rbac-auditor"},
	}

	actors := make([]rbacActor, 0, len(defs))
	for _, d := range defs {
		var user store.User
		var cookie string
		if d.role == store.RoleAdmin {
			u, err := env.st.Users().ByUsername(ctx, d.username)
			if err != nil {
				t.Fatalf("load admin user: %v", err)
			}
			user, cookie = u, env.cookie
		} else {
			user = addTestUser(t, env, d.username, testPassword, d.role, true)
			cookie = loginAs(t, env, d.username, testPassword)
		}

		sess, err := env.st.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
			ServerID:  rbacProbeID,
			UserID:    user.ID,
			Cols:      120,
			Rows:      40,
			StartedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create term session for %s: %v", d.name, err)
		}

		actors = append(actors, rbacActor{
			name:          d.name,
			role:          d.role,
			username:      d.username,
			userID:        user.ID,
			cookie:        cookie,
			termSessionID: sess.ID,
		})
	}
	return actors
}

// rbacProbePath 把路由 pattern 物化为可请求的具体路径。
func rbacProbePath(r routeRule, actor rbacActor) string {
	if r.Path == "/api/term/sessions/:id/ws" {
		id := actor.termSessionID
		if id == "" {
			id = rbacProbeID
		}
		return "/api/term/sessions/" + id + "/ws"
	}
	return strings.NewReplacer(":cid", rbacProbeID, ":id", rbacProbeID).Replace(r.Path)
}

// TestRBACMatrixAllRolesAllRoutes 是核心穷举矩阵：
//
//	admin/operator/approver/auditor × 每条非公开路由 → 一次真实请求
//
// 有权限：断言不是 401/403（可能是 200/400/404/422/500 等业务结果，
// 不断言具体业务成功，避免业务逻辑变化导致矩阵脆弱）；
// 无权限：断言精确的拒绝状态（403，实例规则 404）且错误体可读。
func TestRBACMatrixAllRolesAllRoutes(t *testing.T) {
	env := newRBACMatrixEnv(t)
	actors := newRBACMatrixActors(t, env)
	rules := rbacSortedRules()

	// 统计实际执行的断言数，便于在输出中确认矩阵规模。
	assertions := 0
	for _, r := range rules {
		if r.Kind == guardPublic {
			// 公开端点（登录/邀请接受）不参与角色矩阵：它们没有已认证身份，
			// 由 Origin+限流+一次性 token 保护（契约 12.1/12.3），
			// 在 fail-closed 之外单独说明。
			continue
		}

		for _, actor := range actors {
			actor := actor
			r := r
			name := fmt.Sprintf("%s/%s", actor.name, rbacDescribeRule(r))
			t.Run(name, func(t *testing.T) {
				path := rbacProbePath(r, actor)
				cookie := actor.cookie

				// logout 会吊销当前会话：用一次性新会话发这一条，避免
				// 影响同一角色后续路由。
				if r.Method == http.MethodPost && r.Path == "/api/auth/logout" {
					cookie = loginAs(t, env, actor.username, testPassword)
				}

				rec := env.doAs(cookie, r.Method, path, "")
				assertions++

				if rbacRoleAllowed(actor.role, r) {
					if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
						t.Fatalf("role=%s method=%s path=%s expected=allowed (non-401/403) actual=%d body=%s",
							actor.name, r.Method, path, rec.Code, rec.Body.String())
					}
					return
				}

				want := rbacDenyStatus(r)
				if rec.Code != want {
					t.Fatalf("role=%s method=%s path=%s expected=%d (%s) actual=%d body=%s",
						actor.name, r.Method, path, want, rbacDenyStatusText(r), rec.Code, rec.Body.String())
				}
				code, msg := errorBody(t, rec)
				if want == http.StatusForbidden {
					if code != authz.CodeForbidden {
						t.Fatalf("role=%s method=%s path=%s expected=403 %s actual code=%q body=%s",
							actor.name, r.Method, path, authz.CodeForbidden, code, rec.Body.String())
					}
				} else if want == http.StatusNotFound && code == "" {
					t.Fatalf("role=%s method=%s path=%s expected=404 with error body actual body=%s",
						actor.name, r.Method, path, rec.Body.String())
				}
				if strings.TrimSpace(msg) == "" {
					t.Fatalf("role=%s method=%s path=%s expected=readable denial message actual body=%s",
						actor.name, r.Method, path, rec.Body.String())
				}
			})
		}
	}

	// 矩阵规模自检：4 角色 × 非公开路由数。防止 routeRules 被意外清空时
	// 测试「零断言通过」。用 -run 只跑单个子测试时（模式里带 "/"）跳过，
	// 否则调试单条路由必然被这条规模断言误伤。
	protected := 0
	for _, r := range rules {
		if r.Kind != guardPublic {
			protected++
		}
	}
	subtestFiltered := false
	if f := flag.Lookup("test.run"); f != nil {
		subtestFiltered = strings.Contains(f.Value.String(), "/")
	}
	if want := protected * len(actors); !subtestFiltered && assertions != want {
		t.Fatalf("matrix assertions = %d, want %d (%d routes × %d roles)", assertions, want, protected, len(actors))
	}
	t.Logf("RBAC matrix: %d protected routes × %d roles = %d cases", protected, len(actors), protected*len(actors))
}

func rbacDenyStatusText(r routeRule) string {
	if r.Kind == guardInstancePermission {
		return "instance rule: 无权限一律 404，不泄露存在性"
	}
	return "permission denied"
}

// ---------------------------------------------------------------------------
// 3. fail-closed
// ---------------------------------------------------------------------------

// TestRBACMatrixFailClosed 断言两处失败关闭：
//   - 没有已认证用户（context 无 user / 无会话）→ 401，绝不进入 handler；
//   - 未知角色（不在四类映射内）→ 403，绝不被当成有权限。
func TestRBACMatrixFailClosed(t *testing.T) {
	env := newTestEnv(t)

	t.Run("no session cookie returns 401", func(t *testing.T) {
		rec := env.doAs("", http.MethodGet, "/api/servers", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("role=<none> method=GET path=/api/servers expected=401 actual=%d body=%s",
				rec.Code, rec.Body.String())
		}
		if code, _ := errorBody(t, rec); code != apierr.AuthRequired {
			t.Fatalf("401 code = %q, want %q", code, apierr.AuthRequired)
		}
	})

	t.Run("guard without user in context returns 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/servers", nil)
		// 直接执行权限守卫：context 里没有 requireAuth 注入的 user。
		authz.RequirePermission(authz.PermAssetsRead)(c)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("role=<anonymous> method=GET path=/api/servers expected=401 actual=%d body=%s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown role denied with 403", func(t *testing.T) {
		ghostRole := store.Role("rbac-ghost-role")
		if store.Role(ghostRole).Valid() {
			t.Fatalf("ghost role must be invalid")
		}
		// 未知角色在权限映射里必须零权限（失败关闭）。
		for _, p := range authz.AllPermissions {
			if authz.Has(ghostRole, p) {
				t.Fatalf("unknown role %q unexpectedly has permission %q", ghostRole, p)
			}
		}

		addTestUser(t, env, "rbac-ghost", testPassword, ghostRole, true)
		cookie := loginAs(t, env, "rbac-ghost", testPassword)
		rec := env.doAs(cookie, http.MethodGet, "/api/servers", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role=%s method=GET path=/api/servers expected=403 actual=%d body=%s",
				ghostRole, rec.Code, rec.Body.String())
		}
		if code, msg := errorBody(t, rec); code != authz.CodeForbidden || strings.TrimSpace(msg) == "" {
			t.Fatalf("role=%s expected 403 %s with message actual code=%q msg=%q",
				ghostRole, authz.CodeForbidden, code, msg)
		}
	})
}

// TestRBACMatrixUnauthenticatedSweep 对每条非公开路由发一次「无会话」请求，
// 断言全部 401 AUTH_REQUIRED。补足单点用例：即使某条路由被注册到
// requireAuth 组之外（或 handler 忘了自己鉴权），这里也会变红——这正是
// 「context 里没有 user 必须 401」在路由规模上的穷举版本。
func TestRBACMatrixUnauthenticatedSweep(t *testing.T) {
	env := newRBACMatrixEnv(t)
	checked := 0
	for _, r := range rbacSortedRules() {
		if r.Kind == guardPublic {
			continue // 登录/邀请接受：公开端点，设计上不要求登录。
		}
		path := rbacProbePath(r, rbacActor{})
		rec := env.doAs("", r.Method, path, "")
		checked++
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("role=<anonymous> method=%s path=%s expected=401 actual=%d body=%s",
				r.Method, path, rec.Code, rec.Body.String())
			continue
		}
		if code, _ := errorBody(t, rec); code != apierr.AuthRequired {
			t.Errorf("role=<anonymous> method=%s path=%s expected code %s actual=%q",
				r.Method, path, apierr.AuthRequired, code)
		}
	}
	if checked == 0 {
		t.Fatal("unauthenticated sweep checked 0 routes")
	}
	t.Logf("unauthenticated sweep: %d routes", checked)
}

// ---------------------------------------------------------------------------
// 4. 职责分离（SoD）
// ---------------------------------------------------------------------------

// TestRBACMatrixSeparationOfDuties 断言 approver 不能决定自己 requested_by
// 的审批（契约 12.3 第 6 条 / 12.4 第 4 条）：
//   - deny 走到 Service.Decide 的自审检查 → 403 APPROVAL_SELF_DECISION；
//   - approve 也必须返回 APPROVAL_SELF_DECISION：SoD 先于 handler 的发起人
//     执行权限复检判定。此前 approver 无 exec:write，复检先行把自审短路成
//     FORBIDDEN（第 6 轮走查 low——直连 API 的客户端无法区分自审与普通
//     无权限）；
//   - 两次拒绝后审批必须保持 pending（绝不落 decided_by）。
func TestRBACMatrixSeparationOfDuties(t *testing.T) {
	env := newTestEnv(t)
	requester := addTestUser(t, env, "rbac-sod-requester", testPassword, store.RoleApprover, true)
	cookie := loginAs(t, env, "rbac-sod-requester", testPassword)

	ctx := context.Background()
	ap, err := env.apprSvc.Create(ctx, approvals.CreateRequest{
		ConversationID: testConvPtr("conv-rbac-sod"),
		ToolCallID:     testConvPtr("tc-rbac-sod"),
		Command:        "systemctl restart nginx",
		RiskLevel:      "needs_approval",
		RuleID:         "SERVICE_CHANGE",
		Reason:         "rbac matrix SoD",
		RequestedBy:    requester.ID,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	t.Run("approver deny own approval returns 403 APPROVAL_SELF_DECISION", func(t *testing.T) {
		rec := env.doAs(cookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"deny"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role=approver method=POST path=/api/approvals/%s/decide expected=403 actual=%d body=%s",
				ap.ID, rec.Code, rec.Body.String())
		}
		code, msg := errorBody(t, rec)
		if code != "APPROVAL_SELF_DECISION" {
			t.Fatalf("role=approver expected code APPROVAL_SELF_DECISION actual=%q body=%s", code, rec.Body.String())
		}
		if !strings.Contains(msg, "requester cannot decide own approval") {
			t.Fatalf("role=approver expected readable SoD reason actual msg=%q", msg)
		}
	})

	t.Run("approver approve own approval is rejected before execution", func(t *testing.T) {
		rec := env.doAs(cookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("role=approver method=POST path=/api/approvals/%s/decide expected=denied (not 200) actual=200 body=%s",
				ap.ID, rec.Body.String())
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role=approver method=POST path=/api/approvals/%s/decide expected=403 actual=%d body=%s",
				ap.ID, rec.Code, rec.Body.String())
		}
		code, msg := errorBody(t, rec)
		if strings.TrimSpace(code) == "" || strings.TrimSpace(msg) == "" {
			t.Fatalf("role=approver expected readable denial, actual code=%q msg=%q", code, msg)
		}
		// SoD 必须先于发起人执行权限复检：approver 无 exec:write，若复检
		// 先行会返回 FORBIDDEN，直连 API 的客户端无法区分自审与普通无权限。
		t.Logf("approve-self denial code=%s message=%q", code, msg)
		if code != "APPROVAL_SELF_DECISION" {
			t.Fatalf("role=approver expected APPROVAL_SELF_DECISION actual=%q body=%s", code, rec.Body.String())
		}
	})

	// 第 6 轮走查 low 的原始场景：source=console 的审批由 approver 发起，
	// 该角色已无 exec:write；直连 API 批准必须得到 APPROVAL_SELF_DECISION
	//（而不是发起人权限复检的 FORBIDDEN），且审批保持 pending。
	t.Run("console approval requested by exec-denied approver returns APPROVAL_SELF_DECISION", func(t *testing.T) {
		consoleAp, err := env.apprSvc.Create(ctx, approvals.CreateRequest{
			Source:      "console",
			Command:     "docker restart r6",
			RiskLevel:   "needs_approval",
			RuleID:      "CONTAINER_CHANGE",
			Reason:      "round 6 self decision",
			RequestedBy: requester.ID,
		})
		if err != nil {
			t.Fatalf("create console approval: %v", err)
		}
		rec := env.doAs(cookie, http.MethodPost, "/api/approvals/"+consoleAp.ID+"/decide", `{"decision":"approve"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("console self approve = %d %s", rec.Code, rec.Body.String())
		}
		code, _ := errorBody(t, rec)
		if code != "APPROVAL_SELF_DECISION" {
			t.Fatalf("console self approve code = %q, want APPROVAL_SELF_DECISION body=%s", code, rec.Body.String())
		}
		fresh, err := env.apprSvc.Get(ctx, consoleAp.ID)
		if err != nil {
			t.Fatalf("reload console approval: %v", err)
		}
		if fresh.Status != store.ApprovalPending || fresh.DecidedBy != nil {
			t.Fatalf("console approval = %+v, want pending without decided_by", fresh)
		}
	})

	fresh, err := env.apprSvc.Get(ctx, ap.ID)
	if err != nil {
		t.Fatalf("reload approval: %v", err)
	}
	if fresh.Status != store.ApprovalPending {
		t.Fatalf("approval status after self-decision attempts = %q, want pending", fresh.Status)
	}
	if fresh.DecidedBy != nil {
		t.Fatalf("approval decided_by after self-decision attempts = %q, want nil", *fresh.DecidedBy)
	}
}
