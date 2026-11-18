// rbac_frontend_gates_test.go 锁定 RBAC 第 12 轮走查两处前端门禁所依赖的
// 服务端权限语义（与 authz_guards.go 规则表注释同源）：
//
//  1. 会话页供应商空态：运维工程师没有 provider:manage —— GET /api/providers
//     仍返回 200 最小 DTO（页面可判空），但任何写操作 403。这正是前端在
//     无权限时只显示「请联系管理员配置模型供应商」、不渲染 /providers 入口
//     的依据。
//  2. 监控空态：运维工程师没有 asset:write / monitor:manage —— POST /api/servers
//     与 PUT /api/monitor/collector 均 403，因此空态不得让他去添加资产并
//     开启采集。
//  3. 回放深链：审计员 audit:read 可以读跨用户 replay，但 /alerts、
//     /inspections、/batch、/chat 对应的服务端端点一律 403；前端据此
//     隐藏深链，仅保留 ID 文本与复制按钮。
//
// 这些断言把「前端为什么这样分支」固定在服务端事实上：若未来权限矩阵
// 调整（例如给审计员开放告警只读），本测试与前端入口会同时被提醒。
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"aiops/internal/authz"
	"aiops/internal/store"
)

func TestFrontendGatePermissionContract(t *testing.T) {
	env := newTestEnv(t)
	addTestUser(t, env, "fg-operator", testPassword, store.RoleOperator, true)
	addTestUser(t, env, "fg-auditor", testPassword, store.RoleAuditor, true)
	operatorCookie := loginAs(t, env, "fg-operator", testPassword)
	auditorCookie := loginAs(t, env, "fg-auditor", testPassword)

	// 角色权限集合是 /api/auth/me 下发的同一事实源（authz/rolePermissions）。
	if authz.Has(store.RoleOperator, authz.PermProviders) {
		t.Fatalf("operator must not hold provider:manage")
	}
	if authz.Has(store.RoleOperator, authz.PermAssetsWrite) {
		t.Fatalf("operator must not hold asset:write")
	}
	if authz.Has(store.RoleOperator, authz.PermAlertConfig) {
		t.Fatalf("operator must not hold monitor:manage")
	}
	for _, perm := range []authz.Permission{
		authz.PermMetricsRead,
		authz.PermInspection,
		authz.PermExecuteReadonly,
		authz.PermChat,
	} {
		if authz.Has(store.RoleAuditor, perm) {
			t.Fatalf("auditor must not hold %s", perm)
		}
	}
	if !authz.Has(store.RoleAuditor, authz.PermAudit) {
		t.Fatalf("auditor must hold audit:read (replay access)")
	}

	assertStatus := func(cookie, method, path, body string, want int) {
		t.Helper()
		rec := env.doAs(cookie, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
	}

	// 1) 供应商：读 200（最小 DTO）/ 写 403。
	assertStatus(operatorCookie, http.MethodGet, "/api/providers", "", http.StatusOK)
	assertStatus(operatorCookie, http.MethodPost, "/api/providers", `{}`, http.StatusForbidden)

	// 2) 监控空态的两个 CTA：添加资产、开启采集。
	assertStatus(operatorCookie, http.MethodGet, "/api/monitor/overview", "", http.StatusOK)
	assertStatus(operatorCookie, http.MethodPost, "/api/servers", `{}`, http.StatusForbidden)
	assertStatus(operatorCookie, http.MethodPut, "/api/monitor/collector",
		`{"enabled":false,"interval_seconds":60}`, http.StatusForbidden)

	// 3) 审计员的回放可读，但 execution 深链的目标端点全部 403。
	rec := env.doAs(operatorCookie, http.MethodPost, "/api/conversations", `{"title":"fg-replay"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create conversation = %d: %s", rec.Code, rec.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	assertStatus(auditorCookie, http.MethodGet, "/api/conversations/"+conv.ID+"/replay", "", http.StatusOK)
	assertStatus(auditorCookie, http.MethodGet, "/api/alert-events", "", http.StatusForbidden)
	assertStatus(auditorCookie, http.MethodGet, "/api/inspection/reports", "", http.StatusForbidden)
	assertStatus(auditorCookie, http.MethodGet, "/api/batch/jobs", "", http.StatusForbidden)
	assertStatus(auditorCookie, http.MethodPost, "/api/chat", `{}`, http.StatusForbidden)
}
